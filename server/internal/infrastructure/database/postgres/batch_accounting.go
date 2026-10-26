package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Accounting: every entry change moves the job's counters, the tenant's
// usage and the dataset's bytes together. Rows are locked in one order:
// entry, tenant usage, dataset, job. Counter decrements floor at zero, so a
// drifted counter cannot fail a transition; reconciliation repairs the
// drift.
//
// The global upload slots are not a counter: a claim counts the entries in
// flight (a partial index keeps that cheap), under the one lock all claims
// share. Only claims add slots, so finishing, expiring or cancelling an
// upload never waits on another tenant.

var stateColumn = map[model.EntryState]string{
	model.EntryInvalid:    "invalid_entries",
	model.EntryPending:    "pending_entries",
	model.EntryScheduled:  "scheduled_entries",
	model.EntryUploading:  "uploading_entries",
	model.EntryUploaded:   "uploaded_entries",
	model.EntryProcessing: "processing_entries",
	model.EntryPublished:  "published_entries",
	model.EntryRetryable:  "retryable_entries",
	model.EntryFailed:     "failed_entries",
	model.EntryCancelled:  "cancelled_entries",
}

// delta is what a set of entry changes does to the counters.
type delta struct {
	counts         map[model.EntryState]int64
	requiredFailed int64
	slots          int
	multipart      int
	// reserved and stored are bytes, for the tenant and the dataset.
	reserved int64
	stored   int64
	// open and awaiting are the tenant's pending work and processing
	// backlog.
	open     int64
	awaiting int64
}

func failedFinal(s model.EntryState) bool {
	return s == model.EntryInvalid || s == model.EntryFailed || s == model.EntryCancelled
}

func sign(on bool) int64 {
	if on {
		return 1
	}
	return -1
}

// move adds the change of e to state `to`. storedBytes is the verified size
// of a newly published entry.
func (d *delta) move(e model.IngestionEntry, to model.EntryState, storedBytes int64) {
	from := e.State
	if d.counts == nil {
		d.counts = map[model.EntryState]int64{}
	}
	if from != to {
		d.counts[from]--
		d.counts[to]++
	}
	if from.HoldsSlot() != to.HoldsSlot() {
		n := int(sign(to.HoldsSlot()))
		d.slots += n
		if e.Strategy == model.StrategyMultipart {
			d.multipart += n
		}
	}
	if from.HoldsReservation() && !to.HoldsReservation() {
		d.reserved -= e.ReservedBytes
	}
	if to == model.EntryPublished && from != model.EntryPublished {
		d.stored += storedBytes
	}
	if from.Open() != to.Open() {
		d.open += sign(to.Open())
	}
	if from.AwaitingProcessing() != to.AwaitingProcessing() {
		d.awaiting += sign(to.AwaitingProcessing())
	}
	if e.Required && failedFinal(from) != failedFinal(to) {
		d.requiredFailed += sign(failedFinal(to))
	}
}

// closing reports whether d closes an entry: only then can its job finish.
func (d delta) closing() bool { return d.open < 0 }

// applyDelta writes d in lock order and returns the job as it now is. The
// tenant usage and the dataset are hot rows shared by a tenant's jobs, so a
// change that leaves them as they are does not lock them. A closing change
// always does: finishing the job updates both, and they must be locked
// before the job.
func applyDelta(ctx context.Context, tx pgx.Tx, tenantID, datasetID, jobID string, d delta, now time.Time) (model.IngestionJob, error) {
	// A closing change always changes the usage: one less open entry.
	if d.reserved != 0 || d.stored != 0 || d.slots != 0 || d.multipart != 0 || d.open != 0 || d.awaiting != 0 {
		if err := updateUsage(ctx, tx, tenantID, d, now); err != nil {
			return model.IngestionJob{}, err
		}
	}
	if d.reserved != 0 || d.stored != 0 || d.closing() {
		if _, err := tx.Exec(ctx, `
			UPDATE datasets SET reserved_bytes = GREATEST(reserved_bytes + $2, 0), stored_bytes = GREATEST(stored_bytes + $3, 0),
			       updated_at = $4
			WHERE id = $1
		`, datasetID, d.reserved, d.stored, now); err != nil {
			return model.IngestionJob{}, fmt.Errorf("update dataset bytes: %w", err)
		}
	}

	sets := "required_failed = GREATEST(required_failed + $2, 0), stored_bytes = stored_bytes + $3, updated_at = $4"
	args := []any{jobID, d.requiredFailed, d.stored, now}
	states := make([]model.EntryState, 0, len(d.counts))
	for s := range d.counts {
		states = append(states, s)
	}
	slices.Sort(states)
	for _, s := range states {
		if d.counts[s] == 0 {
			continue
		}
		col := stateColumn[s]
		args = append(args, d.counts[s])
		sets += fmt.Sprintf(", %s = GREATEST(%s + $%d, 0)", col, col, len(args))
	}
	job, err := scanIngestionJob(tx.QueryRow(ctx, `UPDATE ingestion_jobs SET `+sets+` WHERE id = $1 RETURNING `+ingestionJobColumns, args...))
	if err != nil {
		return model.IngestionJob{}, fmt.Errorf("update job counters: %w", err)
	}
	return job, nil
}

