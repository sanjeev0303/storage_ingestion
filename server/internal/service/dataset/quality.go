package dataset

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Data quality (docs/quality.md): column profiles per version, drift
// against the parent, leakage between training and evaluation versions,
// and release labels.

// QualityStore keeps profiles, leakage checks, settings and releases.
type QualityStore interface {
	Settings(ctx context.Context, tenantID, datasetID string) (model.QualitySettings, error)
	PutSettings(ctx context.Context, s model.QualitySettings) error
	Profile(ctx context.Context, tenantID, versionID string) (model.VersionProfile, bool, error)
	QueueProfile(ctx context.Context, tenantID, datasetID, versionID string) error
	ClaimProfile(ctx context.Context, owner string, lease time.Duration, maxAttempts int) (model.VersionProfile, bool, error)
	ExtendProfile(ctx context.Context, versionID, owner string, lease time.Duration) error
	FinishProfile(ctx context.Context, owner string, p model.VersionProfile) error
	Baseline(ctx context.Context, tenantID, versionID string) (string, error)
	MetadataValues(ctx context.Context, tenantID, versionID, key string, top int) ([]model.Count, int64, int64, error)
	ScanMembers(ctx context.Context, tenantID, versionID, key string, each func(model.QualityMember) error) error
	HashOverlap(ctx context.Context, tenantID, trainID, evalID string, limit int) (int64, int64, int64, []model.LeakageExample, error)
	AddLeakage(ctx context.Context, c model.LeakageCheck) error
	Leakage(ctx context.Context, tenantID, runID, versionID string, limit int) ([]model.LeakageCheck, error)
	RunInputs(ctx context.Context, tenantID, runID string) ([]model.RunInput, error)
	RunsToCheck(ctx context.Context, limit int) ([][2]string, error)
	SetRelease(ctx context.Context, tenantID, versionID, label, notes, actor string, at time.Time) error
	Releases(ctx context.Context, tenantID, datasetID string) ([]model.Release, error)
}

// Profiling limits. A file or version past them is listed as skipped,
// with the reason, never silently left out.
var (
	profileMaxFileBytes    int64 = 4 << 30
	profileMaxVersionBytes int64 = 50 << 30
	profileMaxRows         int64 = 50_000_000
	profileLease                 = 2 * time.Minute
	profileAttempts              = 5
	leakageExamples              = 20
)

var ErrQualityOff = errors.New("data quality is not configured")

func (s *Service) quality() (QualityStore, error) {
	if s.deps.Quality == nil {
		return nil, ErrQualityOff
	}
	return s.deps.Quality, nil
}

// ─── Settings ─────────────────────────────────────────────────────────

func (s *Service) QualitySettings(ctx context.Context, p model.Principal, datasetID string) (model.QualitySettings, error) {
	q, err := s.quality()
	if err != nil {
		return model.QualitySettings{}, err
	}
	if _, err := s.Get(ctx, p, datasetID); err != nil {
		return model.QualitySettings{}, err
	}
	return q.Settings(ctx, p.TenantID, datasetID)
}

func (s *Service) PutQualitySettings(ctx context.Context, p model.Principal, in model.QualitySettings) (model.QualitySettings, error) {
	q, err := s.quality()
	if err != nil {
		return in, err
	}
	if err := s.authorize(ctx, p, model.PermDatasetsManage, "dataset.quality_settings", "dataset", in.DatasetID); err != nil {
		return in, err
	}
	if _, err := s.deps.Repo.GetDataset(ctx, p.TenantID, in.DatasetID); err != nil {
		return in, err
	}
	in.SubjectKey, in.SubjectPattern = strings.TrimSpace(in.SubjectKey), strings.TrimSpace(in.SubjectPattern)
	switch {
	case in.MaxPSI <= 0 || in.MaxPSI > 10:
		return in, fmt.Errorf("%w: max_psi is above 0 and at most 10", ErrInvalidRequest)
	case in.MaxNullChange <= 0 || in.MaxNullChange > 1:
		return in, fmt.Errorf("%w: max_null_change is above 0 and at most 1", ErrInvalidRequest)
	case in.MaxRowChange <= 0 || in.MaxRowChange > 100:
		return in, fmt.Errorf("%w: max_row_change is above 0 and at most 100", ErrInvalidRequest)
	case in.SubjectKey != "" && in.SubjectPattern != "":
		return in, fmt.Errorf("%w: name the subject by subject_key or subject_pattern, not both", ErrInvalidRequest)
	case len(in.SubjectKey) > 128 || hasControl(in.SubjectKey):
		return in, fmt.Errorf("%w: subject_key is a metadata key of at most 128 bytes", ErrInvalidRequest)
	}
	if in.SubjectPattern != "" {
		if _, err := subjectRegexp(in.SubjectPattern); err != nil {
			return in, err
		}
	}
	in.TenantID, in.UpdatedBy, in.UpdatedAt = p.TenantID, p.UserID, s.now()
	if err := q.PutSettings(ctx, in); err != nil {
		return in, err
	}
	s.record(ctx, p, "dataset.quality_settings", "dataset", in.DatasetID, map[string]any{
		"profile_snapshots": in.ProfileSnapshots, "max_psi": in.MaxPSI, "max_null_change": in.MaxNullChange,
		"max_row_change": in.MaxRowChange, "subject_key": in.SubjectKey, "subject_pattern": in.SubjectPattern})
	return in, nil
}

