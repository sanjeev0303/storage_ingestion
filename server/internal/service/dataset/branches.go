package dataset

import (
	"context"
	"fmt"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Branches and merges (docs/datasets.md, "Branches"). A branch is a
// dataset of its own with a parent: it starts with the members of one
// snapshot of the parent's family, and every dataset feature works on it.
// Branches are one level deep: a branch is never branched again, so the
// family is a parent and its branches.

// maxMergeConflicts bounds the conflicts a merge answer lists; all are
// counted.
const maxMergeConflicts = 1000

// root is the family a dataset belongs to: its parent, or itself.
func root(d model.Dataset) string {
	if d.ParentID != "" {
		return d.ParentID
	}
	return d.ID
}

// CreateBranch branches the dataset from one of its family's versions:
// fromVersionID, or the dataset's latest version when it is empty.
func (s *Service) CreateBranch(ctx context.Context, p model.Principal, datasetID, name, fromVersionID string) (model.Dataset, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsWrite, "dataset.branch_create", "dataset", datasetID); err != nil {
		return model.Dataset{}, err
	}
	if !model.ValidDataset(name) {
		return model.Dataset{}, fmt.Errorf("%w: a branch name is 1-64 characters of a-z, 0-9, '.', '_' or '-', starting with a letter or digit", ErrInvalidRequest)
	}
	parent, err := s.deps.Repo.GetDataset(ctx, p.TenantID, datasetID)
	if err != nil {
		return model.Dataset{}, err
	}
	if parent.ParentID != "" {
		return model.Dataset{}, fmt.Errorf("%w: branch the dataset this branch belongs to; branches are one level deep", ErrInvalidRequest)
	}
	if parent.Lifecycle == model.LifecycleArchived {
		return model.Dataset{}, model.ErrDatasetArchived
	}
	fork := fromVersionID
	if fork == "" {
		fork = parent.CurrentVersionID
	}
	if fork == "" {
		return model.Dataset{}, fmt.Errorf("%w: take a snapshot first: a branch starts from a version", ErrNoVersion)
	}
	v, err := s.deps.Repo.GetVersion(ctx, p.TenantID, fork)
	if err != nil {
		return model.Dataset{}, err
	}
	if ok, err := s.sameFamily(ctx, p.TenantID, parent.ID, v.DatasetID); err != nil || !ok {
		if err == nil {
			err = model.ErrDatasetVersionNotFound
		}
		return model.Dataset{}, err
	}
	branch, err := s.deps.Repo.CreateBranch(ctx, parent, name, fork, event.NewID(), p.UserID, s.now())
	if err != nil {
		return branch, err
	}
	s.record(ctx, p, "dataset.branch_create", "dataset", branch.ID, map[string]any{
		"parent_id": parent.ID, "branch": name, "from_version_id": fork, "members": branch.MemberCount})
	return branch, nil
}

// Branches lists a dataset's branches.
func (s *Service) Branches(ctx context.Context, p model.Principal, datasetID string) ([]model.Dataset, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsRead, "dataset.read", "dataset", datasetID); err != nil {
		return nil, err
	}
	if _, err := s.deps.Repo.GetDataset(ctx, p.TenantID, datasetID); err != nil {
		return nil, err
	}
	return s.deps.Repo.ListBranches(ctx, p.TenantID, datasetID)
}

// MergeRequest merges a version of a branch into its parent's draft, or a
// version of the parent into the branch's draft.
type MergeRequest struct {
	SourceDatasetID string
	// SourceVersionID is the version merged: the source's latest when empty.
	SourceVersionID string
	Strategy        model.MergeStrategy
	DryRun          bool
	IdempotencyKey  string
}