func updateUsage(ctx context.Context, tx pgx.Tx, tenantID string, d delta, now time.Time) error {
	if _, err := tx.Exec(ctx, `
		UPDATE ingestion_tenant_usage
		SET reserved_bytes = GREATEST(reserved_bytes + $2, 0), stored_bytes = GREATEST(stored_bytes + $3, 0),
		    active_uploads = GREATEST(active_uploads + $4, 0), active_multipart = GREATEST(active_multipart + $5, 0),
		    pending_entries = GREATEST(pending_entries + $6, 0), awaiting_processing = GREATEST(awaiting_processing + $7, 0),
		    updated_at = $8
		WHERE tenant_id = $1
	`, tenantID, d.reserved, d.stored, d.slots, d.multipart, d.open, d.awaiting, now); err != nil {
		return fmt.Errorf("update tenant usage: %w", err)
	}
	return nil
}

// settle ends a job once none of its entries has work left. The caller
// holds the tenant usage and dataset rows.
func settle(ctx context.Context, tx pgx.Tx, job model.IngestionJob, now time.Time) (model.IngestionJob, error) {
	c := job.Counters
	if c.Open() > 0 {
		return job, nil
	}
	next := job.State
	switch job.State {
	case model.JobCancelling:
		next = model.JobCancelled
		if job.Error != "" {
			next = model.JobFailed // its manifest was rejected
		}
	case model.JobReady, model.JobRunning:
		if job.FinalizedAt == nil {
			return job, nil
		}
		switch {
		case c.Published == c.Valid() && c.Invalid == 0:
			next = model.JobCompleted
		case c.Published > 0:
			next = model.JobPartiallyCompleted
		default:
			next = model.JobFailed
		}
	}
	if next == job.State {
		return job, nil
	}
	return finish(ctx, tx, job, next, "", now)
}

