package evidence

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

// ─── Provenance and the publish gate ──────────────────────────────────

func (s *Service) Provenance(ctx context.Context, p model.Principal, datasetID string) (model.Provenance, error) {
	if _, err := s.deps.Datasets.Get(ctx, p, datasetID); err != nil {
		return model.Provenance{}, err
	}
	return s.deps.Store.Provenance(ctx, p.TenantID, datasetID)
}

// PutProvenance records a dataset's lawful basis, licence and source. The
// record is an entry in the audit chain, so the order of basis, snapshot
// and run can be shown later.
func (s *Service) PutProvenance(ctx context.Context, p model.Principal, datasetID string, in model.Provenance) (model.Provenance, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsManage, "dataset.provenance_recorded", "dataset", datasetID); err != nil {
		return model.Provenance{}, err
	}
	if _, err := s.deps.Datasets.Get(ctx, p, datasetID); err != nil {
		return model.Provenance{}, err
	}
	in.LawfulBasis, in.License, in.Source = strings.TrimSpace(in.LawfulBasis), strings.TrimSpace(in.License), strings.TrimSpace(in.Source)
	switch {
	case len(in.LawfulBasis) > 500 || len(in.License) > 500 || len(in.Source) > 2000:
		return model.Provenance{}, invalid("lawful_basis and license are at most 500 characters, source 2000")
	case in.SourceKind != "" && !slices.Contains(model.SourceKinds, in.SourceKind):
		return model.Provenance{}, invalid("source_kind is one of %s", strings.Join(model.SourceKinds, ", "))
	}
	now := s.now()
	in.RecordedAt, in.RecordedBy = &now, p.UserID
	if err := s.deps.Store.PutProvenance(ctx, p.TenantID, datasetID, in); err != nil {
		return model.Provenance{}, err
	}
	s.record(ctx, p, "dataset.provenance_recorded", "dataset", datasetID, map[string]any{"lawful_basis": in.LawfulBasis,
		"license": in.License, "source": in.Source, "source_kind": in.SourceKind})
	return in, nil
}

// BeforeSnapshot is the publish gate: with require_lawful_basis, a
// snapshot needs its dataset's provenance first.
func (s *Service) BeforeSnapshot(ctx context.Context, tenantID, datasetID string) error {
	st, err := s.deps.Store.Settings(ctx, tenantID)
	if err != nil || !st.RequireLawfulBasis {
		return err
	}
	prov, err := s.deps.Store.Provenance(ctx, tenantID, datasetID)
	if err != nil {
		return err
	}
	if !prov.Complete() {
		return model.ErrProvenanceRequired
	}
	return nil
}

// BeforeRun is the gate on training: with require_lawful_basis, every
// snapshot a run reads must have been taken after its dataset's
// provenance was recorded.
func (s *Service) BeforeRun(ctx context.Context, tenantID string, versionIDs []string) error {
	st, err := s.deps.Store.Settings(ctx, tenantID)
	if err != nil || !st.RequireLawfulBasis || len(versionIDs) == 0 {
		return err
	}
	orders, err := s.deps.Store.VersionOrders(ctx, tenantID, versionIDs)
	if err != nil {
		return err
	}
	for _, o := range orders {
		if !o.Provenance.Complete() || o.Provenance.RecordedAt == nil {
			return fmt.Errorf("%w: %s has no lawful basis recorded", model.ErrProvenanceRequired, o.DatasetName)
		}
		if o.Provenance.RecordedAt.After(o.CreatedAt) {
			return fmt.Errorf("%w: %s version %d", model.ErrProvenanceOrder, o.DatasetName, o.Version)
		}
	}
	return nil
}

// OrderStep is one step of a run's evidence of order, with its place in
// the audit chain.
type OrderStep struct {
	Step     string     `json:"step"`
	Subject  string     `json:"subject"`
	At       *time.Time `json:"at,omitempty"`
	AuditSeq int64      `json:"audit_seq,omitempty"`
	Detail   string     `json:"detail,omitempty"`
}

// RunOrder is a training run's evidence that each dataset's lawful basis
// came before its snapshot, and the snapshot before the run.
type RunOrder struct {
	RunID   string      `json:"run_id"`
	InOrder bool        `json:"in_order"`
	Steps   []OrderStep `json:"steps"`
	Problem []string    `json:"problems"`
}

func (s *Service) RunOrder(ctx context.Context, p model.Principal, runID string) (RunOrder, error) {
	if _, err := s.deps.Datasets.TrainingRun(ctx, p, runID); err != nil {
		return RunOrder{}, err
	}
	versions, runAt, err := s.deps.Store.RunVersions(ctx, p.TenantID, runID)
	if err != nil {
		return RunOrder{}, err
	}
	orders, err := s.deps.Store.VersionOrders(ctx, p.TenantID, versions)
	if err != nil {
		return RunOrder{}, err
	}
	out := RunOrder{RunID: runID, InOrder: true, Steps: []OrderStep{}, Problem: []string{}}
	for _, o := range orders {
		basisSeq, _ := s.deps.Store.AuditSeqOf(ctx, p.TenantID, "dataset.provenance_recorded", o.DatasetID)
		snapSeq, _ := s.deps.Store.AuditSeqOf(ctx, p.TenantID, "dataset.snapshot", o.DatasetID)
		created := o.CreatedAt
		out.Steps = append(out.Steps,
			OrderStep{Step: "lawful basis", Subject: o.DatasetName, At: o.Provenance.RecordedAt, AuditSeq: basisSeq,
				Detail: strings.TrimSpace(o.Provenance.LawfulBasis + "; " + o.Provenance.License + "; " + o.Provenance.Source)},
			OrderStep{Step: "snapshot", Subject: fmt.Sprintf("%s version %d", o.DatasetName, o.Version), At: &created, AuditSeq: snapSeq})
		switch {
		case o.Provenance.RecordedAt == nil:
			out.InOrder = false
			out.Problem = append(out.Problem, o.DatasetName+": no lawful basis recorded")
		case o.Provenance.RecordedAt.After(o.CreatedAt):
			out.InOrder = false
			out.Problem = append(out.Problem, fmt.Sprintf("%s version %d was taken before its lawful basis was recorded", o.DatasetName, o.Version))
		}
		if o.CreatedAt.After(runAt) {
			out.InOrder = false
			out.Problem = append(out.Problem, fmt.Sprintf("%s version %d is newer than the run", o.DatasetName, o.Version))
		}
	}
	runSeq, _ := s.deps.Store.AuditSeqOf(ctx, p.TenantID, "training_run.create", runID)
	out.Steps = append(out.Steps, OrderStep{Step: "training run", Subject: runID, At: &runAt, AuditSeq: runSeq})
	return out, nil
}