// subjectRegexp compiles a pattern whose first group is the subject, as
// in ^patients/([^/]+)/.
func subjectRegexp(pattern string) (*regexp.Regexp, error) {
	if len(pattern) > 500 {
		return nil, fmt.Errorf("%w: subject_pattern is at most 500 characters", ErrInvalidRequest)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("%w: subject_pattern: %v", ErrInvalidRequest, err)
	}
	if re.NumSubexp() < 1 {
		return nil, fmt.Errorf("%w: subject_pattern needs a group around the subject, as in ^patients/([^/]+)/", ErrInvalidRequest)
	}
	return re, nil
}

// subjectOf reads a member's subject; "" when it has none.
func subjectOf(m model.QualityMember, re *regexp.Regexp) string {
	if re == nil {
		return m.Subject
	}
	if g := re.FindStringSubmatch(m.Path); len(g) > 1 {
		return g[1]
	}
	return ""
}

// ─── Profiles and drift ───────────────────────────────────────────────

// ProfileNone is the state of a version that was never profiled.
const ProfileNone = "NONE"

func (s *Service) VersionProfile(ctx context.Context, p model.Principal, datasetID, versionID string) (model.VersionProfile, error) {
	q, err := s.quality()
	if err != nil {
		return model.VersionProfile{}, err
	}
	v, err := s.Version(ctx, p, datasetID, versionID)
	if err != nil {
		return model.VersionProfile{}, err
	}
	out, found, err := q.Profile(ctx, p.TenantID, v.ID)
	if err != nil {
		return out, err
	}
	if !found {
		return model.VersionProfile{VersionID: v.ID, DatasetID: v.DatasetID, State: ProfileNone,
			Tables: []model.TableProfile{}, Skipped: []model.SkippedFile{}}, nil
	}
	return out, nil
}

// ProfileVersion queues a version to be profiled again.
func (s *Service) ProfileVersion(ctx context.Context, p model.Principal, datasetID, versionID string) error {
	q, err := s.quality()
	if err != nil {
		return err
	}
	if err := s.authorize(ctx, p, model.PermDatasetsManage, "dataset.profile", "dataset_version", versionID); err != nil {
		return err
	}
	v, err := s.Version(ctx, p, datasetID, versionID)
	if err != nil {
		return err
	}
	if err := q.QueueProfile(ctx, p.TenantID, v.DatasetID, v.ID); err != nil {
		return err
	}
	s.record(ctx, p, "dataset.profile", "dataset_version", v.ID, nil)
	return nil
}

// Drift compares a version's profile with against's, or with its
// baseline (its parent, or the version before it) when against is "".
func (s *Service) Drift(ctx context.Context, p model.Principal, datasetID, versionID, against string) (model.DriftReport, error) {
	q, err := s.quality()
	if err != nil {
		return model.DriftReport{}, err
	}
	v, err := s.Version(ctx, p, datasetID, versionID)
	if err != nil {
		return model.DriftReport{}, err
	}
	if against == "" {
		if against, err = q.Baseline(ctx, p.TenantID, v.ID); err != nil {
			return model.DriftReport{}, err
		}
		if against == "" {
			return model.DriftReport{}, fmt.Errorf("%w: the first version has nothing before it to compare with", ErrInvalidRequest)
		}
	}
	if _, err := s.Version(ctx, p, "", against); err != nil {
		return model.DriftReport{}, err
	}
	return s.drift(ctx, p.TenantID, v.DatasetID, against, v.ID)
}

