package copyengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

// Public internet egress prices per GB, from the providers' price lists.
// An estimate only: in-region, private links and committed discounts cost
// less, and S3-compatible stores set their own.
var egressPerGB = map[model.StorageProviderType]float64{
	model.ProviderAWSS3: 0.09, model.ProviderGCS: 0.12, model.ProviderAzureBlob: 0.087, model.ProviderR2: 0,
}

// MigrationPlan is what moving a dataset to another connection involves.
type MigrationPlan struct {
	Objects      int64   `json:"objects"`
	Bytes        int64   `json:"bytes"`
	AlreadyThere int64   `json:"already_there"`
	EgressUSD    float64 `json:"egress_estimate_usd"`
	EgressNote   string  `json:"egress_note"`
}

func (s *Service) plan(ctx context.Context, tenantID string, assets []postgres.StoredAsset, targetID string) (MigrationPlan, []postgres.StoredAsset) {
	var plan MigrationPlan
	seen := map[string]bool{}
	var todo []postgres.StoredAsset
	unknown := map[string]bool{}
	for _, a := range assets {
		if a.ConnectionID == targetID {
			plan.AlreadyThere++
			continue
		}
		key := a.ConnectionID + "\x00" + a.ObjectKey
		if seen[key] {
			continue
		}
		seen[key] = true
		todo = append(todo, a)
		plan.Objects++
		plan.Bytes += a.SizeBytes
		provider := model.ProviderPlatform
		if loc, err := s.deps.Router.Locate(ctx, tenantID, a.ConnectionID); err == nil {
			provider = loc.Provider
		}
		price, known := egressPerGB[provider]
		if !known {
			unknown[string(provider)] = true
		}
		plan.EgressUSD += float64(a.SizeBytes) / (1 << 30) * price
	}
	plan.EgressUSD = float64(int64(plan.EgressUSD*100+0.5)) / 100
	plan.EgressNote = "internet egress at the providers' list prices; transfers inside one region or over private links cost less"
	if len(unknown) > 0 {
		var names []string
		for n := range unknown {
			names = append(names, n)
		}
		sort.Strings(names)
		plan.EgressNote += "; not priced: " + strings.Join(names, ", ")
	}
	return plan, todo
}

// MigrationInput moves a dataset's objects to another connection.
type MigrationInput struct {
	TargetConnectionID string
	DryRun             bool
	IdempotencyKey     string
}

// MigrateFor plans a migration and, unless it is a dry run, queues it.
// Every object is copied and verified first; only when all are does the
// dataset switch, in one transaction, keeping its assets' IDs. The source
// objects are left where they were.
func (s *Service) MigrateFor(ctx context.Context, p model.Principal, datasetID string, in MigrationInput) (MigrationPlan, *model.CopyJob, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermStorageManage|model.PermDatasetsManage, "dataset.migrate", "dataset", datasetID); err != nil {
		return MigrationPlan{}, nil, err
	}
	_, current, err := s.deps.Store.Protection(ctx, p.TenantID, datasetID)
	if err != nil {
		return MigrationPlan{}, nil, err
	}
	if in.TargetConnectionID == "" || in.TargetConnectionID == current || (current == "" && model.IsPlatformConnection(in.TargetConnectionID)) {
		return MigrationPlan{}, nil, fmt.Errorf("%w: choose a target connection other than the dataset's own", model.ErrInvalidCopyJob)
	}
	if _, err := s.deps.Router.Locate(ctx, p.TenantID, in.TargetConnectionID); err != nil {
		return MigrationPlan{}, nil, err
	}
	assets, err := s.deps.Store.DatasetAssets(ctx, p.TenantID, datasetID)
	if err != nil {
		return MigrationPlan{}, nil, err
	}
	plan, _ := s.plan(ctx, p.TenantID, assets, in.TargetConnectionID)
	if in.DryRun {
		return plan, nil, nil
	}
	options, _ := json.Marshal(plan)
	job, err := s.create(ctx, p, model.CopyJob{Kind: model.CopyMigrate, DatasetID: datasetID, SourceConnectionID: current,
		TargetConnectionID: in.TargetConnectionID, Options: options, IdempotencyKey: in.IdempotencyKey})
	return plan, &job, err
}