// finish moves a job to a terminal state, releases its active-job slot and
// updates its dataset's status.
func finish(ctx context.Context, tx pgx.Tx, job model.IngestionJob, state model.JobState, reason string, now time.Time) (model.IngestionJob, error) {
	done, err := scanIngestionJob(tx.QueryRow(ctx, `
		UPDATE ingestion_jobs
		SET state = $2, error = COALESCE(NULLIF($3, ''), error), completed_at = $4,
		    lease_owner = NULL, lease_expires_at = NULL, updated_at = $4
		WHERE id = $1
		RETURNING `+ingestionJobColumns, job.ID, string(state), clipText(reason), now))
	if err != nil {
		return model.IngestionJob{}, fmt.Errorf("finish ingestion job: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE ingestion_tenant_usage SET active_jobs = GREATEST(active_jobs - 1, 0), updated_at = $2 WHERE tenant_id = $1
	`, job.TenantID, now); err != nil {
		return model.IngestionJob{}, fmt.Errorf("release active job: %w", err)
	}
	return done, refreshDataset(ctx, tx, job.DatasetID, now)
}

// refreshDataset sets a dataset's status from its jobs' counters.
func refreshDataset(ctx context.Context, tx pgx.Tx, datasetID string, now time.Time) error {
	var policy string
	var s model.DatasetSummary
	err := tx.QueryRow(ctx, `
		SELECT d.completion_policy, count(j.id),
		       count(j.id) FILTER (WHERE j.state NOT IN ('PARTIALLY_COMPLETED', 'COMPLETED', 'FAILED', 'CANCELLED')),
		       COALESCE(sum(j.total_entries - j.invalid_entries), 0), COALESCE(sum(j.published_entries), 0),
		       COALESCE(sum(j.invalid_entries), 0), COALESCE(sum(j.failed_entries), 0),
		       COALESCE(sum(j.cancelled_entries), 0), COALESCE(sum(j.required_failed), 0)
		FROM datasets AS d LEFT JOIN ingestion_jobs AS j ON j.dataset_id = d.id
		WHERE d.id = $1
		GROUP BY d.completion_policy
	`, datasetID).Scan(&policy, &s.Jobs, &s.ActiveJobs, &s.Valid, &s.Published, &s.Invalid, &s.Failed, &s.Cancelled, &s.RequiredFailed)
	if err != nil {
		return fmt.Errorf("summarize dataset: %w", err)
	}
	status := model.DatasetStatusOf(model.CompletionPolicy(policy), s)
	_, err = tx.Exec(ctx, `
		UPDATE datasets
		SET status = $2,
		    ready_at = CASE WHEN $2 IN ('READY', 'PARTIALLY_READY') THEN COALESCE(ready_at, $3) ELSE NULL END,
		    updated_at = $3
		WHERE id = $1
	`, datasetID, string(status), now)
	if err != nil {
		return fmt.Errorf("update dataset status: %w", err)
	}
	return nil
}

// lockUsageRow creates the tenant's usage row if needed, locks it and
// returns it.
func lockUsageRow(ctx context.Context, tx pgx.Tx, tenantID string, now time.Time) (model.TenantUsage, error) {
	var u model.TenantUsage
	err := tx.QueryRow(ctx, `
		INSERT INTO ingestion_tenant_usage (tenant_id, updated_at) VALUES ($1, $2)
		ON CONFLICT (tenant_id) DO UPDATE SET updated_at = ingestion_tenant_usage.updated_at
		RETURNING tenant_id, reserved_bytes, stored_bytes, active_jobs, active_uploads, active_multipart,
		          pending_entries, awaiting_processing
	`, tenantID, now).Scan(&u.TenantID, &u.ReservedBytes, &u.StoredBytes, &u.ActiveJobs, &u.ActiveUploads,
		&u.ActiveMultipart, &u.PendingEntries, &u.AwaitingProcessing)
	if err != nil {
		return model.TenantUsage{}, fmt.Errorf("lock tenant usage: %w", err)
	}
	return u, nil
}

func lockUsage(ctx context.Context, tx pgx.Tx, tenantID string, now time.Time) error {
	_, err := lockUsageRow(ctx, tx, tenantID, now)
	return err
}

func lockDataset(ctx context.Context, tx pgx.Tx, tenantID, datasetID string) (model.Dataset, error) {
	d, err := scanDataset(tx.QueryRow(ctx,
		`SELECT `+datasetColumns+` FROM datasets WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenantID, datasetID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Dataset{}, model.ErrDatasetNotFound
	}
	if err != nil {
		return model.Dataset{}, fmt.Errorf("lock dataset: %w", err)
	}
	return d, nil
}

func ceilDiv(a, b int) int {
	if b <= 0 {
		return a
	}
	return (a + b - 1) / b
}

// Claim grants upload slots. The grant is the smallest of: what was asked,
// the free global slots, the tenant's free slots and the job's free slots.
// Under contention a tenant's slots shrink to a fair share of the global
// slots (divided among tenants with pending entries), and a job's to a
// fair share of its tenant's (divided among its jobs with pending
// entries), so one large tenant or job cannot take everything. Nothing is
// granted while the tenant's processing backlog is full. Entries are then
// taken in position order; one that would pass the storage quota, or open
// one multipart session too many, is left for a later claim.
func (r *IngestionRepository) Claim(ctx context.Context, req interfaces.ClaimRequest) (interfaces.ClaimResult, error) {
	var result interfaces.ClaimResult
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		job, err := scanIngestionJob(tx.QueryRow(ctx,
			`SELECT `+ingestionJobColumns+` FROM ingestion_jobs WHERE tenant_id = $1 AND id = $2`, req.TenantID, req.JobID))
		if errors.Is(err, pgx.ErrNoRows) {
			return model.ErrIngestionNotFound
		}
		if err != nil {
			return fmt.Errorf("read ingestion job: %w", err)
		}
		if job.State != model.JobReady && job.State != model.JobRunning {
			result.LimitedBy = interfaces.LimitJobNotRunning
			return nil
		}

		// Lock order: the claim lock, tenant usage, dataset, job. Claims
		// run one at a time, so each sees every earlier claim's grants.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM ingestion_capacity WHERE name = 'upload_slots' FOR UPDATE`); err != nil {
			return fmt.Errorf("lock upload slots: %w", err)
		}
		globalInUse, err := countSlots(ctx, tx)
		if err != nil {
			return err
		}
		usage, err := lockUsageRow(ctx, tx, req.TenantID, req.Now)
		if err != nil {
			return err
		}
		dataset, err := lockDataset(ctx, tx, req.TenantID, job.DatasetID)
		if err != nil {
			return err
		}
		job, err = scanIngestionJob(tx.QueryRow(ctx, `SELECT `+ingestionJobColumns+` FROM ingestion_jobs WHERE id = $1 FOR UPDATE`, req.JobID))
		if err != nil {
			return fmt.Errorf("lock ingestion job: %w", err)
		}
		if job.State != model.JobReady && job.State != model.JobRunning {
			result.LimitedBy = interfaces.LimitJobNotRunning
			return nil
		}
		if job.Counters.Pending == 0 {
			result.LimitedBy = interfaces.LimitNothingToDo
			return nil
		}
		if usage.AwaitingProcessing >= req.ProcessingBacklog {
			result.LimitedBy = interfaces.LimitBacklog
			return nil
		}

		var tenants, jobs int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ingestion_tenant_usage WHERE pending_entries > 0`).Scan(&tenants); err != nil {
			return fmt.Errorf("count tenants with work: %w", err)
		}
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM ingestion_jobs
			WHERE tenant_id = $1 AND state IN ('READY', 'RUNNING') AND (pending_entries > 0 OR scheduled_entries + uploading_entries > 0)
		`, req.TenantID).Scan(&jobs); err != nil {
			return fmt.Errorf("count jobs with work: %w", err)
		}
		tenantCap := min(req.TenantSlots, ceilDiv(req.GlobalSlots, max(tenants, 1)))
		jobCap := min(req.JobSlots, ceilDiv(tenantCap, max(jobs, 1)))
		if job.MaxConcurrency > 0 {
			jobCap = min(jobCap, job.MaxConcurrency)
		}

		want := req.Max
		limits := []struct {
			free int
			name string
		}{
			{req.GlobalSlots - globalInUse, interfaces.LimitGlobalSlots},
			{tenantCap - usage.ActiveUploads, interfaces.LimitTenantSlots},
			{jobCap - int(job.Counters.Scheduled+job.Counters.Uploading), interfaces.LimitJobSlots},
		}
		for _, l := range limits {
			if l.free < want {
				want, result.LimitedBy = max(l.free, 0), l.name
			}
		}
		if want == 0 {
			return nil
		}

		candidates, err := collectEntries(tx.Query(ctx, `
			SELECT `+entryColumns+` FROM ingestion_entries
			WHERE job_id = $1 AND state = 'PENDING'
			ORDER BY position
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		`, req.JobID, want))
		if err != nil {
			return err
		}

		multipartFree := req.TenantMultipart - usage.ActiveMultipart
		bytesFree := req.MaxStorageBytes - usage.ReservedBytes - usage.StoredBytes
		datasetFree := int64(-1) // no cap of its own
		if dataset.MaxBytes > 0 {
			datasetFree = dataset.MaxBytes - dataset.ReservedBytes - dataset.StoredBytes
		}
		var granted []int64
		var d delta
		for _, e := range candidates {
			if e.Strategy == model.StrategyMultipart && multipartFree <= 0 {
				result.LimitedBy = interfaces.LimitMultipart
				continue
			}
			if e.SizeBytes > bytesFree || (datasetFree >= 0 && e.SizeBytes > datasetFree) {
				result.LimitedBy = interfaces.LimitQuota
				continue
			}
			if e.Strategy == model.StrategyMultipart {
				multipartFree--
			}
			bytesFree -= e.SizeBytes
			if datasetFree >= 0 {
				datasetFree -= e.SizeBytes
			}
			granted = append(granted, e.Position)
			d.move(e, model.EntryScheduled, 0)
			d.reserved += e.SizeBytes
		}
		if len(granted) == 0 {
			return nil
		}

		result.Entries, err = collectEntries(tx.Query(ctx, `
			UPDATE ingestion_entries
			SET state = 'SCHEDULED', lease_expires_at = $3, reserved_bytes = size_bytes,
			    next_attempt_at = NULL, error_code = NULL, error_detail = NULL, updated_at = $4
			WHERE job_id = $1 AND position = ANY($2)
			RETURNING `+entryColumns, req.JobID, granted, req.Now.Add(req.Lease), req.Now))
		if err != nil {
			return err
		}
		slices.SortFunc(result.Entries, func(a, b model.IngestionEntry) int { return int(a.Position - b.Position) })
		if _, err := applyDelta(ctx, tx, req.TenantID, job.DatasetID, req.JobID, d, req.Now); err != nil {
			return err
		}
		if job.State == model.JobReady {
			if _, err := tx.Exec(ctx, `UPDATE ingestion_jobs SET state = 'RUNNING', updated_at = $2 WHERE id = $1`, req.JobID, req.Now); err != nil {
				return fmt.Errorf("start ingestion job: %w", err)
			}
		}
		if len(result.Entries) == req.Max {
			result.LimitedBy = ""
		}
		return nil
	})
	return result, err
}

