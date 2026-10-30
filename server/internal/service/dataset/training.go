package dataset

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Training runs link dataset versions to the models trained on them. The
// SDK records them, and so do the MLflow and Weights & Biases helpers.

const (
	maxRunInputs      = 100
	maxRunParams      = 64
	maxRunMetrics     = 64
	maxRunKeyBytes    = 64
	maxRunValueBytes  = 512
	maxRunNameRunes   = 200
	maxRunExternalLen = 256
	maxRunURLLen      = 2048
)

// ModelInput names a model a run produced.
type ModelInput struct {
	Name    string
	Version string
	URI     string
}

// TrainingRunInput records a run.
type TrainingRunInput struct {
	Name       string
	Source     model.TrainingSource
	ExternalID string
	URL        string
	Status     model.TrainingStatus
	Params     map[string]string
	Metrics    map[string]float64
	VersionIDs []string
	// Roles maps a version to TRAIN, VALIDATION or TEST; runs with both
	// are checked for leakage.
	Roles map[string]string
	Model *ModelInput
}

// RecordTrainingRun records a run and the versions it trained on. A run
// sent again with the same source and external ID answers the recorded
// one, with created false.
func (s *Service) RecordTrainingRun(ctx context.Context, p model.Principal, in TrainingRunInput) (model.TrainingRun, bool, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsWrite, "training_run.create", "training_run", ""); err != nil {
		return model.TrainingRun{}, false, err
	}
	run, versions, err := checkRun(in)
	if err != nil {
		return model.TrainingRun{}, false, err
	}
	// A snapshot an erasure superseded is not trained on any more.
	for _, id := range versions {
		v, err := s.deps.Repo.GetVersion(ctx, p.TenantID, id)
		if err == nil && v.SupersededBy != "" {
			return model.TrainingRun{}, false, fmt.Errorf("%w: version %s (%s); its successor is %s", model.ErrVersionSuperseded, id,
				v.SupersededReason, v.SupersededBy)
		}
	}
	if s.gate != nil {
		if err := s.gate.BeforeRun(ctx, p.TenantID, versions); err != nil {
			return model.TrainingRun{}, false, err
		}
	}
	now := s.now()
	run.ID, run.CreatedBy, run.CreatedAt, run.UpdatedAt = event.NewID(), p.UserID, now, now
	if run.Status != model.RunRunning {
		run.FinishedAt = &now
	}
	produced, err := checkModel(in.Model, p.UserID, now)
	if err != nil {
		return model.TrainingRun{}, false, err
	}
	out, created, err := s.deps.Repo.CreateTrainingRun(ctx, p.TenantID, run, versions, produced)
	if err != nil {
		return model.TrainingRun{}, false, err
	}
	if created {
		s.record(ctx, p, "training_run.create", "training_run", out.ID, map[string]any{
			"name": out.Name, "source": string(out.Source), "versions": versions, "model": in.Model != nil,
		})
	}
	return out, created, nil
}

// UpdateTrainingRunInput finishes a run, adds metrics or records a model.
type UpdateTrainingRunInput struct {
	Status  model.TrainingStatus
	Metrics map[string]float64
	Model   *ModelInput
}

func (s *Service) UpdateTrainingRun(ctx context.Context, p model.Principal, runID string, in UpdateTrainingRunInput) (model.TrainingRun, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsWrite, "training_run.update", "training_run", runID); err != nil {
		return model.TrainingRun{}, err
	}
	switch in.Status {
	case "", model.RunRunning, model.RunFinished, model.RunFailed:
	default:
		return model.TrainingRun{}, fmt.Errorf("%w: status must be RUNNING, FINISHED or FAILED", ErrInvalidRequest)
	}
	if err := checkMetrics(in.Metrics); err != nil {
		return model.TrainingRun{}, err
	}
	now := s.now()
	produced, err := checkModel(in.Model, p.UserID, now)
	if err != nil {
		return model.TrainingRun{}, err
	}
	out, err := s.deps.Repo.UpdateTrainingRun(ctx, p.TenantID, runID, in.Status, in.Metrics, produced, now)
	if err != nil {
		return model.TrainingRun{}, err
	}
	s.record(ctx, p, "training_run.update", "training_run", runID, map[string]any{"status": string(out.Status), "model": in.Model != nil})
	return out, nil
}

func (s *Service) TrainingRun(ctx context.Context, p model.Principal, runID string) (model.TrainingRun, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsRead, "training_run.read", "training_run", runID); err != nil {
		return model.TrainingRun{}, err
	}
	return s.deps.Repo.GetTrainingRun(ctx, p.TenantID, runID)
}

// TrainingRuns lists runs newest first; with versionID, those that
// trained on that dataset version.
func (s *Service) TrainingRuns(ctx context.Context, p model.Principal, versionID, cursor string, limit int) (model.TrainingRunPage, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsRead, "training_run.read", "training_run", versionID); err != nil {
		return model.TrainingRunPage{}, err
	}
	size, err := s.pageSize(limit)
	if err != nil {
		return model.TrainingRunPage{}, err
	}
	return s.deps.Repo.ListTrainingRuns(ctx, p.TenantID, versionID, cursor, size)
}