// RestoreInput copies a snapshot to a connection, under a prefix of its
// own.
type RestoreInput struct {
	VersionID          string
	TargetConnectionID string
	Prefix             string
	IdempotencyKey     string
}

func (s *Service) RestoreFor(ctx context.Context, p model.Principal, datasetID string, in RestoreInput) (model.CopyJob, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsRead|model.PermStorageUse, "dataset.restore", "dataset", datasetID); err != nil {
		return model.CopyJob{}, err
	}
	if in.VersionID == "" || in.TargetConnectionID == "" {
		return model.CopyJob{}, fmt.Errorf("%w: a version and a target connection are required", model.ErrInvalidCopyJob)
	}
	if _, err := s.deps.Router.Locate(ctx, p.TenantID, in.TargetConnectionID); err != nil {
		return model.CopyJob{}, err
	}
	return s.create(ctx, p, model.CopyJob{Kind: model.CopyRestore, DatasetID: datasetID, VersionID: in.VersionID,
		TargetConnectionID: in.TargetConnectionID, TargetPrefix: cleanPath(in.Prefix), IdempotencyKey: in.IdempotencyKey})
}

// DrillInput re-reads a sample of a dataset, from its own connection or
// its replica, and measures how fast a restore would run.
type DrillInput struct {
	Sample         int
	FromReplica    bool
	IdempotencyKey string
}

type drillOptions struct {
	Sample      int  `json:"sample"`
	FromReplica bool `json:"from_replica"`
}

func (s *Service) DrillFor(ctx context.Context, p model.Principal, datasetID string, in DrillInput) (model.CopyJob, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsManage, "dataset.drill", "dataset", datasetID); err != nil {
		return model.CopyJob{}, err
	}
	if in.Sample <= 0 {
		in.Sample = 100
	}
	if in.Sample > 10000 {
		return model.CopyJob{}, fmt.Errorf("%w: a drill samples at most 10000 objects", model.ErrInvalidCopyJob)
	}
	if _, _, err := s.deps.Store.Protection(ctx, p.TenantID, datasetID); err != nil {
		return model.CopyJob{}, err
	}
	options, _ := json.Marshal(drillOptions{Sample: in.Sample, FromReplica: in.FromReplica})
	return s.create(ctx, p, model.CopyJob{Kind: model.CopyDrill, DatasetID: datasetID, Options: options, IdempotencyKey: in.IdempotencyKey})
}

// ExportFor queues the export-everything bundle to a connection.
func (s *Service) ExportFor(ctx context.Context, p model.Principal, connectionID, idempotencyKey string) (model.CopyJob, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermAuditRead|model.PermStorageUse, "organization.export", "storage_connection", connectionID); err != nil {
		return model.CopyJob{}, err
	}
	if _, err := s.deps.Router.Locate(ctx, p.TenantID, connectionID); err != nil {
		return model.CopyJob{}, err
	}
	return s.create(ctx, p, model.CopyJob{Kind: model.CopyExport, TargetConnectionID: connectionID, IdempotencyKey: idempotencyKey})
}

func (s *Service) create(ctx context.Context, p model.Principal, j model.CopyJob) (model.CopyJob, error) {
	j.ID, j.TenantID, j.CreatedBy, j.CreatedAt = s.newID(), p.TenantID, p.UserID, s.now()
	stored, created, err := s.deps.Store.CreateJob(ctx, j)
	if err == nil && created && s.deps.Audit != nil {
		e := audit.From(ctx, p, "copy_job.create", "copy_job", stored.ID)
		e.Details = map[string]any{"kind": stored.Kind, "dataset_id": stored.DatasetID, "target": stored.TargetConnectionID}
		s.deps.Audit.Record(ctx, e)
	}
	return stored, err
}

func (s *Service) JobsFor(ctx context.Context, p model.Principal, datasetID string) ([]model.CopyJob, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsRead, "copy_job.read", "copy_job", ""); err != nil {
		return nil, err
	}
	return s.deps.Store.ListJobs(ctx, p.TenantID, datasetID, 200)
}