func (s *Service) drift(ctx context.Context, tenantID, datasetID, baseID, versionID string) (model.DriftReport, error) {
	q := s.deps.Quality
	base, found, err := q.Profile(ctx, tenantID, baseID)
	if err != nil {
		return model.DriftReport{}, err
	}
	if !found || base.State != model.ProfileDone {
		return model.DriftReport{}, fmt.Errorf("%w: the baseline version %s", model.ErrProfileNotReady, baseID)
	}
	cur, found, err := q.Profile(ctx, tenantID, versionID)
	if err != nil {
		return model.DriftReport{}, err
	}
	if !found || cur.State != model.ProfileDone {
		return model.DriftReport{}, fmt.Errorf("%w: version %s", model.ErrProfileNotReady, versionID)
	}
	set, err := q.Settings(ctx, tenantID, datasetID)
	if err != nil {
		return model.DriftReport{}, err
	}
	return compareProfiles(baseID, versionID, base.Tables, cur.Tables, limitsOf(set)), nil
}

func limitsOf(s model.QualitySettings) model.DriftLimits {
	return model.DriftLimits{MaxPSI: s.MaxPSI, MaxNullChange: s.MaxNullChange, MaxRowChange: s.MaxRowChange}
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += int64(n)
	return n, err
}

// ProfileOnce profiles the oldest queued version, if any, and compares it
// with its baseline. Run by the dataset worker.
func (s *Service) ProfileOnce(ctx context.Context, owner string) (bool, error) {
	q, err := s.quality()
	if err != nil {
		return false, err
	}
	job, ok, err := q.ClaimProfile(ctx, owner, profileLease, profileAttempts)
	if err != nil || !ok {
		return false, err
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(profileLease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_ = q.ExtendProfile(ctx, job.VersionID, owner, profileLease)
			}
		}
	}()

	out, err := s.profileVersion(ctx, job)
	if err != nil {
		// The lease runs out and another pass tries again.
		return true, fmt.Errorf("profile version %s: %w", job.VersionID, err)
	}
	baseline, err := q.Baseline(ctx, job.TenantID, job.VersionID)
	if err != nil {
		return true, err
	}
	out.BaselineVersionID = baseline
	if baseline != "" {
		if base, found, err := q.Profile(ctx, job.TenantID, baseline); err == nil && found && base.State == model.ProfileDone {
			set, err := q.Settings(ctx, job.TenantID, job.DatasetID)
			if err != nil {
				return true, err
			}
			report := compareProfiles(baseline, job.VersionID, base.Tables, out.Tables, limitsOf(set))
			out.Drift, out.Drifted = &report, report.Breaches > 0
		}
	}
	out.State = model.ProfileDone
	if err := q.FinishProfile(ctx, owner, out); err != nil {
		return true, err
	}
	s.deps.Log.Info("version profiled", "version_id", job.VersionID, "tables", len(out.Tables), "files", out.FilesProfiled,
		"skipped", len(out.Skipped), "drifted", out.Drifted)
	return true, nil
}