// Merge merges into targetID's draft. With strategy FAIL (the default) a
// merge with conflicts changes nothing and answers model.ErrMergeConflict
// with the conflicts. Take a snapshot of the target afterwards to keep the
// result as a version.
func (s *Service) Merge(ctx context.Context, p model.Principal, targetID string, req MergeRequest) (model.MergeResult, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsWrite, "dataset.merge", "dataset", targetID); err != nil {
		return model.MergeResult{}, err
	}
	if req.Strategy == "" {
		req.Strategy = model.MergeFail
	}
	switch req.Strategy {
	case model.MergeFail, model.MergeSource, model.MergeTarget:
	default:
		return model.MergeResult{}, fmt.Errorf("%w: strategy must be FAIL, SOURCE or TARGET", ErrInvalidRequest)
	}
	if len(req.IdempotencyKey) > 128 || hasControl(req.IdempotencyKey) {
		return model.MergeResult{}, fmt.Errorf("%w: the idempotency key is at most 128 characters", ErrInvalidRequest)
	}
	target, err := s.deps.Repo.GetDataset(ctx, p.TenantID, targetID)
	if err != nil {
		return model.MergeResult{}, err
	}
	if target.Lifecycle == model.LifecycleArchived {
		return model.MergeResult{}, model.ErrDatasetArchived
	}
	source, err := s.deps.Repo.GetDataset(ctx, p.TenantID, req.SourceDatasetID)
	if err != nil {
		return model.MergeResult{}, err
	}
	var branch model.Dataset
	switch {
	case source.ParentID == target.ID:
		branch = source
	case target.ParentID == source.ID:
		branch = target
	default:
		return model.MergeResult{}, fmt.Errorf("%w: a merge is between a branch and the dataset it belongs to", ErrInvalidRequest)
	}
	sourceVersion := req.SourceVersionID
	if sourceVersion == "" {
		sourceVersion = source.CurrentVersionID
	}
	if sourceVersion == "" {
		return model.MergeResult{}, fmt.Errorf("%w: take a snapshot of %s first: a merge brings in a version", ErrNoVersion, source.Name)
	}
	v, err := s.deps.Repo.GetVersion(ctx, p.TenantID, sourceVersion)
	if err != nil {
		return model.MergeResult{}, err
	}
	if v.DatasetID != source.ID {
		return model.MergeResult{}, model.ErrDatasetVersionNotFound
	}

	res, err := s.deps.Repo.Merge(ctx, model.MergeInput{
		TenantID: p.TenantID, TargetID: target.ID, SourceID: source.ID, SourceVersionID: sourceVersion,
		BaseVersionID: branch.MergeBaseVersionID, BranchID: branch.ID, Strategy: req.Strategy, DryRun: req.DryRun,
		IdempotencyKey: req.IdempotencyKey, Actor: p.UserID, Now: s.now(), MaxConflicts: maxMergeConflicts,
	})
	if err != nil {
		return res, err
	}
	if res.Applied {
		s.record(ctx, p, "dataset.merge", "dataset", target.ID, map[string]any{
			"source_id": source.ID, "source_version_id": sourceVersion, "base_version_id": res.BaseVersionID,
			"strategy": string(req.Strategy), "added": res.Added, "updated": res.Updated, "removed": res.Removed,
			"conflicts": res.Conflicts})
	}
	if !res.Applied && !req.DryRun && res.Conflicts > 0 {
		return res, model.ErrMergeConflict
	}
	return res, nil
}

// Merges lists the merges into a dataset, newest first.
func (s *Service) Merges(ctx context.Context, p model.Principal, datasetID string) ([]model.MergeResult, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsRead, "dataset.read", "dataset", datasetID); err != nil {
		return nil, err
	}
	if _, err := s.deps.Repo.GetDataset(ctx, p.TenantID, datasetID); err != nil {
		return nil, err
	}
	return s.deps.Repo.ListMerges(ctx, p.TenantID, datasetID, 100)
}

// sameFamily reports whether two datasets are a parent and its branch, two
// branches of one parent, or the same dataset.
func (s *Service) sameFamily(ctx context.Context, tenantID, a, b string) (bool, error) {
	if a == b {
		return true, nil
	}
	da, err := s.deps.Repo.GetDataset(ctx, tenantID, a)
	if err != nil {
		return false, err
	}
	db, err := s.deps.Repo.GetDataset(ctx, tenantID, b)
	if err != nil {
		return false, err
	}
	return root(da) == root(db), nil
}