func (s *Service) JobFor(ctx context.Context, p model.Principal, id string) (model.CopyJob, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsRead, "copy_job.read", "copy_job", id); err != nil {
		return model.CopyJob{}, err
	}
	return s.deps.Store.GetJob(ctx, p.TenantID, id)
}

func (s *Service) ItemsFor(ctx context.Context, p model.Principal, id, state string, after, limit int) ([]model.CopyItem, error) {
	if _, err := s.JobFor(ctx, p, id); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	return s.deps.Store.Items(ctx, p.TenantID, id, state, after, limit)
}

func (s *Service) CancelFor(ctx context.Context, p model.Principal, id string) error {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsManage, "copy_job.cancel", "copy_job", id); err != nil {
		return err
	}
	return s.deps.Store.Cancel(ctx, p.TenantID, id, s.now())
}

// ─── Running jobs ─────────────────────────────────────────────────────

// RunOnce advances one job by a batch of objects, planning it first, and
// finishes it once nothing is left. It reports whether it found a job.
func (s *Service) RunOnce(ctx context.Context) (bool, error) {
	job, ok, err := s.deps.Store.ClaimJob(ctx, s.cfg.Owner, s.now(), s.cfg.Lease)
	if err != nil || !ok {
		return false, err
	}
	defer func() { _ = s.deps.Store.Release(context.WithoutCancel(ctx), job.ID, s.cfg.Owner) }()
	if !job.Planned {
		items, err := s.items(ctx, job)
		if err != nil {
			return true, s.deps.Store.Finish(ctx, job.ID, s.cfg.Owner, model.CopyFailed, truncate(err.Error(), 500), map[string]any{}, s.now())
		}
		if err := s.deps.Store.AddItems(ctx, job.ID, items); err != nil {
			return true, err
		}
	}
	pending, err := s.deps.Store.PendingItems(ctx, job.ID, s.cfg.Batch)
	if err != nil {
		return true, err
	}
	for _, it := range pending {
		started := s.now()
		done := s.process(ctx, job, it)
		done.DurationMS = s.now().Sub(started).Milliseconds()
		if err := s.deps.Store.FinishItem(ctx, done, s.now()); err != nil {
			return true, err
		}
	}
	if len(pending) == s.cfg.Batch {
		return true, nil
	}
	job, err = s.deps.Store.GetJob(ctx, job.TenantID, job.ID)
	if err != nil {
		return true, err
	}
	return true, s.finish(ctx, job)
}

// items lists what a job copies.
func (s *Service) items(ctx context.Context, job model.CopyJob) ([]model.CopyItem, error) {
	var items []model.CopyItem
	add := func(a postgres.StoredAsset, key string) {
		items = append(items, model.CopyItem{JobID: job.ID, Ordinal: len(items) + 1, TenantID: job.TenantID, AssetID: a.AssetID,
			Path: a.Path, TargetKey: key, SizeBytes: a.SizeBytes, ChecksumSHA256: a.SHA256})
	}
	switch job.Kind {
	case model.CopyMigrate:
		target, err := s.deps.Router.Locate(ctx, job.TenantID, job.TargetConnectionID)
		if err != nil {
			return nil, err
		}
		assets, err := s.deps.Store.DatasetAssets(ctx, job.TenantID, job.DatasetID)
		if err != nil {
			return nil, err
		}
		_, todo := s.plan(ctx, job.TenantID, assets, job.TargetConnectionID)
		for _, a := range todo {
			add(a, promotedKey(target.ReadyPrefix, a))
		}
	case model.CopyRestore:
		target, err := s.deps.Router.Locate(ctx, job.TenantID, job.TargetConnectionID)
		if err != nil {
			return nil, err
		}
		assets, err := s.deps.Store.VersionAssets(ctx, job.TenantID, job.VersionID)
		if err != nil {
			return nil, err
		}
		if len(assets) == 0 {
			return nil, fmt.Errorf("%w: the version has no available members", model.ErrInvalidCopyJob)
		}
		base := s.restoreBase(target, job)
		for _, a := range assets {
			add(a, base+cleanPath(a.Path))
		}
	case model.CopyDrill:
		var opts drillOptions
		_ = json.Unmarshal(job.Options, &opts)
		assets, err := s.deps.Store.DatasetAssets(ctx, job.TenantID, job.DatasetID)
		if err != nil {
			return nil, err
		}
		// A sample that differs from drill to drill, but is the same if one
		// is re-planned.
		rank := func(a postgres.StoredAsset) string {
			sum := sha256.Sum256([]byte(job.ID + a.AssetID))
			return hex.EncodeToString(sum[:])
		}
		sort.Slice(assets, func(i, j int) bool { return rank(assets[i]) < rank(assets[j]) })
		for _, a := range assets[:min(len(assets), opts.Sample)] {
			add(a, "")
		}
	case model.CopyExport:
		items = append(items, model.CopyItem{JobID: job.ID, Ordinal: 1, TenantID: job.TenantID, Path: "bundle", TargetKey: ""})
	}
	return items, nil
}

