package dataset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/correlation"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

// ErrNoVersion: a validation needs a version, and the dataset has none.
var ErrNoVersion = errors.New("the dataset has no version yet: take a snapshot first")

// ValidationInput describes a validation or readiness assessment.
type ValidationInput struct {
	// VersionID defaults to the dataset's current version.
	VersionID string
	// ProfileID defaults to the dataset's profile, then (for a validation,
	// not an assessment) to builtin:general. Version 0 is the latest.
	ProfileID      string
	ProfileVersion int
	Mode           model.ValidationMode
	// SampleSize is how many members a SAMPLED run reads.
	SampleSize     int
	IdempotencyKey string
}

// StartValidation queues a validation of a dataset version.
func (s *Service) StartValidation(ctx context.Context, p model.Principal, datasetID string, in ValidationInput) (model.ValidationJob, bool, error) {
	return s.start(ctx, p, datasetID, in, false)
}

// StartReadiness queues a readiness assessment: a validation whose report
// also judges each readiness dimension against the profile the caller
// chose.
func (s *Service) StartReadiness(ctx context.Context, p model.Principal, datasetID string, in ValidationInput) (model.ValidationJob, bool, error) {
	return s.start(ctx, p, datasetID, in, true)
}

func (s *Service) start(ctx context.Context, p model.Principal, datasetID string, in ValidationInput, readiness bool) (model.ValidationJob, bool, error) {
	action := "dataset.validate"
	if readiness {
		action = "dataset.readiness"
	}
	if err := s.authorize(ctx, p, model.PermDatasetsWrite, action, "dataset", datasetID); err != nil {
		return model.ValidationJob{}, false, err
	}
	if len(in.IdempotencyKey) > 128 || hasControl(in.IdempotencyKey) {
		return model.ValidationJob{}, false, fmt.Errorf("%w: the idempotency key is at most 128 characters", ErrInvalidRequest)
	}
	d, err := s.deps.Repo.GetDataset(ctx, p.TenantID, datasetID)
	if err != nil {
		return model.ValidationJob{}, false, err
	}
	if d.Lifecycle == model.LifecycleArchived {
		return model.ValidationJob{}, false, model.ErrDatasetArchived
	}
	versionID := in.VersionID
	if versionID == "" {
		versionID = d.CurrentVersionID
	}
	if versionID == "" {
		return model.ValidationJob{}, false, ErrNoVersion
	}
	v, err := s.Version(ctx, p, datasetID, versionID)
	if err != nil {
		return model.ValidationJob{}, false, err
	}

	profileID, profileVersion := in.ProfileID, in.ProfileVersion
	if profileID == "" {
		profileID, profileVersion = d.ProfileID, d.ProfileVersion
	}
	if profileID == "" {
		if readiness {
			return model.ValidationJob{}, false, fmt.Errorf("%w: a readiness assessment needs a profile: name one, or set the dataset's", ErrInvalidRequest)
		}
		profileID = "builtin:general"
	}
	profile, err := s.profile(ctx, p.TenantID, profileID, profileVersion)
	if err != nil {
		return model.ValidationJob{}, false, err
	}
	if _, err := compile(profile.Definition); err != nil {
		return model.ValidationJob{}, false, err
	}

	if in.Mode == "" {
		in.Mode = model.ModeMetadata
	}
	if !in.Mode.Valid() {
		return model.ValidationJob{}, false, fmt.Errorf("%w: mode must be METADATA, FULL or SAMPLED", ErrInvalidRequest)
	}
	switch {
	case in.Mode != model.ModeSampled && in.SampleSize != 0:
		return model.ValidationJob{}, false, fmt.Errorf("%w: sample_size is for SAMPLED validations", ErrInvalidRequest)
	case in.Mode == model.ModeSampled && in.SampleSize == 0:
		in.SampleSize = s.cfg.DefaultSampleSize
	case in.SampleSize < 0 || in.SampleSize > s.cfg.MaxSampleSize:
		return model.ValidationJob{}, false, fmt.Errorf("%w: sample_size must be between 1 and %d", ErrInvalidRequest, s.cfg.MaxSampleSize)
	}

	now := s.now()
	id := event.NewID()
	job, created, err := s.deps.Repo.CreateValidation(ctx, model.ValidationJob{
		ID: id, TenantID: p.TenantID, DatasetID: datasetID, DatasetVersionID: v.ID,
		ProfileID: profile.ID, ProfileVersion: profile.Version, Profile: profile.Definition,
		Mode: in.Mode, SampleSize: in.SampleSize, Readiness: readiness, MaxAttempts: s.cfg.MaxAttempts,
		IdempotencyKey: in.IdempotencyKey, CreatedBy: p.UserID, CreatedAt: now,
	}, s.cfg.MaxActiveValidations, event.NewValidationQueued(p.TenantID, id, now, correlation.From(ctx)))
	if err == nil && created {
		s.record(ctx, p, action, "dataset", datasetID, map[string]any{
			"job_id": job.ID, "version_id": v.ID, "profile": profile.ID, "profile_version": profile.Version, "mode": string(in.Mode)})
		s.count("dataset_validations_created", 1)
	}
	return job, created, err
}