func collectEntries(rows pgx.Rows, err error) ([]model.IngestionEntry, error) {
	if err != nil {
		return nil, fmt.Errorf("query ingestion entries: %w", err)
	}
	return collect(rows, scanEntry)
}

func (r *IngestionRepository) Transition(ctx context.Context, ch interfaces.EntryChange) (model.IngestionEntry, error) {
	var out model.IngestionEntry
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		before, err := scanEntry(tx.QueryRow(ctx,
			`SELECT `+entryColumns+` FROM ingestion_entries WHERE job_id = $1 AND position = $2 FOR UPDATE`,
			ch.JobID, ch.Position))
		if errors.Is(err, pgx.ErrNoRows) {
			return model.ErrEntryNotFound
		}
		if err != nil {
			return fmt.Errorf("lock ingestion entry: %w", err)
		}
		out = before
		if !slices.Contains(ch.From, before.State) || (ch.UploadID != "" && before.UploadID != ch.UploadID) {
			return model.ErrInvalidEntryState
		}

		var d delta
		d.move(before, ch.To, ch.StoredBytes)
		job, err := applyDelta(ctx, tx, before.TenantID, before.DatasetID, before.JobID, d, ch.Now)
		if err != nil {
			return err
		}
		attempt, generation := 0, 0
		if ch.CountAttempt {
			attempt = 1
		}
		if ch.NewGeneration {
			generation = 1
		}
		out, err = scanEntry(tx.QueryRow(ctx, `
			UPDATE ingestion_entries SET
			    state = $3,
			    attempts = attempts + $4,
			    asset_id = COALESCE(NULLIF($5, ''), asset_id),
			    stored_bytes = CASE WHEN $3 = 'PUBLISHED' THEN $6 ELSE stored_bytes END,
			    reserved_bytes = CASE WHEN $3 IN ('SCHEDULED', 'UPLOADING', 'UPLOADED', 'PROCESSING') THEN reserved_bytes ELSE 0 END,
			    error_code = NULLIF($7, ''),
			    error_detail = NULLIF($8, ''),
			    next_attempt_at = $9,
			    lease_expires_at = CASE WHEN $3 IN ('SCHEDULED', 'UPLOADING') THEN COALESCE($10, lease_expires_at) ELSE NULL END,
			    upload_generation = upload_generation + $11,
			    upload_id = CASE WHEN $11 > 0 THEN NULL ELSE upload_id END,
			    updated_at = $12
			WHERE job_id = $1 AND position = $2
			RETURNING `+entryColumns,
			ch.JobID, ch.Position, string(ch.To), attempt, ch.AssetID, ch.StoredBytes,
			ch.ErrorCode, clipText(ch.ErrorDetail), ch.NextAttemptAt, ch.LeaseExpiresAt, generation, ch.Now))
		if err != nil {
			return fmt.Errorf("move ingestion entry: %w", err)
		}
		if !d.closing() {
			return nil // no entry closed: the job cannot have finished
		}
		_, err = settle(ctx, tx, job, ch.Now)
		return err
	})
	return out, err
}