func (s *Service) profileVersion(ctx context.Context, job model.VersionProfile) (model.VersionProfile, error) {
	out := job
	out.Tables, out.Skipped = []model.TableProfile{}, []model.SkippedFile{}
	tables := map[string]*tableStats{}
	var order []*tableStats
	names := map[string]string{} // table name -> format, to tell formats in one folder apart
	skip := func(p, reason string) {
		out.Skipped = append(out.Skipped, model.SkippedFile{Path: p, Reason: reason})
	}
	after := int64(0)
	for {
		batch, err := s.deps.Repo.MemberEvidence(ctx, job.TenantID, job.VersionID, after, 500)
		if err != nil {
			return out, err
		}
		for _, m := range batch {
			after = m.Ordinal
			format, gz := tableFormat(m.Path, m.ContentType)
			if format == "" {
				out.OtherFiles++
				continue
			}
			switch {
			case m.Availability != "" && m.Availability != model.MemberAvailable:
				skip(m.Path, "its bytes are "+strings.ToLower(strings.ReplaceAll(m.Availability, "_", " ")))
				continue
			case m.AssetStatus != model.AssetStatusAvailable || m.ObjectKey == "":
				skip(m.Path, "the object is not available")
				continue
			case m.SizeBytes > profileMaxFileBytes:
				skip(m.Path, fmt.Sprintf("larger than the %d GiB a file may be", profileMaxFileBytes>>30))
				continue
			case out.BytesRead+m.SizeBytes > profileMaxVersionBytes:
				skip(m.Path, fmt.Sprintf("the version's %d GiB profiling budget was spent", profileMaxVersionBytes>>30))
				continue
			}
			name := tableName(m.Path)
			if f, ok := names[name]; ok && f != format {
				name += " (" + format + ")"
			}
			key := name + "\x00" + format
			t := tables[key]
			if t == nil {
				if len(order) >= profileTables {
					skip(m.Path, fmt.Sprintf("the version has more than %d tables", profileTables))
					continue
				}
				names[name] = format
				t = newTable(name, format, job.VersionID, profileMaxRows)
				tables[key] = t
				order = append(order, t)
			}
			if t.partial {
				skip(m.Path, fmt.Sprintf("its table reached %d rows", profileMaxRows))
				continue
			}
			src, err := s.memberSource(ctx, job.TenantID, m)
			if err != nil {
				skip(m.Path, "could not be read: "+err.Error())
				continue
			}
			body, err := src.open(ctx)
			if err != nil {
				skip(m.Path, "could not be read: "+err.Error())
				continue
			}
			cr := &countingReader{r: body}
			err = profileStream(ctx, cr, format, gz, t, "")
			body.Close()
			out.BytesRead += cr.n
			if err != nil {
				skip(m.Path, "not read as "+format+": "+err.Error())
				continue
			}
			out.FilesProfiled++
		}
		if len(batch) < 500 {
			break
		}
	}
	for _, t := range order {
		if t.files > 0 {
			out.Tables = append(out.Tables, t.profile())
		}
	}
	return out, nil
}

// ─── Leakage ──────────────────────────────────────────────────────────

// LeakageReport is every check of one evaluation version against one
// training version, and what was not checked.
type LeakageReport struct {
	TrainVersionID string               `json:"train_version_id"`
	EvalVersionID  string               `json:"eval_version_id"`
	Checks         []model.LeakageCheck `json:"checks"`
	NotChecked     []string             `json:"not_checked"`
}

// CheckLeakage compares an evaluation version with a training version:
// exact content by SHA-256, and subjects (patients, users, scenes) by the
// evaluation dataset's subject key or pattern.
func (s *Service) CheckLeakage(ctx context.Context, p model.Principal, evalDatasetID, evalID, trainID string) (LeakageReport, error) {
	if _, err := s.quality(); err != nil {
		return LeakageReport{}, err
	}
	ev, err := s.Version(ctx, p, evalDatasetID, evalID)
	if err != nil {
		return LeakageReport{}, err
	}
	tv, err := s.Version(ctx, p, "", trainID)
	if err != nil {
		return LeakageReport{}, err
	}
	if ev.ID == tv.ID {
		return LeakageReport{}, fmt.Errorf("%w: compare two different versions", ErrInvalidRequest)
	}
	return s.leakage(ctx, p.TenantID, p.UserID, "", tv, ev)
}