func (s *Service) Validation(ctx context.Context, p model.Principal, jobID string) (model.ValidationJob, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsRead, "dataset.read", "validation_job", jobID); err != nil {
		return model.ValidationJob{}, err
	}
	return s.deps.Repo.GetValidation(ctx, p.TenantID, jobID)
}

// Validations pages a dataset's validations, newest first.
func (s *Service) Validations(ctx context.Context, p model.Principal, datasetID, cursor string, limit int) (Page[model.ValidationJob], error) {
	if err := s.authorize(ctx, p, model.PermDatasetsRead, "dataset.read", "dataset", datasetID); err != nil {
		return Page[model.ValidationJob]{}, err
	}
	size, err := s.pageSize(limit)
	if err != nil {
		return Page[model.ValidationJob]{}, err
	}
	if _, err := s.deps.Repo.GetDataset(ctx, p.TenantID, datasetID); err != nil {
		return Page[model.ValidationJob]{}, err
	}
	jobs, next, err := s.deps.Repo.ListValidations(ctx, p.TenantID, datasetID, cursor, size)
	return Page[model.ValidationJob]{Items: jobs, NextCursor: next}, err
}

// CancelValidation stops a validation: at once when no worker holds it,
// otherwise after the worker's current batch.
func (s *Service) CancelValidation(ctx context.Context, p model.Principal, jobID string) (model.ValidationJob, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsWrite, "dataset.validation_cancel", "validation_job", jobID); err != nil {
		return model.ValidationJob{}, err
	}
	job, err := s.deps.Repo.CancelValidation(ctx, p.TenantID, jobID, s.now())
	if err == nil {
		s.record(ctx, p, "dataset.validation_cancel", "validation_job", jobID, map[string]any{"state": string(job.State)})
	}
	return job, err
}

func (s *Service) Report(ctx context.Context, p model.Principal, reportID string) (model.ValidationReport, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsRead, "dataset.read", "validation_report", reportID); err != nil {
		return model.ValidationReport{}, err
	}
	return s.deps.Repo.GetReport(ctx, p.TenantID, reportID)
}

// Reports pages a dataset's reports, newest first; readinessOnly keeps
// readiness assessments.
func (s *Service) Reports(ctx context.Context, p model.Principal, datasetID string, readinessOnly bool, cursor string, limit int) (Page[model.ValidationReport], error) {
	if err := s.authorize(ctx, p, model.PermDatasetsRead, "dataset.read", "dataset", datasetID); err != nil {
		return Page[model.ValidationReport]{}, err
	}
	size, err := s.pageSize(limit)
	if err != nil {
		return Page[model.ValidationReport]{}, err
	}
	if _, err := s.deps.Repo.GetDataset(ctx, p.TenantID, datasetID); err != nil {
		return Page[model.ValidationReport]{}, err
	}
	reports, next, err := s.deps.Repo.ListReports(ctx, p.TenantID, datasetID, readinessOnly, cursor, size)
	return Page[model.ValidationReport]{Items: reports, NextCursor: next}, err
}

// Findings pages a report's findings, optionally of one severity or rule.
func (s *Service) Findings(ctx context.Context, p model.Principal, reportID, severity, ruleID, cursor string, limit int) (Page[model.Finding], error) {
	rep, err := s.Report(ctx, p, reportID)
	if err != nil {
		return Page[model.Finding]{}, err
	}
	sev := model.Severity(strings.ToUpper(severity))
	if severity != "" && !sev.Valid() {
		return Page[model.Finding]{}, fmt.Errorf("%w: severity must be ERROR, WARNING or INFO", ErrInvalidRequest)
	}
	size, err := s.pageSize(limit)
	if err != nil {
		return Page[model.Finding]{}, err
	}
	after, err := decodeCursor(cursor)
	if err != nil {
		return Page[model.Finding]{}, err
	}
	page, err := s.deps.Repo.ListFindings(ctx, p.TenantID, rep.JobID, interfaces.FindingFilter{Severity: sev, RuleID: ruleID, After: after, Limit: size})
	if err != nil {
		return Page[model.Finding]{}, err
	}
	out := Page[model.Finding]{Items: page.Findings}
	if page.After != "" {
		out.NextCursor = encodeCursor(page.After)
	}
	return out, nil
}

// ─── The worker's side ────────────────────────────────────────────────