// ─── Snapshot holds ───────────────────────────────────────────────────

// SnapshotHold is a hold over every member of a snapshot: a FOIA request,
// litigation or an audit, one legal hold per object version.
type SnapshotHold struct {
	Matter   string   `json:"matter"`
	Kind     string   `json:"kind"`
	Holds    []string `json:"hold_ids"`
	Released bool     `json:"released"`
}

var holdKinds = []string{"FOIA", "LITIGATION", "AUDIT", "INVESTIGATION"}

func matterOf(versionID, kind, reference string) string {
	return fmt.Sprintf("snapshot %s: %s %s", versionID, kind, reference)
}

func (s *Service) HoldSnapshot(ctx context.Context, p model.Principal, datasetID, versionID, kind, reference, reason string) (SnapshotHold, error) {
	if _, err := s.deps.Datasets.Version(ctx, p, datasetID, versionID); err != nil {
		return SnapshotHold{}, err
	}
	reference, reason = strings.TrimSpace(reference), strings.TrimSpace(reason)
	if !slices.Contains(holdKinds, kind) || reference == "" || len(reference) > 200 || len(reason) > 1000 {
		return SnapshotHold{}, invalid("kind is FOIA, LITIGATION, AUDIT or INVESTIGATION, with a reference of 1 to 200 characters")
	}
	rows, err := s.deps.Store.Custody(ctx, p.TenantID, versionID)
	if err != nil {
		return SnapshotHold{}, err
	}
	matter := matterOf(versionID, kind, reference)
	out := SnapshotHold{Matter: matter, Kind: kind, Holds: []string{}}
	for _, r := range rows {
		if r.Status == "DELETED" {
			continue
		}
		h, err := s.deps.Holds.PlaceHoldFor(ctx, p, model.LegalHold{ID: newID(), Scope: model.ScopeAsset, ScopeKey: r.AssetID,
			Reason: strings.TrimSpace(kind + " " + reference + ": " + reason), Matter: matter, PlacedAt: s.now()})
		if err != nil {
			return out, err
		}
		out.Holds = append(out.Holds, h.ID)
	}
	return out, nil
}

// SnapshotHolds lists the holds placed over a snapshot, by matter.
func (s *Service) SnapshotHolds(ctx context.Context, p model.Principal, datasetID, versionID string) ([]SnapshotHold, error) {
	if _, err := s.deps.Datasets.Version(ctx, p, datasetID, versionID); err != nil {
		return nil, err
	}
	all, err := s.deps.Holds.HoldsFor(ctx, p, true)
	if err != nil {
		return nil, err
	}
	byMatter := map[string]*SnapshotHold{}
	var order []string
	prefix := "snapshot " + versionID + ": "
	for _, h := range all {
		if !strings.HasPrefix(h.Matter, prefix) {
			continue
		}
		g, ok := byMatter[h.Matter]
		if !ok {
			kind, _, _ := strings.Cut(strings.TrimPrefix(h.Matter, prefix), " ")
			g = &SnapshotHold{Matter: h.Matter, Kind: kind, Holds: []string{}, Released: true}
			byMatter[h.Matter] = g
			order = append(order, h.Matter)
		}
		g.Holds = append(g.Holds, h.ID)
		if h.ReleasedAt == nil {
			g.Released = false
		}
	}
	out := make([]SnapshotHold, 0, len(order))
	for _, m := range order {
		out = append(out, *byMatter[m])
	}
	return out, nil
}

// ReleaseSnapshotHold releases a snapshot hold's legal holds, through the
// governed release (a second member's approval, where required).
func (s *Service) ReleaseSnapshotHold(ctx context.Context, p model.Principal, datasetID, versionID, matter, reason string) (int, error) {
	groups, err := s.SnapshotHolds(ctx, p, datasetID, versionID)
	if err != nil {
		return 0, err
	}
	for _, g := range groups {
		if g.Matter != matter {
			continue
		}
		all, err := s.deps.Holds.HoldsFor(ctx, p, false)
		if err != nil {
			return 0, err
		}
		n := 0
		for _, h := range all {
			if h.Matter == matter {
				if err := s.deps.Holds.ReleaseHoldGoverned(ctx, p, h.ID, reason); err != nil {
					return n, err
				}
				n++
			}
		}
		return n, nil
	}
	return 0, model.ErrHoldNotFound
}