func (s *Service) restoreBase(target interfaces.StorageLocation, job model.CopyJob) string {
	name := job.TargetPrefix
	if name == "" {
		name = job.ID
	}
	return target.ReadyPrefix + "/" + job.TenantID + "/.restores/" + name + "/"
}

// process copies or checks one item.
func (s *Service) process(ctx context.Context, job model.CopyJob, it model.CopyItem) model.CopyItem {
	fail := func(err error) model.CopyItem {
		it.State, it.Error = model.ItemFailed, truncate(err.Error(), 500)
		return it
	}
	if job.Kind == model.CopyExport {
		if err := s.exportBundle(ctx, job); err != nil {
			return fail(err)
		}
		it.State = model.ItemDone
		return it
	}
	a, err := s.deps.Store.GetStoredAsset(ctx, job.TenantID, it.AssetID)
	if err != nil {
		return fail(err)
	}
	a.Path = it.Path
	switch job.Kind {
	case model.CopyDrill:
		var opts drillOptions
		_ = json.Unmarshal(job.Options, &opts)
		if err := s.reread(ctx, a, opts.FromReplica); err != nil {
			return fail(err)
		}
	default:
		target, err := s.deps.Router.Locate(ctx, job.TenantID, job.TargetConnectionID)
		if err != nil {
			return fail(err)
		}
		meta, err := s.copyTo(ctx, a, target, it.TargetKey)
		if err != nil {
			return fail(err)
		}
		if job.Kind == model.CopyMigrate {
			if _, err := s.carryLock(ctx, a, target, it.TargetKey, meta.VersionID); err != nil {
				return fail(err)
			}
		}
		it.TargetVersionID, it.TargetETag = meta.VersionID, meta.ETag
	}
	it.State = model.ItemDone
	return it
}

// reread reads an asset's bytes, or its replica's, and checks them.
func (s *Service) reread(ctx context.Context, a postgres.StoredAsset, fromReplica bool) error {
	open := func(ctx context.Context) (io.ReadCloser, error) { return s.open(ctx, a) }
	if fromReplica {
		rep, ok, err := s.deps.Store.VerifiedReplica(ctx, a.TenantID, a.AssetID)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("MISSING: no replica of this object")
		}
		open = func(ctx context.Context) (io.ReadCloser, error) { return s.openReplica(ctx, rep) }
	}
	body, err := open(ctx)
	if errors.Is(err, interfaces.ErrObjectNotFound) {
		return errors.New("MISSING: the stored object is gone")
	}
	if err != nil {
		return fmt.Errorf("UNREADABLE: %w", err)
	}
	defer body.Close()
	sum, n, err := hashOf(body)
	switch {
	case err != nil:
		return fmt.Errorf("UNREADABLE: %w", err)
	case n != a.SizeBytes || (a.SHA256 != "" && sum != a.SHA256):
		return fmt.Errorf("MISMATCH: read %d bytes hashing to %s", n, sum)
	}
	return nil
}

func (s *Service) openReplica(ctx context.Context, rep model.AssetReplica) (io.ReadCloser, error) {
	loc, err := s.deps.Router.Locate(ctx, rep.TenantID, rep.ConnectionID)
	if err != nil {
		return nil, err
	}
	return loc.Storage.OpenRange(ctx, loc.ReadyBucket, rep.ObjectKey, 0, -1, interfaces.ReadCondition{VersionID: rep.VersionID})
}