// RunValidation runs a validation named by its event. The event's tenant
// must be the job's.
func (s *Service) RunValidation(ctx context.Context, owner, tenantID, jobID string) (string, error) {
	jobs, err := s.deps.Repo.ClaimValidations(ctx, owner, jobID, s.now(), s.cfg.Lease, s.cfg.RunningPerTenant, 1)
	if err != nil {
		return "", err
	}
	if len(jobs) == 0 {
		return "not due: running, finished, waiting to retry, or its tenant is at its limit", nil
	}
	job := jobs[0]
	if job.TenantID != tenantID {
		_ = s.deps.Repo.RetryValidation(ctx, job.ID, owner, s.now(), "event tenant mismatch", s.now())
		return "", retry.Permanent(fmt.Errorf("the event's tenant does not own validation %s", job.ID))
	}
	return s.runValidation(ctx, owner, job), nil
}

// RunDueValidations claims due validations, up to slots, and runs them
// side by side.
func (s *Service) RunDueValidations(ctx context.Context, owner string, slots int) (int, error) {
	jobs, err := s.deps.Repo.ClaimValidations(ctx, owner, "", s.now(), s.cfg.Lease, s.cfg.RunningPerTenant, slots)
	if err != nil {
		return 0, err
	}
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.runValidation(ctx, owner, job)
		}()
	}
	wg.Wait()
	return len(jobs), nil
}

// runValidation runs a claimed job for at most the validation budget.
// Failures stay on the job, which is retried with backoff until its
// attempts run out and then fails (its dead letter). The note says what
// happened.
func (s *Service) runValidation(ctx context.Context, owner string, job model.ValidationJob) string {
	log := s.deps.Log.With("tenant_id", job.TenantID, "dataset_id", job.DatasetID, "version_id", job.DatasetVersionID, "job_id", job.ID)
	fail := func(reason string) string {
		if err := s.deps.Repo.FailValidation(context.WithoutCancel(ctx), job.ID, owner, reason, s.now()); err != nil {
			log.Error("failing a validation failed", "error", retry.Sanitize(err))
			return "fail failed"
		}
		s.count("dataset_validations_failed_to_run", 1)
		log.Warn("validation could not run", "reason", reason)
		return "validation failed to run"
	}
	switch {
	case job.CancelRequested:
		if err := s.deps.Repo.StopValidation(context.WithoutCancel(ctx), job.ID, owner, s.now()); err != nil {
			return "stop failed"
		}
		s.count("dataset_validations_cancelled", 1)
		return "validation cancelled"
	case job.Attempts > job.MaxAttempts:
		s.count("dataset_validation_dead", 1)
		return fail("attempts exhausted: " + job.LastError)
	case job.StartedAt != nil && s.now().Sub(*job.StartedAt) > s.cfg.MaxJobDuration:
		return fail(fmt.Sprintf("the validation ran longer than %s", s.cfg.MaxJobDuration))
	}
	checks, err := compile(job.Profile)
	if err != nil {
		return fail(retry.Sanitize(err))
	}

	note, err := s.validate(ctx, owner, job, checks, log)
	switch {
	case err == nil:
		return note
	case errors.Is(err, model.ErrValidationState):
		return "lost the validation's lease"
	case retry.IsPermanent(err):
		return fail(retry.Sanitize(err))
	}
	s.count("dataset_validation_retries", 1)
	next := s.now().Add(s.retry.Delay(job.Attempts))
	if rerr := s.deps.Repo.RetryValidation(context.WithoutCancel(ctx), job.ID, owner, next, retry.Sanitize(err), s.now()); rerr != nil {
		log.Error("scheduling a validation retry failed", "error", retry.Sanitize(rerr))
	}
	log.Warn("validation will be retried", "attempt", job.Attempts, "error", retry.Sanitize(err))
	return "retry scheduled"
}