func (r *IngestionRepository) RequestCancel(ctx context.Context, tenantID, jobID string, now time.Time) (model.IngestionJob, error) {
	var job model.IngestionJob
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		job, err = scanIngestionJob(tx.QueryRow(ctx,
			`SELECT `+ingestionJobColumns+` FROM ingestion_jobs WHERE tenant_id = $1 AND id = $2`, tenantID, jobID))
		if errors.Is(err, pgx.ErrNoRows) {
			return model.ErrIngestionNotFound
		}
		if err != nil {
			return fmt.Errorf("read ingestion job: %w", err)
		}
		if err := lockUsage(ctx, tx, tenantID, now); err != nil {
			return err
		}
		if _, err := lockDataset(ctx, tx, tenantID, job.DatasetID); err != nil {
			return err
		}
		job, err = scanIngestionJob(tx.QueryRow(ctx, `SELECT `+ingestionJobColumns+` FROM ingestion_jobs WHERE id = $1 FOR UPDATE`, jobID))
		if err != nil {
			return fmt.Errorf("lock ingestion job: %w", err)
		}
		switch job.State {
		case model.JobCancelling:
			return nil
		case model.JobPendingValidation, model.JobValidating, model.JobReady, model.JobRunning:
		default:
			return model.ErrInvalidJobState
		}
		job, err = scanIngestionJob(tx.QueryRow(ctx, `
			UPDATE ingestion_jobs SET state = 'CANCELLING', cancelled_at = $2, updated_at = $2
			WHERE id = $1 RETURNING `+ingestionJobColumns, jobID, now))
		if err != nil {
			return fmt.Errorf("cancel ingestion job: %w", err)
		}
		job, err = settle(ctx, tx, job, now)
		return err
	})
	return job, err
}