// allItems pages through a job's items in one state.
func (s *Service) allItems(ctx context.Context, job model.CopyJob, state string) ([]model.CopyItem, error) {
	var out []model.CopyItem
	after := 0
	for {
		page, err := s.deps.Store.Items(ctx, job.TenantID, job.ID, state, after, 1000)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if len(page) < 1000 {
			return out, nil
		}
		after = page[len(page)-1].Ordinal
	}
}

// finish ends a job once every item is done or failed.
func (s *Service) finish(ctx context.Context, job model.CopyJob) error {
	started := s.now()
	if job.StartedAt != nil {
		started = *job.StartedAt
	}
	report := map[string]any{"objects": job.TotalItems, "copied": job.DoneItems, "failed": job.FailedItems,
		"bytes": job.DoneBytes, "seconds": int64(s.now().Sub(started).Seconds())}
	failed, err := s.allItems(ctx, job, model.ItemFailed)
	if err != nil {
		return err
	}
	if len(failed) > 0 {
		var sample []map[string]any
		for _, it := range failed[:min(len(failed), 50)] {
			sample = append(sample, map[string]any{"asset_id": it.AssetID, "path": it.Path, "error": it.Error})
		}
		report["failures"] = sample
	}
	state, message := model.CopyCompleted, ""
	switch job.Kind {
	case model.CopyMigrate:
		if len(failed) > 0 {
			state, message = model.CopyFailed, fmt.Sprintf("%d objects could not be copied and verified; nothing was switched", len(failed))
			report["switched"] = 0
			break
		}
		done, err := s.allItems(ctx, job, model.ItemDone)
		if err != nil {
			return err
		}
		var moved []postgres.SwitchedAsset
		for _, it := range done {
			a, err := s.deps.Store.GetStoredAsset(ctx, job.TenantID, it.AssetID)
			if err != nil {
				return err
			}
			moved = append(moved, postgres.SwitchedAsset{SourceConnectionID: a.ConnectionID, SourceKey: a.ObjectKey,
				TargetKey: it.TargetKey, VersionID: it.TargetVersionID, ETag: it.TargetETag})
		}
		switched, err := s.deps.Store.SwitchConnection(ctx, job.TenantID, job.DatasetID, job.TargetConnectionID, moved, s.now())
		if err != nil {
			state, message = model.CopyFailed, "the copies are verified but the switch failed: "+truncate(err.Error(), 300)
			break
		}
		report["switched"] = switched
		report["source_objects"] = "left in place; delete them at the source once the target is trusted"
		var plan MigrationPlan
		_ = json.Unmarshal(job.Options, &plan)
		report["egress_estimate_usd"] = plan.EgressUSD
	case model.CopyRestore:
		if err := s.writeRestoreManifest(ctx, job); err != nil {
			state, message = model.CopyFailed, "the restore manifest could not be written: "+truncate(err.Error(), 300)
		} else if len(failed) > 0 {
			state, message = model.CopyFailed, fmt.Sprintf("%d of %d objects could not be restored", len(failed), job.TotalItems)
		}
		report["manifest"] = "manifest-sha256.txt"
	case model.CopyDrill:
		s.drillReport(ctx, job, report, failed)
	case model.CopyExport:
		if len(failed) > 0 {
			state, message = model.CopyFailed, failed[0].Error
		}
	}
	return s.deps.Store.Finish(ctx, job.ID, s.cfg.Owner, state, message, report, s.now())
}

// writeRestoreManifest writes what the restore holds, with each file's
// SHA-256, next to it.
func (s *Service) writeRestoreManifest(ctx context.Context, job model.CopyJob) error {
	target, err := s.deps.Router.Locate(ctx, job.TenantID, job.TargetConnectionID)
	if err != nil {
		return err
	}
	done, err := s.allItems(ctx, job, model.ItemDone)
	if err != nil {
		return err
	}
	base := s.restoreBase(target, job)
	var b strings.Builder
	for _, it := range done {
		fmt.Fprintf(&b, "%s  %s\n", it.ChecksumSHA256, strings.TrimPrefix(it.TargetKey, base))
	}
	body := b.String()
	_, err = target.Storage.WriteObject(ctx, target.ReadyBucket, base+"manifest-sha256.txt", strings.NewReader(body), int64(len(body)), "text/plain")
	return err
}