// validate runs the member pass from the checkpoint, batch by batch, then
// the dataset-level checks and the report.
func (s *Service) validate(ctx context.Context, owner string, job model.ValidationJob, checks []check, log *slog.Logger) (string, error) {
	var members, datasetLevel []check
	var content *check
	for i := range checks {
		switch {
		case checks[i].member != nil:
			members = append(members, checks[i])
		case checks[i].content != nil:
			content = &checks[i]
		default:
			datasetLevel = append(datasetLevel, checks[i])
		}
	}
	if job.RuleCounts == nil {
		job.RuleCounts = map[string]model.RuleCount{}
	}
	deadline := time.Now().Add(s.cfg.ValidationBudget)
	for {
		batch, err := s.deps.Repo.MemberEvidence(ctx, job.TenantID, job.DatasetVersionID, job.CheckpointOrdinal, s.cfg.ValidationBatch)
		if err != nil {
			return "", err
		}
		if len(batch) == 0 {
			break
		}
		b := s.evaluate(ctx, owner, job, batch, members, content)
		if err := s.deps.Repo.CommitValidationBatch(ctx, b); err != nil {
			return "", err
		}
		s.count("dataset_validation_members", b.Processed)
		s.count("dataset_validation_bytes_read", b.BytesRead)
		job.CheckpointOrdinal = b.To
		for id, c := range b.RuleCounts {
			total := job.RuleCounts[id]
			total.Findings += c.Findings
			job.RuleCounts[id] = total
		}
		cancel, err := s.deps.Repo.ExtendValidation(ctx, job.ID, owner, s.now(), s.cfg.Lease)
		if err != nil {
			return "", err
		}
		if cancel {
			if err := s.deps.Repo.StopValidation(context.WithoutCancel(ctx), job.ID, owner, s.now()); err != nil {
				return "", err
			}
			s.count("dataset_validations_cancelled", 1)
			return "validation cancelled", nil
		}
		if time.Now().After(deadline) {
			// Let go; the next poll resumes after the checkpoint.
			if err := s.deps.Repo.RetryValidation(ctx, job.ID, owner, s.now(), "", s.now()); err != nil {
				return "", err
			}
			return fmt.Sprintf("checked through member %d; continues", job.CheckpointOrdinal), nil
		}
	}
	// The counters as committed, not as this pass saw them.
	fresh, err := s.deps.Repo.GetValidation(ctx, job.TenantID, job.ID)
	if err != nil {
		return "", err
	}
	return s.finish(ctx, owner, fresh, checks, datasetLevel, content != nil, log)
}

// sampled says whether a SAMPLED run reads member ordinal: every step-th
// member in canonical order, so the sample is the same on every run of
// the same version and never larger than asked.
func sampled(job model.ValidationJob, ordinal int64) bool {
	switch job.Mode {
	case model.ModeFull:
		return true
	case model.ModeSampled:
		if job.SampleSize <= 0 {
			return false
		}
		step := (job.TotalItems + int64(job.SampleSize) - 1) / int64(job.SampleSize)
		return step <= 1 || (ordinal-1)%step == 0
	}
	return false
}

// contentResult is the content check of one member.
type contentResult struct {
	// examined: integrity was established, from evidence or by reading.
	examined bool
	read     bool
	bytes    int64
	issues   []issue
}

// evaluate applies the member checks to a batch and the content check to
// its sampled members, reading at most ContentConcurrency objects at a
// time while keeping the job's lease alive.
func (s *Service) evaluate(ctx context.Context, owner string, job model.ValidationJob, batch []model.MemberEvidence,
	members []check, content *check) interfaces.ValidationBatch {
	b := interfaces.ValidationBatch{JobID: job.ID, Owner: owner, From: job.CheckpointOrdinal, To: batch[len(batch)-1].Ordinal,
		RuleCounts: map[string]model.RuleCount{}, Now: s.now()}

	results := make([]*contentResult, len(batch))
	if content != nil {
		var todo []int
		for i, m := range batch {
			if sampled(job, m.Ordinal) {
				todo = append(todo, i)
			}
		}
		s.readContents(ctx, owner, job, batch, todo, content.content, results)
	}

	// add stores a check's findings until the rule has stored its
	// maximum; the failures are counted either way.
	add := func(c check, issues []issue) {
		count := b.RuleCounts[c.rule.ID]
		for _, is := range issues {
			if job.RuleCounts[c.rule.ID].Findings+count.Findings >= s.cfg.MaxFindingsPerRule {
				break
			}
			b.Findings = append(b.Findings, s.finding(job, c, is))
			count.Findings++
		}
		b.RuleCounts[c.rule.ID] = count
	}
	for i, m := range batch {
		worst := model.Severity("")
		for _, c := range members {
			issues := c.member(m)
			count := b.RuleCounts[c.rule.ID]
			count.Checked++
			if len(issues) > 0 {
				count.Failed++
				worst = worse(worst, c.rule.Severity)
			}
			b.RuleCounts[c.rule.ID] = count
			add(c, issues)
		}
		if content != nil {
			count := b.RuleCounts[content.rule.ID]
			r := results[i]
			switch {
			case r == nil || !r.examined:
				count.Skipped++
				b.Skipped++
			default:
				count.Checked++
				b.Examined++
				if r.read {
					count.Read++
				}
			}
			if r != nil {
				b.BytesRead += r.bytes
				if len(r.issues) > 0 {
					count.Failed++
					worst = worse(worst, content.rule.Severity)
				}
			}
			b.RuleCounts[content.rule.ID] = count
			if r != nil {
				add(*content, r.issues)
			}
		}
		b.Processed++
		b.BytesTotal += m.SizeBytes
		switch worst {
		case model.SeverityError:
			b.Failed++
		case model.SeverityWarning:
			b.Warnings++
		default:
			b.Passed++
		}
	}
	return b
}