func (s *Service) leakage(ctx context.Context, tenantID, actor, runID string, train, eval model.DatasetVersion) (LeakageReport, error) {
	q := s.deps.Quality
	now := s.now()
	out := LeakageReport{TrainVersionID: train.ID, EvalVersionID: eval.ID, NotChecked: []string{}}
	base := model.LeakageCheck{TenantID: tenantID, RunID: runID, TrainVersionID: train.ID, EvalVersionID: eval.ID, CreatedBy: actor, CreatedAt: now}

	overlap, members, unhashed, examples, err := q.HashOverlap(ctx, tenantID, train.ID, eval.ID, leakageExamples)
	if err != nil {
		return out, err
	}
	exact := base
	exact.ID, exact.Method, exact.Overlap, exact.EvalMembers, exact.Examples = event.NewID(), model.LeakExactHash, overlap, members, examples
	exact.State = model.LeakClean
	if overlap > 0 {
		exact.State = model.LeakLeaked
	}
	exact.Detail = fmt.Sprintf("%d of %d evaluation files have the same SHA-256 as a training file", overlap, members)
	if unhashed > 0 {
		exact.Detail += fmt.Sprintf("; %d have no checksum and were not compared", unhashed)
	}
	exact.Parameters = map[string]any{}
	out.Checks = append(out.Checks, exact)

	subject, err := s.subjectLeakage(ctx, base, eval.DatasetID)
	if err != nil {
		return out, err
	}
	out.Checks = append(out.Checks, subject)
	if subject.State == model.LeakNotChecked {
		out.NotChecked = append(out.NotChecked, "subject leakage: "+subject.Detail)
	}

	ngram, err := q.Leakage(ctx, tenantID, "", eval.ID, 100)
	if err != nil {
		return out, err
	}
	hasNgram := false
	for _, c := range ngram {
		if c.Method == model.LeakNgram && c.TrainVersionID == train.ID {
			hasNgram = true
		}
	}
	if !hasNgram {
		out.NotChecked = append(out.NotChecked, "near-duplicate contamination: only exact hashes and subjects were compared; "+
			"run osp leakage --ngram to compare text by n-grams")
	}
	for _, c := range out.Checks {
		if err := q.AddLeakage(ctx, c); err != nil {
			return out, err
		}
	}
	return out, nil
}

func (s *Service) subjectLeakage(ctx context.Context, base model.LeakageCheck, evalDatasetID string) (model.LeakageCheck, error) {
	q := s.deps.Quality
	c := base
	c.ID, c.Method, c.Examples, c.Parameters = event.NewID(), model.LeakSubject, []model.LeakageExample{}, map[string]any{}
	set, err := q.Settings(ctx, base.TenantID, evalDatasetID)
	if err != nil {
		return c, err
	}
	if set.SubjectKey == "" && set.SubjectPattern == "" {
		c.State, c.Detail = model.LeakNotChecked, "the evaluation dataset names no subject key or pattern"
		return c, nil
	}
	var re *regexp.Regexp
	if set.SubjectPattern != "" {
		if re, err = subjectRegexp(set.SubjectPattern); err != nil {
			return c, err
		}
		c.Parameters["subject_pattern"] = set.SubjectPattern
	} else {
		c.Parameters["subject_key"] = set.SubjectKey
	}
	// ponytail: the training version's subjects are held in memory; a
	// version with tens of millions of distinct subjects would need a
	// join in the database instead.
	trained := map[string]string{}
	err = q.ScanMembers(ctx, base.TenantID, base.TrainVersionID, set.SubjectKey, func(m model.QualityMember) error {
		if sub := subjectOf(m, re); sub != "" {
			if _, ok := trained[sub]; !ok {
				trained[sub] = m.Path
			}
		}
		return nil
	})
	if err != nil {
		return c, err
	}
	var withSubject int64
	err = q.ScanMembers(ctx, base.TenantID, base.EvalVersionID, set.SubjectKey, func(m model.QualityMember) error {
		c.EvalMembers++
		sub := subjectOf(m, re)
		if sub == "" {
			return nil
		}
		withSubject++
		if path, ok := trained[sub]; ok {
			c.Overlap++
			if len(c.Examples) < leakageExamples {
				c.Examples = append(c.Examples, model.LeakageExample{EvalPath: m.Path, TrainPath: path, Key: sub})
			}
		}
		return nil
	})
	if err != nil {
		return c, err
	}
	c.State = model.LeakClean
	if c.Overlap > 0 {
		c.State = model.LeakLeaked
	}
	c.Detail = fmt.Sprintf("%d of %d evaluation files belong to a subject also in training", c.Overlap, c.EvalMembers)
	if missing := c.EvalMembers - withSubject; missing > 0 {
		c.Detail += fmt.Sprintf("; %d name no subject and were not compared", missing)
	}
	return c, nil
}

// RunLeakage compares each evaluation input of a run (VALIDATION, TEST)
// with each of its training inputs.
func (s *Service) RunLeakage(ctx context.Context, p model.Principal, runID string) ([]LeakageReport, error) {
	if _, err := s.quality(); err != nil {
		return nil, err
	}
	if _, err := s.TrainingRun(ctx, p, runID); err != nil {
		return nil, err
	}
	return s.runLeakage(ctx, p.TenantID, p.UserID, runID)
}