// TrainingRunLineage walks the graph from a run: upstream to the data it
// trained on, downstream to the models it produced.
func (s *Service) TrainingRunLineage(ctx context.Context, p model.Principal, runID string, req LineageRequest) (model.LineagePage, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsRead, "dataset.lineage", "training_run", runID); err != nil {
		return model.LineagePage{}, err
	}
	if _, err := s.deps.Repo.GetTrainingRun(ctx, p.TenantID, runID); err != nil {
		return model.LineagePage{}, err
	}
	return s.lineage(ctx, p, model.LineageRef{Kind: model.NodeTrainingRun, RefID: runID}, req)
}

func checkRun(in TrainingRunInput) (model.TrainingRun, []string, error) {
	invalid := func(format string, args ...any) (model.TrainingRun, []string, error) {
		return model.TrainingRun{}, nil, fmt.Errorf("%w: "+format, append([]any{ErrInvalidRequest}, args...)...)
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len([]rune(name)) > maxRunNameRunes || hasControl(name) {
		return invalid("name is 1 to %d characters", maxRunNameRunes)
	}
	source := in.Source
	if source == "" {
		source = model.SourceSDK
	}
	switch source {
	case model.SourceSDK, model.SourceMLflow, model.SourceWandb, model.SourceOther:
	default:
		return invalid("source must be SDK, MLFLOW, WANDB or OTHER")
	}
	if len(in.ExternalID) > maxRunExternalLen || hasControl(in.ExternalID) {
		return invalid("external_id is at most %d characters", maxRunExternalLen)
	}
	if in.URL != "" {
		u, err := url.Parse(in.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(in.URL) > maxRunURLLen {
			return invalid("url must be an http or https address of at most %d characters", maxRunURLLen)
		}
	}
	status := in.Status
	if status == "" {
		status = model.RunRunning
	}
	switch status {
	case model.RunRunning, model.RunFinished, model.RunFailed:
	default:
		return invalid("status must be RUNNING, FINISHED or FAILED")
	}
	if len(in.Params) > maxRunParams {
		return invalid("at most %d params", maxRunParams)
	}
	for k, v := range in.Params {
		if k == "" || len(k) > maxRunKeyBytes || len(v) > maxRunValueBytes || hasControl(k) || hasControl(v) {
			return invalid("param keys are 1 to %d bytes and values at most %d", maxRunKeyBytes, maxRunValueBytes)
		}
	}
	if err := checkMetrics(in.Metrics); err != nil {
		return model.TrainingRun{}, nil, err
	}
	if len(in.VersionIDs) == 0 || len(in.VersionIDs) > maxRunInputs {
		return invalid("name 1 to %d dataset versions the run trained on", maxRunInputs)
	}
	seen := map[string]bool{}
	var versions []string
	for _, id := range in.VersionIDs {
		id = strings.TrimSpace(id)
		if id == "" || hasControl(id) {
			return invalid("a dataset version id is empty")
		}
		if !seen[id] {
			seen[id] = true
			versions = append(versions, id)
		}
	}
	roles := map[string]string{}
	for id, role := range in.Roles {
		role = strings.ToUpper(strings.TrimSpace(role))
		if !seen[strings.TrimSpace(id)] {
			return invalid("a role names version %q, which the run does not list", id)
		}
		if role != model.RoleTrain && role != model.RoleValidation && role != model.RoleTest {
			return invalid("a role is TRAIN, VALIDATION or TEST")
		}
		roles[strings.TrimSpace(id)] = role
	}
	return model.TrainingRun{
		Name: name, Source: source, ExternalID: in.ExternalID, URL: in.URL, Status: status,
		Params: orEmpty(in.Params), Metrics: orEmptyMetrics(in.Metrics), Roles: roles,
	}, versions, nil
}

func checkMetrics(metrics map[string]float64) error {
	if len(metrics) > maxRunMetrics {
		return fmt.Errorf("%w: at most %d metrics", ErrInvalidRequest, maxRunMetrics)
	}
	for k, v := range metrics {
		if k == "" || len(k) > maxRunKeyBytes || hasControl(k) || math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("%w: metric names are 1 to %d bytes and values finite numbers", ErrInvalidRequest, maxRunKeyBytes)
		}
	}
	return nil
}

func checkModel(in *ModelInput, actor string, now time.Time) (*model.Model, error) {
	if in == nil {
		return nil, nil
	}
	name, version := strings.TrimSpace(in.Name), strings.TrimSpace(in.Version)
	if name == "" || len([]rune(name)) > maxRunNameRunes || hasControl(name) {
		return nil, fmt.Errorf("%w: a model name is 1 to %d characters", ErrInvalidRequest, maxRunNameRunes)
	}
	if version == "" || len(version) > 64 || hasControl(version) {
		return nil, fmt.Errorf("%w: a model version is 1 to 64 characters", ErrInvalidRequest)
	}
	if len(in.URI) > maxRunURLLen || hasControl(in.URI) {
		return nil, fmt.Errorf("%w: a model uri is at most %d characters", ErrInvalidRequest, maxRunURLLen)
	}
	return &model.Model{ID: event.NewID(), Name: name, Version: version, URI: in.URI, CreatedBy: actor, CreatedAt: now}, nil
}

func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func orEmptyMetrics(m map[string]float64) map[string]float64 {
	if m == nil {
		return map[string]float64{}
	}
	return m
}