func worse(a, b model.Severity) model.Severity {
	rank := map[model.Severity]int{"": 0, model.SeverityInfo: 1, model.SeverityWarning: 2, model.SeverityError: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// readContents runs the content check for the members at todo with a
// fixed pool of readers, renewing the job's lease while they work.
func (s *Service) readContents(ctx context.Context, owner string, job model.ValidationJob, batch []model.MemberEvidence,
	todo []int, rule *contentRule, results []*contentResult) {
	if len(todo) == 0 {
		return
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(s.cfg.Lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_, _ = s.deps.Repo.ExtendValidation(ctx, job.ID, owner, s.now(), s.cfg.Lease)
			}
		}
	}()

	next := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < min(s.cfg.ContentConcurrency, len(todo)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				r := s.checkContent(ctx, rule, batch[i])
				results[i] = &r
			}
		}()
	}
	for _, i := range todo {
		next <- i
	}
	close(next)
	wg.Wait()
}

// checkContent establishes one member's content integrity: from
// processing's verification of the same bytes when there is one (unless
// the rule asks to read again), otherwise by reading the bytes and
// comparing their SHA-256 and size with the snapshot's records. Versions
// that cannot be read are not examined; that is never a pass.
func (s *Service) checkContent(ctx context.Context, rule *contentRule, m model.MemberEvidence) contentResult {
	if m.AssetStatus != model.AssetStatusAvailable || m.ObjectKey == "" {
		return contentResult{}
	}
	if m.ChecksumSHA256 == "" {
		return contentResult{} // nothing to compare against; checksum_present reports it
	}
	if !rule.Reverify && m.VerificationStatus == "PASSED" && m.ChecksumSHA256 == m.AssetChecksum {
		return contentResult{examined: true}
	}
	if s.deps.Content == nil || s.deps.ContentBucket == "" {
		return contentResult{}
	}

	var r contentResult
	err := s.content.Do(ctx, func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, s.cfg.ObjectTimeout)
		defer cancel()
		body, err := s.deps.Content.OpenObject(ctx, s.deps.ContentBucket, m.ObjectKey)
		if err != nil {
			return err
		}
		defer body.Close()
		h := sha256.New()
		n, err := io.Copy(h, body)
		r.bytes = n
		if err != nil {
			return err
		}
		r.read, r.examined = true, true
		if sum := hex.EncodeToString(h.Sum(nil)); sum != m.ChecksumSHA256 {
			is := memberIssue(m, "CHECKSUM_MISMATCH", "the stored bytes do not match the recorded SHA-256")
			is.Expected, is.Observed = m.ChecksumSHA256, sum
			is.Remediation = "restore the object from a good copy, or remove the member"
			r.issues = append(r.issues, is)
		}
		if n != m.SizeBytes {
			is := memberIssue(m, "SIZE_MISMATCH", "the stored object's size differs from the recorded size")
			is.Key = m.ObjectID + "/size"
			is.Expected, is.Observed = fmt.Sprint(m.SizeBytes), fmt.Sprint(n)
			r.issues = append(r.issues, is)
		}
		return nil
	})
	if errors.Is(err, interfaces.ErrObjectNotFound) {
		is := memberIssue(m, "CONTENT_MISSING", "the version's bytes are missing from storage")
		is.Remediation = "restore the object, or remove the member"
		return contentResult{examined: true, issues: []issue{is}}
	}
	if err != nil {
		// Unreadable now (timeout, storage error): not examined.
		return contentResult{bytes: r.bytes}
	}
	return r
}

// finding turns an issue into a finding with an ID stable for the job:
// the same problem found again is the same row.
func (s *Service) finding(job model.ValidationJob, c check, is issue) model.Finding {
	sum := sha256.Sum256([]byte(job.ID + "\x00" + c.rule.ID + "\x00" + is.Code + "\x00" + is.Key))
	evidence := "catalog records"
	switch {
	case c.content != nil && strings.HasPrefix(is.Code, "CHECKSUM_MISMATCH"), is.Code == "SIZE_MISMATCH":
		evidence = "object bytes read by the validation"
	case c.content != nil:
		evidence = "object storage"
	case c.rule.Type == "checksum_verified", c.rule.Type == "malware_scan":
		evidence = "processing records"
	case c.dataset != nil:
		evidence = "aggregates of the dataset version"
	}
	return model.Finding{
		ID: hex.EncodeToString(sum[:16]), JobID: job.ID, TenantID: job.TenantID, RuleID: c.rule.ID, RuleVersion: c.rule.Version,
		Severity: c.rule.Severity, Code: is.Code, ObjectID: is.ObjectID, AssetID: is.AssetID, Message: is.Message,
		Expected: clip(is.Expected), Observed: clip(is.Observed), Evidence: evidence, Remediation: is.Remediation, CreatedAt: s.now(),
	}
}