func (s *Service) runLeakage(ctx context.Context, tenantID, actor, runID string) ([]LeakageReport, error) {
	inputs, err := s.deps.Quality.RunInputs(ctx, tenantID, runID)
	if err != nil {
		return nil, err
	}
	out := []LeakageReport{}
	for _, t := range inputs {
		if t.Role != model.RoleTrain {
			continue
		}
		for _, e := range inputs {
			if e.Role != model.RoleValidation && e.Role != model.RoleTest {
				continue
			}
			train, err := s.deps.Repo.GetVersion(ctx, tenantID, t.VersionID)
			if err != nil {
				return out, err
			}
			eval, err := s.deps.Repo.GetVersion(ctx, tenantID, e.VersionID)
			if err != nil {
				return out, err
			}
			r, err := s.leakage(ctx, tenantID, actor, runID, train, eval)
			if err != nil {
				return out, err
			}
			out = append(out, r)
		}
	}
	return out, nil
}

// CheckRunsOnce compares the inputs of runs recorded with roles that have
// not been compared yet. Run by the dataset worker.
func (s *Service) CheckRunsOnce(ctx context.Context) (int, error) {
	q, err := s.quality()
	if err != nil {
		return 0, err
	}
	runs, err := q.RunsToCheck(ctx, 20)
	if err != nil {
		return 0, err
	}
	for _, r := range runs {
		if _, err := s.runLeakage(ctx, r[0], "system", r[1]); err != nil {
			return 0, fmt.Errorf("leakage of run %s: %w", r[1], err)
		}
	}
	return len(runs), nil
}

// LeakageChecks lists a run's checks, or those a version took part in.
func (s *Service) LeakageChecks(ctx context.Context, p model.Principal, runID, datasetID, versionID string) ([]model.LeakageCheck, error) {
	q, err := s.quality()
	if err != nil {
		return nil, err
	}
	if runID != "" {
		if _, err := s.TrainingRun(ctx, p, runID); err != nil {
			return nil, err
		}
	} else if _, err := s.Version(ctx, p, datasetID, versionID); err != nil {
		return nil, err
	}
	return q.Leakage(ctx, p.TenantID, runID, versionID, 200)
}

// NgramInput is an n-gram comparison made where the text is (the SDK).
type NgramInput struct {
	RunID          string
	TrainVersionID string
	EvalVersionID  string
	N              int
	Threshold      float64
	Overlap        int64
	EvalMembers    int64
	Examples       []model.LeakageExample
}

// RecordNgram records an n-gram comparison the caller made, as theirs.
func (s *Service) RecordNgram(ctx context.Context, p model.Principal, in NgramInput) (model.LeakageCheck, error) {
	q, err := s.quality()
	if err != nil {
		return model.LeakageCheck{}, err
	}
	if err := s.authorize(ctx, p, model.PermDatasetsWrite, "dataset.leakage_recorded", "dataset_version", in.EvalVersionID); err != nil {
		return model.LeakageCheck{}, err
	}
	if in.N < 3 || in.N > 50 || in.Threshold <= 0 || in.Threshold > 1 || in.Overlap < 0 || in.EvalMembers < in.Overlap || len(in.Examples) > 100 {
		return model.LeakageCheck{}, fmt.Errorf("%w: n 3 to 50, a threshold above 0 and at most 1, and 0 <= overlap <= eval_members", ErrInvalidRequest)
	}
	for _, id := range []string{in.TrainVersionID, in.EvalVersionID} {
		if _, err := s.Version(ctx, p, "", id); err != nil {
			return model.LeakageCheck{}, err
		}
	}
	if in.RunID != "" {
		if _, err := s.TrainingRun(ctx, p, in.RunID); err != nil {
			return model.LeakageCheck{}, err
		}
	}
	c := model.LeakageCheck{ID: event.NewID(), TenantID: p.TenantID, RunID: in.RunID, TrainVersionID: in.TrainVersionID,
		EvalVersionID: in.EvalVersionID, Method: model.LeakNgram, State: model.LeakClean, Overlap: in.Overlap, EvalMembers: in.EvalMembers,
		Examples: in.Examples, Parameters: map[string]any{"n": in.N, "threshold": in.Threshold}, CreatedBy: p.UserID, CreatedAt: s.now(),
		Detail: fmt.Sprintf("%d of %d evaluation texts share at least %.0f%% of their %d-grams with the training set (computed by the caller)",
			in.Overlap, in.EvalMembers, in.Threshold*100, in.N)}
	if c.Examples == nil {
		c.Examples = []model.LeakageExample{}
	}
	if in.Overlap > 0 {
		c.State = model.LeakLeaked
	}
	if err := q.AddLeakage(ctx, c); err != nil {
		return c, err
	}
	s.record(ctx, p, "dataset.leakage_recorded", "dataset_version", in.EvalVersionID, map[string]any{
		"train_version_id": in.TrainVersionID, "n": in.N, "overlap": in.Overlap})
	return c, nil
}