func (r *IngestionRepository) CancelWaiting(ctx context.Context, jobID string, after int64, limit int, now time.Time) (int, int64, error) {
	count, last := 0, after
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		entries, err := collectEntries(tx.Query(ctx, `
			SELECT `+entryColumns+` FROM ingestion_entries
			WHERE job_id = $1 AND position > $3 AND state IN ('PENDING', 'FAILED_RETRYABLE')
			  AND EXISTS (SELECT 1 FROM ingestion_jobs WHERE id = $1 AND state = 'CANCELLING')
			ORDER BY position LIMIT $2
			FOR UPDATE SKIP LOCKED
		`, jobID, limit, after))
		if err != nil || len(entries) == 0 {
			return err
		}
		var d delta
		positions := make([]int64, len(entries))
		for i, e := range entries {
			d.move(e, model.EntryCancelled, 0)
			positions[i] = e.Position
		}
		if _, err := tx.Exec(ctx, `
			UPDATE ingestion_entries
			SET state = 'CANCELLED', error_code = 'cancelled', error_detail = 'the job was cancelled',
			    next_attempt_at = NULL, updated_at = $3
			WHERE job_id = $1 AND position = ANY($2)
		`, jobID, positions, now); err != nil {
			return fmt.Errorf("cancel waiting entries: %w", err)
		}
		job, err := applyDelta(ctx, tx, entries[0].TenantID, entries[0].DatasetID, jobID, d, now)
		if err != nil {
			return err
		}
		count, last = len(entries), positions[len(positions)-1]
		_, err = settle(ctx, tx, job, now)
		return err
	})
	if err != nil {
		return 0, after, err
	}
	return count, last, nil
}