// ─── Reports ──────────────────────────────────────────────────────────

// Check statuses in a report.
const (
	CheckPassed      = "PASSED"
	CheckFailed      = "FAILED"
	CheckNotAssessed = "NOT_ASSESSED"
	// CheckPartial: passed where assessed, but only part of the version was
	// (a sample, or members that could not be read).
	CheckPartial = "PARTIAL"
)

type checkResult struct {
	RuleID         string `json:"rule_id"`
	RuleVersion    int    `json:"rule_version"`
	Type           string `json:"type"`
	Severity       string `json:"severity"`
	Scope          string `json:"scope"`
	Dimension      string `json:"dimension"`
	Status         string `json:"status"`
	Checked        int64  `json:"checked"`
	Failed         int64  `json:"failed"`
	NotAssessed    int64  `json:"not_assessed"`
	Read           int64  `json:"read,omitempty"`
	FindingsStored int64  `json:"findings_stored"`
	Note           string `json:"note,omitempty"`
}

type reportBody struct {
	SchemaVersion    int    `json:"schema_version"`
	ReportID         string `json:"report_id"`
	JobID            string `json:"job_id"`
	DatasetID        string `json:"dataset_id"`
	DatasetVersionID string `json:"dataset_version_id"`
	DatasetVersion   int    `json:"dataset_version"`
	ManifestSHA256   string `json:"manifest_sha256"`
	Profile          struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
		Kind    string `json:"kind"`
	} `json:"profile"`
	Thresholds  model.Thresholds `json:"thresholds"`
	Mode        string           `json:"mode"`
	SampleSize  int              `json:"sample_size,omitempty"`
	Status      string           `json:"status"`
	StartedAt   time.Time        `json:"started_at"`
	CompletedAt time.Time        `json:"completed_at"`
	Members     struct {
		Total    int64 `json:"total"`
		Examined int64 `json:"examined"`
		Passed   int64 `json:"passed"`
		Warnings int64 `json:"warnings"`
		Failed   int64 `json:"failed"`
	} `json:"members"`
	Content struct {
		Coverage         string `json:"coverage"`
		Examined         int64  `json:"examined"`
		Read             int64  `json:"read"`
		NotExamined      int64  `json:"not_examined"`
		BytesRepresented int64  `json:"bytes_represented"`
		BytesRead        int64  `json:"bytes_read"`
		Note             string `json:"note"`
	} `json:"content"`
	Checks struct {
		Passed      int `json:"passed"`
		Failed      int `json:"failed"`
		Partial     int `json:"partial"`
		NotAssessed int `json:"not_assessed"`
	} `json:"checks"`
	Results  []checkResult `json:"results"`
	Findings struct {
		BySeverity map[string]int64 `json:"by_severity"`
		ByRule     map[string]int64 `json:"by_rule"`
		Stored     int64            `json:"stored"`
	} `json:"findings"`
	Evidence struct {
		VerifiedChecksums int64         `json:"verified_checksums"`
		Verification      []model.Count `json:"checksum_verification"`
		MalwareScans      []model.Count `json:"malware_scans"`
		VersionStates     []model.Count `json:"version_states"`
	} `json:"evidence"`
	Distributions struct {
		Splits       []model.Count `json:"splits"`
		Labels       []model.Count `json:"labels"`
		LabelCount   int64         `json:"distinct_labels"`
		Unlabeled    int64         `json:"unlabeled"`
		ContentTypes []model.Count `json:"content_types"`
		Extensions   []model.Count `json:"extensions"`
		Sizes        struct {
			Total int64 `json:"total_bytes"`
			Min   int64 `json:"min_bytes"`
			Max   int64 `json:"max_bytes"`
		} `json:"sizes"`
	} `json:"distributions"`
	Readiness *readinessReport `json:"readiness,omitempty"`
	Summary   string           `json:"summary"`
}