// drillReport measures the drill: how much read back intact, and how long
// restoring the whole dataset would take at that speed.
func (s *Service) drillReport(ctx context.Context, job model.CopyJob, report map[string]any, failed []model.CopyItem) {
	done, _ := s.allItems(ctx, job, model.ItemDone)
	var ms, bytes int64
	for _, it := range append(done, failed...) {
		ms += it.DurationMS
		if it.State == model.ItemDone {
			bytes += it.SizeBytes
		}
	}
	kinds := map[string]int{}
	for _, it := range failed {
		kind, _, _ := strings.Cut(it.Error, ":")
		kinds[kind]++
	}
	report["sampled"], report["intact"], report["problems"] = job.TotalItems, len(done), kinds
	if ms > 0 && bytes > 0 {
		perSecond := float64(bytes) / (float64(ms) / 1000)
		report["read_bytes_per_second"] = int64(perSecond)
		if assets, err := s.deps.Store.DatasetAssets(ctx, job.TenantID, job.DatasetID); err == nil {
			var total int64
			for _, a := range assets {
				total += a.SizeBytes
			}
			report["dataset_bytes"] = total
			report["estimated_restore_seconds"] = int64(float64(total) / perSecond)
		}
	}
	if len(failed) > 0 && s.deps.Alerts != nil {
		_, _ = s.deps.Alerts.AddAlert(ctx, model.SecurityAlert{
			ID: s.newID(), TenantID: job.TenantID, PrincipalID: "dataset:" + job.DatasetID, Kind: model.AlertScrubMismatch,
			Detail:  fmt.Sprintf("a restore drill found %d of %d sampled objects missing or damaged", len(failed), job.TotalItems),
			Metrics: map[string]any{"job_id": job.ID, "dataset_id": job.DatasetID}, State: "OPEN",
			DedupKey: "drill:" + job.ID, CreatedAt: s.now(),
		})
	}
}

// ─── Schedules ────────────────────────────────────────────────────────

// RunSchedules queues the export bundles that are due, and raises an
// alert for anyone who asked for many deletions in the last hour.
func (s *Service) RunSchedules(ctx context.Context) error {
	due, err := s.deps.Store.DueSchedules(ctx, s.now())
	if err != nil {
		return err
	}
	for _, sch := range due {
		job, _, err := s.deps.Store.CreateJob(ctx, model.CopyJob{ID: s.newID(), TenantID: sch.TenantID, Kind: model.CopyExport,
			TargetConnectionID: sch.ConnectionID, CreatedBy: "schedule", CreatedAt: s.now(),
			IdempotencyKey: "schedule-" + s.now().Format("2006010215")})
		if err != nil {
			return err
		}
		if err := s.deps.Store.MarkScheduleRun(ctx, sch.TenantID, job.ID, s.now()); err != nil {
			return err
		}
	}
	if s.cfg.MassDeletions <= 0 || s.deps.Alerts == nil {
		return nil
	}
	counts, err := s.deps.Store.RecentDeletions(ctx, s.now().Add(-time.Hour), s.cfg.MassDeletions)
	if err != nil {
		return err
	}
	for _, c := range counts {
		if _, err := s.deps.Alerts.AddAlert(ctx, model.SecurityAlert{
			ID: s.newID(), TenantID: c.TenantID, PrincipalID: c.Actor, Kind: model.AlertMassDeletion,
			Detail:  fmt.Sprintf("%d deletions requested in the last hour", c.Count),
			Metrics: map[string]any{"deletions": c.Count}, State: "OPEN",
			DedupKey: "deletions:" + c.Actor + ":" + s.now().Format("2006-01-02T15"), CreatedAt: s.now(),
		}); err != nil {
			return err
		}
	}
	return nil
}