func (r *IngestionRepository) Retry(ctx context.Context, req interfaces.RetryRequest) (int, error) {
	if !req.All && len(req.Positions) == 0 {
		return 0, nil
	}
	count := 0
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		job, err := scanIngestionJob(tx.QueryRow(ctx,
			`SELECT `+ingestionJobColumns+` FROM ingestion_jobs WHERE tenant_id = $1 AND id = $2`, req.TenantID, req.JobID))
		if errors.Is(err, pgx.ErrNoRows) {
			return model.ErrIngestionNotFound
		}
		if err != nil {
			return fmt.Errorf("read ingestion job: %w", err)
		}
		switch job.State {
		case model.JobRunning, model.JobReady, model.JobPartiallyCompleted, model.JobFailed, model.JobCompleted:
		default:
			return model.ErrInvalidJobState
		}
		// An empty array, not nil: nil is sent as NULL, and cardinality(NULL)
		// is not 0, so "retry all" would match nothing.
		positions := req.Positions
		if req.All {
			positions = []int64{}
		}
		// A held processing job is retried by requeueing it, which the
		// service does; everything else starts a new upload. A FAILED
		// entry whose path another entry has taken since cannot come back.
		entries, err := collectEntries(tx.Query(ctx, `
			SELECT `+entryColumns+` FROM ingestion_entries AS e
			WHERE e.job_id = $1
			  AND (cardinality($2::bigint[]) = 0 OR e.position = ANY($2))
			  AND ((e.state = 'FAILED_RETRYABLE' AND e.error_code IS DISTINCT FROM 'processing_exhausted')
			       OR (e.state = 'FAILED' AND e.error_code = ANY($3) AND e.error_code <> 'processing_exhausted' AND e.manual_retries < $4))
			  AND NOT EXISTS (
			      SELECT 1 FROM ingestion_entries AS o
			      WHERE e.state = 'FAILED' AND o.dataset_id = e.dataset_id AND o.path_key = e.path_key
			        AND o.state NOT IN ('INVALID', 'FAILED', 'CANCELLED'))
			ORDER BY e.position LIMIT $5
			FOR UPDATE OF e SKIP LOCKED
		`, req.JobID, positions, model.RetryableEntryErrors, req.MaxManualRetries, req.Limit))
		if err != nil || len(entries) == 0 {
			return err
		}
		// Reviving a finished job refreshes its dataset after the job's
		// counters, so lock the usage and the dataset before the job.
		if err := lockUsage(ctx, tx, job.TenantID, req.Now); err != nil {
			return err
		}
		if _, err := lockDataset(ctx, tx, job.TenantID, job.DatasetID); err != nil {
			return err
		}
		var d delta
		moved := make([]int64, len(entries))
		for i, e := range entries {
			d.move(e, model.EntryPending, 0)
			moved[i] = e.Position
		}
		if _, err := tx.Exec(ctx, `
			UPDATE ingestion_entries
			SET state = 'PENDING', attempts = 0, manual_retries = manual_retries + 1,
			    upload_generation = upload_generation + 1, upload_id = NULL,
			    error_code = NULL, error_detail = NULL, next_attempt_at = NULL, lease_expires_at = NULL, updated_at = $3
			WHERE job_id = $1 AND position = ANY($2)
		`, req.JobID, moved, req.Now); err != nil {
			return fmt.Errorf("retry entries: %w", err)
		}
		if _, err := applyDelta(ctx, tx, job.TenantID, job.DatasetID, job.ID, d, req.Now); err != nil {
			return err
		}
		if job.State.Terminal() {
			// A finished job runs again, and counts as active again.
			tag, err := tx.Exec(ctx, `
				UPDATE ingestion_tenant_usage SET active_jobs = active_jobs + 1, updated_at = $3
				WHERE tenant_id = $1 AND active_jobs < $2
			`, job.TenantID, req.MaxActiveJobs, req.Now)
			if err != nil {
				return fmt.Errorf("count active job: %w", err)
			}
			if tag.RowsAffected() == 0 {
				return fmt.Errorf("%w: at most %d active ingestion jobs", model.ErrQuotaExceeded, req.MaxActiveJobs)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE ingestion_jobs SET state = 'RUNNING', completed_at = NULL, updated_at = $2 WHERE id = $1
			`, job.ID, req.Now); err != nil {
				return fmt.Errorf("restart ingestion job: %w", err)
			}
			if err := refreshDataset(ctx, tx, job.DatasetID, req.Now); err != nil {
				return err
			}
		}
		count = len(entries)
		return nil
	})
	return count, err
}

// ─── Quotas and usage ─────────────────────────────────────────────────

// Quota and SetQuota share the tenant quota policy (tenant_quotas) with
// the upload quota: one policy per tenant.
func (r *IngestionRepository) Quota(ctx context.Context, tenantID string) (model.TenantQuota, error) {
	return readQuota(ctx, r.db.Pool, tenantID)
}

func (r *IngestionRepository) SetQuota(ctx context.Context, tenantID string, q model.TenantQuota, now time.Time) error {
	return r.inTx(ctx, func(tx pgx.Tx) error {
		return writeQuota(ctx, tx, tenantID, q, now)
	})
}

func (r *IngestionRepository) Usage(ctx context.Context, tenantID string) (model.TenantUsage, error) {
	u := model.TenantUsage{TenantID: tenantID}
	err := r.db.Pool.QueryRow(ctx, `
		SELECT reserved_bytes, stored_bytes, active_jobs, active_uploads, active_multipart, pending_entries, awaiting_processing
		FROM ingestion_tenant_usage WHERE tenant_id = $1
	`, tenantID).Scan(&u.ReservedBytes, &u.StoredBytes, &u.ActiveJobs, &u.ActiveUploads, &u.ActiveMultipart,
		&u.PendingEntries, &u.AwaitingProcessing)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, nil
	}
	if err != nil {
		return u, fmt.Errorf("read tenant usage: %w", err)
	}
	return u, nil
}

func (r *IngestionRepository) SlotsInUse(ctx context.Context) (int, error) {
	return countSlots(ctx, r.db.Pool)
}

// countSlots counts the entries holding an upload slot, across tenants.
func countSlots(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (int, error) {
	var n int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM ingestion_entries WHERE state IN ('SCHEDULED', 'UPLOADING')`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count upload slots: %w", err)
	}
	return n, nil
}

// ─── Reconciliation ───────────────────────────────────────────────────

func (r *IngestionRepository) Recount(ctx context.Context, jobID string, now time.Time) (model.JobCounters, model.JobCounters, error) {
	var before, after model.JobCounters
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		job, err := scanIngestionJob(tx.QueryRow(ctx, `SELECT `+ingestionJobColumns+` FROM ingestion_jobs WHERE id = $1`, jobID))
		if errors.Is(err, pgx.ErrNoRows) {
			return model.ErrIngestionNotFound
		}
		if err != nil {
			return fmt.Errorf("read ingestion job: %w", err)
		}
		if err := lockUsage(ctx, tx, job.TenantID, now); err != nil {
			return err
		}
		if _, err := lockDataset(ctx, tx, job.TenantID, job.DatasetID); err != nil {
			return err
		}
		job, err = scanIngestionJob(tx.QueryRow(ctx, `SELECT `+ingestionJobColumns+` FROM ingestion_jobs WHERE id = $1 FOR UPDATE`, jobID))
		if err != nil {
			return fmt.Errorf("lock ingestion job: %w", err)
		}
		before = job.Counters

		rows, err := tx.Query(ctx, `
			SELECT state, count(*),
			       COALESCE(sum(size_bytes) FILTER (WHERE state <> 'INVALID'), 0),
			       COALESCE(sum(stored_bytes), 0),
			       count(*) FILTER (WHERE required AND state IN ('INVALID', 'FAILED', 'CANCELLED'))
			FROM ingestion_entries WHERE job_id = $1
			GROUP BY state
		`, jobID)
		if err != nil {
			return fmt.Errorf("recount entries: %w", err)
		}
		counts := map[model.EntryState]int64{}
		for rows.Next() {
			var state string
			var n, expected, stored, requiredFailed int64
			if err := rows.Scan(&state, &n, &expected, &stored, &requiredFailed); err != nil {
				rows.Close()
				return err
			}
			counts[model.EntryState(state)] = n
			after.Total += n
			after.ExpectedBytes += expected
			after.StoredBytes += stored
			after.RequiredFailed += requiredFailed
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		after.Invalid, after.Pending, after.Scheduled = counts[model.EntryInvalid], counts[model.EntryPending], counts[model.EntryScheduled]
		after.Uploading, after.Uploaded, after.Processing = counts[model.EntryUploading], counts[model.EntryUploaded], counts[model.EntryProcessing]
		after.Published, after.Retryable, after.Failed = counts[model.EntryPublished], counts[model.EntryRetryable], counts[model.EntryFailed]
		after.Cancelled = counts[model.EntryCancelled]
		if after == before {
			return nil
		}
		job, err = scanIngestionJob(tx.QueryRow(ctx, `
			UPDATE ingestion_jobs SET
			    total_entries = $2, invalid_entries = $3, pending_entries = $4, scheduled_entries = $5,
			    uploading_entries = $6, uploaded_entries = $7, processing_entries = $8, published_entries = $9,
			    retryable_entries = $10, failed_entries = $11, cancelled_entries = $12, required_failed = $13,
			    expected_bytes = $14, stored_bytes = $15, updated_at = $16
			WHERE id = $1 RETURNING `+ingestionJobColumns,
			jobID, after.Total, after.Invalid, after.Pending, after.Scheduled, after.Uploading, after.Uploaded,
			after.Processing, after.Published, after.Retryable, after.Failed, after.Cancelled, after.RequiredFailed,
			after.ExpectedBytes, after.StoredBytes, now))
		if err != nil {
			return fmt.Errorf("repair job counters: %w", err)
		}
		_, err = settle(ctx, tx, job, now)
		return err
	})
	return before, after, err
}