// finish runs the dataset-level checks on the version's aggregates and
// writes the report, the job's end, and the outcome, together.
func (s *Service) finish(ctx context.Context, owner string, job model.ValidationJob, checks, datasetLevel []check, hasContent bool, log *slog.Logger) (string, error) {
	started := time.Now()
	agg, err := s.deps.Repo.Aggregates(ctx, job.TenantID, job.DatasetVersionID, s.cfg.DistributionTop)
	if err != nil {
		return "", err
	}
	v, err := s.deps.Repo.GetVersion(ctx, job.TenantID, job.DatasetVersionID)
	if err != nil {
		return "", err
	}

	var findings []model.Finding
	notes := map[string]string{}
	failures := map[string]int64{}
	for _, c := range datasetLevel {
		issues, note := c.dataset(agg)
		notes[c.rule.ID] = note
		failures[c.rule.ID] = int64(len(issues))
		for i, is := range issues {
			if int64(i) >= s.cfg.MaxFindingsPerRule {
				break
			}
			findings = append(findings, s.finding(job, c, is))
		}
	}

	var body reportBody
	reportID := event.NewID()
	now := s.now()
	body.SchemaVersion, body.ReportID, body.JobID = model.ReportSchemaVersion, reportID, job.ID
	body.DatasetID, body.DatasetVersionID, body.DatasetVersion, body.ManifestSHA256 = job.DatasetID, v.ID, v.Version, v.ManifestSHA256
	body.Profile.ID, body.Profile.Version, body.Profile.Kind = job.ProfileID, job.ProfileVersion, job.Profile.Kind
	body.Thresholds, body.Mode, body.SampleSize = job.Profile.Thresholds, string(job.Mode), job.SampleSize
	body.StartedAt, body.CompletedAt = *job.StartedAt, now
	body.Members.Total, body.Members.Examined = v.MemberCount, job.ProcessedItems
	body.Members.Passed, body.Members.Warnings, body.Members.Failed = job.PassedItems, job.WarningItems, job.FailedItems
	body.Content.Examined, body.Content.NotExamined = job.ContentExamined, job.ContentSkipped
	body.Content.BytesRepresented, body.Content.BytesRead = job.BytesTotal, job.BytesRead
	body.Findings.BySeverity, body.Findings.ByRule = map[string]int64{}, map[string]int64{}

	var errorCount, warningCount int64
	for _, c := range checks {
		r := checkResult{RuleID: c.rule.ID, RuleVersion: c.rule.Version, Type: c.rule.Type, Severity: string(c.rule.Severity),
			Scope: string(c.rule.Scope), Dimension: c.dimension}
		count := job.RuleCounts[c.rule.ID]
		switch {
		case c.dataset != nil:
			r.Failed, r.Note = failures[c.rule.ID], notes[c.rule.ID]
			r.Checked = 1
			if strings.HasPrefix(r.Note, notAssessedPrefix) && r.Failed == 0 {
				r.Checked, r.NotAssessed = 0, 1
			}
			r.FindingsStored = min(r.Failed, s.cfg.MaxFindingsPerRule)
		default:
			r.Checked, r.Failed, r.NotAssessed, r.Read, r.FindingsStored = count.Checked, count.Failed, count.Skipped, count.Read, count.Findings
		}
		if c.content != nil {
			r.Note = s.contentNote(job, hasContent, count)
			body.Content.Read = count.Read
		}
		switch {
		case r.Failed > 0:
			r.Status = CheckFailed
			body.Checks.Failed++
		case r.Checked == 0:
			r.Status = CheckNotAssessed
			body.Checks.NotAssessed++
		case r.NotAssessed > 0 || (c.content != nil && job.Mode == model.ModeSampled):
			r.Status = CheckPartial
			body.Checks.Partial++
		default:
			r.Status = CheckPassed
			body.Checks.Passed++
		}
		body.Results = append(body.Results, r)
		if r.Failed > 0 {
			body.Findings.BySeverity[string(c.rule.Severity)] += r.Failed
			body.Findings.ByRule[c.rule.ID] = r.Failed
			body.Findings.Stored += r.FindingsStored
		}
		switch c.rule.Severity {
		case model.SeverityError:
			errorCount += r.Failed
		case model.SeverityWarning:
			warningCount += r.Failed
		}
	}
	body.Content.Coverage = map[model.ValidationMode]string{model.ModeMetadata: "NONE", model.ModeFull: "FULL", model.ModeSampled: "SAMPLED"}[job.Mode]
	if !hasContent {
		body.Content.Coverage = "NONE"
	}
	body.Content.Note = s.contentNote(job, hasContent, job.RuleCounts[contentRuleID(checks)])

	body.Evidence.VerifiedChecksums, body.Evidence.Verification = agg.VerifiedChecksums, agg.Verification
	body.Evidence.MalwareScans, body.Evidence.VersionStates = agg.ScanResults, agg.AssetStatuses
	d := &body.Distributions
	d.Splits, d.Labels, d.LabelCount, d.Unlabeled = agg.Splits, agg.Labels, agg.LabelCount, agg.Unlabeled
	d.ContentTypes, d.Extensions = agg.ContentTypes, agg.Extensions
	d.Sizes.Total, d.Sizes.Min, d.Sizes.Max = agg.TotalBytes, agg.MinSize, agg.MaxSize

	outcome := job.Profile.Thresholds.Outcome(errorCount, warningCount)
	body.Status = string(outcome)
	if job.Readiness {
		body.Readiness = assess(body.Results, job, v, agg)
	}
	body.Summary = summarize(body)
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	report := model.ValidationReport{
		ID: reportID, TenantID: job.TenantID, JobID: job.ID, DatasetID: job.DatasetID, DatasetVersionID: v.ID,
		ProfileID: job.ProfileID, ProfileVersion: job.ProfileVersion, Mode: job.Mode, Status: outcome, Readiness: job.Readiness,
		SchemaVersion: model.ReportSchemaVersion, Body: raw, Summary: body.Summary, StartedAt: *job.StartedAt, CompletedAt: now,
	}
	if err := s.deps.Repo.CompleteValidation(ctx, job, owner, report, findings); err != nil {
		return "", err
	}
	s.count("dataset_validations_"+strings.ToLower(string(outcome)), 1)
	for severity, n := range body.Findings.BySeverity {
		s.count("dataset_findings_"+strings.ToLower(severity), n)
	}
	s.count("dataset_validation_ms_total", now.Sub(*job.StartedAt).Milliseconds())
	if job.Readiness {
		s.count("dataset_readiness_assessments", 1)
		s.count("dataset_readiness_ms_total", now.Sub(*job.StartedAt).Milliseconds())
	}
	log.Info("validation completed", "status", outcome, "members", body.Members.Total, "errors", errorCount,
		"warnings", warningCount, "finish_ms", time.Since(started).Milliseconds())
	return "validation " + strings.ToLower(string(outcome)), nil
}