// ─── Releases ─────────────────────────────────────────────────────────

var releasePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,39}$`)

func (s *Service) Release(ctx context.Context, p model.Principal, datasetID, versionID, label, notes string) (model.Release, error) {
	q, err := s.quality()
	if err != nil {
		return model.Release{}, err
	}
	if err := s.authorize(ctx, p, model.PermDatasetsManage, "dataset.release", "dataset_version", versionID); err != nil {
		return model.Release{}, err
	}
	label = strings.TrimSpace(label)
	if !releasePattern.MatchString(label) || len(notes) > 4000 {
		return model.Release{}, fmt.Errorf("%w: a label of 1 to 40 letters, digits, '.', '_' or '-' (DR1, 2026.10), and notes of at most 4000 characters", ErrInvalidRequest)
	}
	v, err := s.Version(ctx, p, datasetID, versionID)
	if err != nil {
		return model.Release{}, err
	}
	if err := q.SetRelease(ctx, p.TenantID, v.ID, label, notes, p.UserID, s.now()); err != nil {
		return model.Release{}, err
	}
	s.record(ctx, p, "dataset.release", "dataset_version", v.ID, map[string]any{"label": label})
	rels, err := q.Releases(ctx, p.TenantID, v.DatasetID)
	if err != nil {
		return model.Release{}, err
	}
	for _, r := range rels {
		if r.VersionID == v.ID {
			return r, nil
		}
	}
	return model.Release{}, model.ErrNoRelease
}

func (s *Service) Releases(ctx context.Context, p model.Principal, datasetID string) ([]model.Release, error) {
	q, err := s.quality()
	if err != nil {
		return nil, err
	}
	if _, err := s.Get(ctx, p, datasetID); err != nil {
		return nil, err
	}
	return q.Releases(ctx, p.TenantID, datasetID)
}

// ReleaseDiff is a release against the one before it (or against).
type ReleaseDiff struct {
	Release  model.Release      `json:"release"`
	Previous model.Release      `json:"previous"`
	Members  model.VersionDiff  `json:"-"`
	Drift    *model.DriftReport `json:"drift,omitempty"`
	// DriftNote says why there is no drift.
	DriftNote string `json:"drift_note,omitempty"`
}

func (s *Service) DiffRelease(ctx context.Context, p model.Principal, datasetID, label, against string) (ReleaseDiff, error) {
	rels, err := s.Releases(ctx, p, datasetID)
	if err != nil {
		return ReleaseDiff{}, err
	}
	var out ReleaseDiff
	found := false
	for i, r := range rels {
		if r.Label != label {
			continue
		}
		out.Release, found = r, true
		if against == "" {
			if i == 0 {
				return out, fmt.Errorf("%w: %s is the first release", ErrInvalidRequest, label)
			}
			out.Previous = rels[i-1]
		}
	}
	if !found {
		return out, model.ErrNoRelease
	}
	if against != "" {
		for _, r := range rels {
			if r.Label == against {
				out.Previous = r
			}
		}
		if out.Previous.Label == "" {
			return out, fmt.Errorf("%w: %s", model.ErrNoRelease, against)
		}
	}
	if out.Members, err = s.DiffVersions(ctx, p, datasetID, out.Previous.VersionID, out.Release.VersionID, "", 50); err != nil {
		return out, err
	}
	d, err := s.drift(ctx, p.TenantID, datasetID, out.Previous.VersionID, out.Release.VersionID)
	switch {
	case errors.Is(err, model.ErrProfileNotReady):
		out.DriftNote = err.Error()
	case err != nil:
		return out, err
	default:
		out.Drift = &d
	}
	return out, nil
}