func (r *IngestionRepository) ReconcileUsage(ctx context.Context, tenantID string, now time.Time) (model.TenantUsage, model.TenantUsage, error) {
	var before, after model.TenantUsage
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		before, err = lockUsageRow(ctx, tx, tenantID, now)
		if err != nil {
			return err
		}
		after.TenantID = tenantID
		err = tx.QueryRow(ctx, `
			SELECT COALESCE(sum(stored_bytes), 0),
			       count(*) FILTER (WHERE state IN ('PENDING_VALIDATION', 'VALIDATING', 'READY', 'RUNNING', 'CANCELLING')),
			       COALESCE(sum(scheduled_entries + uploading_entries), 0),
			       COALESCE(sum(pending_entries + scheduled_entries + uploading_entries + uploaded_entries
			                    + processing_entries + retryable_entries), 0),
			       COALESCE(sum(uploaded_entries + processing_entries), 0)
			FROM ingestion_jobs WHERE tenant_id = $1
		`, tenantID).Scan(&after.StoredBytes, &after.ActiveJobs, &after.ActiveUploads, &after.PendingEntries, &after.AwaitingProcessing)
		if err != nil {
			return fmt.Errorf("sum tenant jobs: %w", err)
		}
		err = tx.QueryRow(ctx, `
			SELECT COALESCE(sum(e.reserved_bytes), 0),
			       count(*) FILTER (WHERE e.strategy = 'MULTIPART' AND e.state IN ('SCHEDULED', 'UPLOADING'))
			FROM ingestion_entries AS e
			JOIN ingestion_jobs AS j ON j.id = e.job_id
			WHERE j.tenant_id = $1 AND j.state NOT IN ('PARTIALLY_COMPLETED', 'COMPLETED', 'FAILED', 'CANCELLED')
			  AND e.state IN ('SCHEDULED', 'UPLOADING', 'UPLOADED', 'PROCESSING')
		`, tenantID).Scan(&after.ReservedBytes, &after.ActiveMultipart)
		if err != nil {
			return fmt.Errorf("sum tenant reservations: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE ingestion_tenant_usage
			SET reserved_bytes = $2, stored_bytes = $3, active_jobs = $4, active_uploads = $5, active_multipart = $6,
			    pending_entries = $7, awaiting_processing = $8, updated_at = $9
			WHERE tenant_id = $1
		`, tenantID, after.ReservedBytes, after.StoredBytes, after.ActiveJobs, after.ActiveUploads, after.ActiveMultipart,
			after.PendingEntries, after.AwaitingProcessing, now); err != nil {
			return fmt.Errorf("repair tenant usage: %w", err)
		}
		return nil
	})
	return before, after, err
}