func contentRuleID(checks []check) string {
	for _, c := range checks {
		if c.content != nil {
			return c.rule.ID
		}
	}
	return ""
}

// contentNote says how much of the version's content was examined, and
// never calls a sample a full scan.
func (s *Service) contentNote(job model.ValidationJob, hasContent bool, count model.RuleCount) string {
	switch {
	case !hasContent:
		return "the profile has no content check"
	case job.Mode == model.ModeMetadata:
		return "metadata-only validation: no object content was examined"
	case job.Mode == model.ModeSampled:
		return fmt.Sprintf("sampled: content examined for %d members of a deterministic sample of at most %d of %d; this is not a full integrity scan",
			count.Checked, job.SampleSize, job.TotalItems)
	case count.Skipped > 0:
		return fmt.Sprintf("full: content examined for %d of %d members; %d could not be examined", count.Checked, job.TotalItems, count.Skipped)
	}
	return fmt.Sprintf("full: content examined for all %d members (%d read, the rest by processing's verification of the same bytes)",
		count.Checked, count.Read)
}

func summarize(b reportBody) string {
	summary := fmt.Sprintf("%s: %d members; %d passed, %d with warnings, %d failed. Checks: %d passed, %d failed, %d partial, %d not assessed. Content: %s.",
		b.Status, b.Members.Total, b.Members.Passed, b.Members.Warnings, b.Members.Failed,
		b.Checks.Passed, b.Checks.Failed, b.Checks.Partial, b.Checks.NotAssessed, b.Content.Note)
	return summary
}

// ─── Recovery and gauges ──────────────────────────────────────────────

// ReleaseStuck returns datasets left VALIDATING with no unfinished
// validation to DRAFT.
func (s *Service) ReleaseStuck(ctx context.Context) (int64, error) {
	n, err := s.deps.Repo.ReleaseStuckDatasets(ctx, s.now())
	if n > 0 {
		s.count("dataset_stuck_released", n)
		s.deps.Log.Warn("released datasets stuck in VALIDATING", "datasets", n)
	}
	return n, err
}

// Gauges publishes the dataset jobs by state, the age of the oldest due
// one and how many running ones lost their worker.
func (s *Service) Gauges(ctx context.Context) {
	if s.deps.Metrics == nil {
		return
	}
	stats, err := s.deps.Repo.JobStats(ctx, s.now())
	if err != nil {
		s.deps.Log.Error("dataset job gauges failed", "error", retry.Sanitize(err))
		return
	}
	for _, state := range []model.ImportState{model.ImportAwaitingUpload, model.ImportQueued, model.ImportRunning,
		model.ImportCompleted, model.ImportFailed, model.ImportCancelled} {
		s.deps.Metrics.Set("dataset_imports_state_"+strings.ToLower(string(state)), stats.ImportsByState[string(state)])
	}
	for _, state := range []model.ValidationState{model.ValidationQueued, model.ValidationRunning, model.ValidationCompleted,
		model.ValidationCompletedWithWarnings, model.ValidationFailed, model.ValidationCancelled} {
		s.deps.Metrics.Set("dataset_validations_state_"+strings.ToLower(string(state)), stats.ValidationsByState[string(state)])
	}
	s.deps.Metrics.Set("dataset_jobs_oldest_due_seconds", int64(stats.OldestQueuedSeconds))
	s.deps.Metrics.Set("dataset_jobs_stuck", stats.Stuck)
}
