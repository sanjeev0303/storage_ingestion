// Package erasure runs erasure, withdrawal and destroy-by-provenance
// (docs/erasure.md): it finds content by subject, hash or object and
// every exact copy, makes successor snapshots without it, deletes its
// bytes through the lifecycle where no hold forbids it, and issues a
// signed certificate of destruction.
package erasure

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/asset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
	"github.com/sanjeev0303/object-storage-platform/internal/service/catalog"
)

type Signer struct {
	KeyID string
	Key   ed25519.PrivateKey
}

type Deps struct {
	Store     *postgres.ErasureRepository
	Lifecycle *asset.Service
	Catalog   *catalog.Service
	Audit     interfaces.AuditRecorder
	Signer    Signer
	Log       *slog.Logger
}

type Service struct {
	deps Deps
	now  func() time.Time
}

func New(deps Deps) *Service {
	return &Service{deps: deps, now: func() time.Time { return time.Now().UTC() }}
}

// heldChecked is how many objects a plan checks for holds one by one.
const heldChecked = 500

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{model.ErrInvalidErasure}, args...)...)
}

func (s *Service) record(ctx context.Context, p model.Principal, action, id string, details map[string]any) {
	if s.deps.Audit == nil {
		return
	}
	e := audit.From(ctx, p, action, "erasure", id)
	e.Details = details
	s.deps.Audit.Record(ctx, e)
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// normalize trims and checks a selector.
func normalize(sel model.ErasureSelector) (model.ErasureSelector, error) {
	out := model.ErasureSelector{SubjectIDs: []string{}, SHA256s: []string{}, ObjectIDs: []string{}, SubjectKey: strings.TrimSpace(sel.SubjectKey)}
	if out.SubjectKey == "" {
		out.SubjectKey = model.DefaultSubjectKey
	}
	if len(out.SubjectKey) > 128 {
		return out, invalid("subject_key is at most 128 characters")
	}
	add := func(into *[]string, values []string, check func(string) error) error {
		seen := map[string]bool{}
		for _, v := range values {
			v = strings.TrimSpace(v)
			if v == "" || seen[v] {
				continue
			}
			if err := check(v); err != nil {
				return err
			}
			seen[v] = true
			*into = append(*into, v)
		}
		return nil
	}
	if err := add(&out.SubjectIDs, sel.SubjectIDs, func(v string) error {
		if len(v) > 200 {
			return invalid("a subject is at most 200 characters")
		}
		return nil
	}); err != nil {
		return out, err
	}
	lower := make([]string, len(sel.SHA256s))
	for i, h := range sel.SHA256s {
		lower[i] = strings.ToLower(strings.TrimSpace(h))
	}
	if err := add(&out.SHA256s, lower, func(v string) error {
		if !sha256Hex.MatchString(v) {
			return invalid("%q is not a SHA-256 in hex", v)
		}
		return nil
	}); err != nil {
		return out, err
	}
	if err := add(&out.ObjectIDs, sel.ObjectIDs, func(string) error { return nil }); err != nil {
		return out, err
	}
	n := len(out.SubjectIDs) + len(out.SHA256s) + len(out.ObjectIDs)
	if n == 0 {
		return out, invalid("name subjects, SHA-256 hashes or objects to erase")
	}
	if n > 100000 {
		return out, invalid("at most 100,000 subjects, hashes and objects together")
	}
	return out, nil
}

// Search answers what an erasure would reach, recording nothing.
func (s *Service) Search(ctx context.Context, p model.Principal, sel model.ErasureSelector) (model.ErasurePlan, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermRetentionManage, "erasure.search", "erasure", ""); err != nil {
		return model.ErasurePlan{}, err
	}
	sel, err := normalize(sel)
	if err != nil {
		return model.ErasurePlan{}, err
	}
	return s.plan(ctx, p.TenantID, sel)
}

func (s *Service) plan(ctx context.Context, tenantID string, sel model.ErasureSelector) (model.ErasurePlan, error) {
	st := s.deps.Store
	out := model.ErasurePlan{Objects: []model.PlanObject{}, Connections: []string{}, Versions: []model.PlanVersion{},
		Drafts: []model.PlanDraft{}, Models: []model.PlanModel{}, NotSearched: []string{
			"near copies: only exact SHA-256 copies were matched; re-encoded, resized, cropped or paraphrased copies are not detected",
			"copies outside this organization's storage connections",
		}}
	if len(sel.SubjectIDs) > 0 {
		out.NotSearched = append(out.NotSearched, "subjects named only inside files' contents, not in their "+sel.SubjectKey+" metadata")
	}
	matched, err := st.Matches(ctx, tenantID, sel)
	if err != nil || len(matched) == 0 {
		out.Matched = []string{}
		return out, err
	}
	out.Matched = matched
	if out.Objects, err = st.Objects(ctx, tenantID, matched); err != nil {
		return out, err
	}
	conns := map[string]bool{}
	now := s.now()
	for i := range out.Objects {
		o := &out.Objects[i]
		out.Bytes += o.SizeBytes
		conns[o.ConnectionID] = true
		if i >= heldChecked {
			continue
		}
		in, err := s.deps.Lifecycle.Inspect(ctx, tenantID, o.AssetID)
		if err != nil {
			return out, err
		}
		for _, h := range in.Holds {
			about := h.Matter
			if about == "" {
				about = h.Reason
			}
			o.HeldBy = append(o.HeldBy, "legal hold: "+about)
		}
		if in.RetainUntil.After(now) {
			o.HeldBy = append(o.HeldBy, "retention until "+in.RetainUntil.Format(time.RFC3339))
		}
		if in.LockedUntil != nil && in.LockedUntil.After(now) {
			o.HeldBy = append(o.HeldBy, "Object Lock until "+in.LockedUntil.Format(time.RFC3339))
		}
	}
	if len(out.Objects) > heldChecked {
		out.NotSearched = append(out.NotSearched, fmt.Sprintf("holds were checked for the first %d objects; the deletion checks every one", heldChecked))
	}
	for c := range conns {
		out.Connections = append(out.Connections, c)
	}
	slices.Sort(out.Connections)
	if out.Versions, err = st.Versions(ctx, tenantID, matched); err != nil {
		return out, err
	}
	if out.Drafts, err = st.Drafts(ctx, tenantID, matched); err != nil {
		return out, err
	}
	ids := make([]string, len(out.Versions))
	for i, v := range out.Versions {
		ids[i] = v.VersionID
	}
	out.Models, err = st.Models(ctx, tenantID, ids)
	return out, err
}

func hashSubjects(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		sum := sha256.Sum256([]byte(id))
		out[i] = hex.EncodeToString(sum[:])
	}
	return out
}

// CreateInput records a request.
type CreateInput struct {
	Kind        string
	Reason      string
	Reference   string
	Institution string
	Selector    model.ErasureSelector
}

func (s *Service) Create(ctx context.Context, p model.Principal, in CreateInput) (model.Erasure, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermRetentionManage, "erasure.request", "erasure", ""); err != nil {
		return model.Erasure{}, err
	}
	in.Kind, in.Reason = strings.ToUpper(strings.TrimSpace(in.Kind)), strings.TrimSpace(in.Reason)
	in.Reference, in.Institution = strings.TrimSpace(in.Reference), strings.TrimSpace(in.Institution)
	switch {
	case !slices.Contains(model.ErasureKinds, in.Kind):
		return model.Erasure{}, invalid("kind is one of %s", strings.Join(model.ErasureKinds, ", "))
	case in.Reason == "" || len(in.Reason) > 2000:
		return model.Erasure{}, invalid("a reason of 1 to 2000 characters")
	case len(in.Reference) > 200 || len(in.Institution) > 200:
		return model.Erasure{}, invalid("reference and institution are at most 200 characters")
	}
	sel, err := normalize(in.Selector)
	if err != nil {
		return model.Erasure{}, err
	}
	plan, err := s.plan(ctx, p.TenantID, sel)
	if err != nil {
		return model.Erasure{}, err
	}
	if len(plan.Objects) == 0 && len(plan.Versions) == 0 && len(plan.Drafts) == 0 {
		return model.Erasure{}, model.ErrNothingToErase
	}
	now := s.now()
	e := model.Erasure{ID: event.NewID(), TenantID: p.TenantID, Kind: in.Kind, State: model.ErasurePlanned, Reason: in.Reason,
		Reference: in.Reference, Institution: in.Institution, Selector: sel, SubjectHashes: hashSubjects(sel.SubjectIDs), Plan: plan,
		Successors: []model.Successor{}, RequestedBy: p.UserID, CreatedAt: now, UpdatedAt: now}
	if err := s.deps.Store.Create(ctx, e); err != nil {
		return e, err
	}
	s.record(ctx, p, "erasure.requested", e.ID, map[string]any{"kind": e.Kind, "subject_sha256s": e.SubjectHashes,
		"sha256s": sel.SHA256s, "object_ids": sel.ObjectIDs, "objects": len(plan.Objects), "versions": len(plan.Versions),
		"models": len(plan.Models), "institution": e.Institution, "reference": e.Reference})
	return s.Get(ctx, p, e.ID)
}

func progressOf(items []model.ErasureItem) model.ErasureProgress {
	out := model.ErasureProgress{Total: len(items)}
	for _, i := range items {
		switch i.State {
		case model.ErasureItemDestroyed:
			out.Destroyed++
		case model.ErasureItemHeld:
			out.Held++
		case model.ErasureItemFailed:
			out.Failed++
		default:
			out.Pending++
		}
	}
	return out
}

func (s *Service) Get(ctx context.Context, p model.Principal, id string) (model.Erasure, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermRetentionManage, "erasure.read", "erasure", id); err != nil {
		return model.Erasure{}, err
	}
	e, err := s.deps.Store.Get(ctx, p.TenantID, id)
	if err != nil {
		return e, err
	}
	if e.Items, err = s.deps.Store.Items(ctx, p.TenantID, id); err != nil {
		return e, err
	}
	e.Progress = progressOf(e.Items)
	return e, nil
}

func (s *Service) List(ctx context.Context, p model.Principal, state string) ([]model.Erasure, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermRetentionManage, "erasure.read", "erasure", ""); err != nil {
		return nil, err
	}
	return s.deps.Store.List(ctx, p.TenantID, strings.ToUpper(state), 200)
}

// Approve is the second member's approval, where governance asks for one.
func (s *Service) Approve(ctx context.Context, p model.Principal, id, note string) (model.Erasure, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermRetentionManage, "erasure.approve", "erasure", id); err != nil {
		return model.Erasure{}, err
	}
	e, err := s.deps.Store.Get(ctx, p.TenantID, id)
	if err != nil {
		return e, err
	}
	if e.RequestedBy == p.UserID {
		return e, model.ErrSelfApproval
	}
	now := s.now()
	ok, err := s.deps.Store.Transition(ctx, p.TenantID, id, []string{model.ErasurePlanned}, model.ErasureApproved, func(e *model.Erasure) {
		e.ApprovedBy, e.ApprovedNote, e.ApprovedAt = p.UserID, strings.TrimSpace(note), &now
	})
	if err != nil {
		return e, err
	}
	if !ok {
		return e, model.ErrErasureState
	}
	s.record(ctx, p, "erasure.approved", id, map[string]any{"note": strings.TrimSpace(note)})
	return s.Get(ctx, p, id)
}

func (s *Service) Cancel(ctx context.Context, p model.Principal, id, note string) (model.Erasure, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermRetentionManage, "erasure.cancel", "erasure", id); err != nil {
		return model.Erasure{}, err
	}
	ok, err := s.deps.Store.Transition(ctx, p.TenantID, id, []string{model.ErasurePlanned, model.ErasureApproved}, model.ErasureCancelled,
		func(e *model.Erasure) { e.Note = strings.TrimSpace(note) })
	if err != nil {
		return model.Erasure{}, err
	}
	if !ok {
		return model.Erasure{}, model.ErrErasureState
	}
	s.record(ctx, p, "erasure.cancelled", id, map[string]any{"note": strings.TrimSpace(note)})
	return s.Get(ctx, p, id)
}

// Execute plans again, makes the successor snapshots, empties the drafts,
// and asks the lifecycle to delete every matched object now.
func (s *Service) Execute(ctx context.Context, p model.Principal, id string) (model.Erasure, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermRetentionManage, "erasure.execute", "erasure", id); err != nil {
		return model.Erasure{}, err
	}
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermAssetsDelete, "erasure.execute", "erasure", id); err != nil {
		return model.Erasure{}, err
	}
	e, err := s.deps.Store.Get(ctx, p.TenantID, id)
	if err != nil {
		return e, err
	}
	if e.State != model.ErasurePlanned && e.State != model.ErasureApproved {
		return e, model.ErrErasureState
	}
	g, err := s.deps.Lifecycle.GovernanceFor(ctx, p)
	if err != nil {
		return e, err
	}
	if g.ApproveDeletes && e.State != model.ErasureApproved {
		return e, model.ErrErasureApproval
	}
	plan, err := s.plan(ctx, p.TenantID, e.Selector)
	if err != nil {
		return e, err
	}
	now := s.now()

	// ponytail: a second execute racing this one repeats idempotent steps
	// (a superseded snapshot gets no second successor, a deletion asked
	// twice is one); a lock per request would stop the duplicate work.
	versions := slices.Clone(plan.Versions)
	sort.SliceStable(versions, func(i, j int) bool {
		if versions[i].DatasetID != versions[j].DatasetID {
			return versions[i].DatasetID < versions[j].DatasetID
		}
		return versions[i].Version < versions[j].Version
	})
	successors := []model.Successor{}
	for _, v := range versions {
		if v.SupersededBy != "" {
			continue
		}
		succ, made, err := s.deps.Store.Successor(ctx, p.TenantID, v.VersionID, plan.Matched, id, p.UserID, event.NewID(), now)
		if err != nil {
			return e, fmt.Errorf("successor of version %s: %w", v.VersionID, err)
		}
		if made {
			successors = append(successors, succ)
		}
	}
	drafted, err := s.deps.Store.RemoveFromDrafts(ctx, p.TenantID, plan.Matched, p.UserID, now)
	if err != nil {
		return e, err
	}

	items := make([]model.ErasureItem, 0, len(plan.Objects))
	ids := make([]string, 0, len(plan.Objects))
	for _, o := range plan.Objects {
		it := model.ErasureItem{AssetID: o.AssetID, ObjectID: o.ObjectID, ObjectVersion: o.Version, Path: o.Path, SHA256: o.SHA256,
			SizeBytes: o.SizeBytes, ConnectionID: o.ConnectionID, State: model.ErasureItemDeleting}
		if err := s.requestDeletion(ctx, p, o); err != nil {
			it.State, it.Reason = model.ErasureItemPending, err.Error()
			if !errors.Is(err, model.ErrDeletionBlocked) {
				it.State = model.ErasureItemFailed
			}
		}
		items = append(items, it)
		ids = append(ids, o.AssetID)
	}
	if _, err := s.deps.Store.Expedite(ctx, p.TenantID, ids, id, now); err != nil {
		return e, err
	}
	if err := s.deps.Store.PutItems(ctx, p.TenantID, id, items, now); err != nil {
		return e, err
	}
	shas := make([]string, 0, len(plan.Objects))
	for _, o := range plan.Objects {
		if !slices.Contains(shas, o.SHA256) {
			shas = append(shas, o.SHA256)
		}
	}
	s.record(ctx, p, "erasure.executed", id, map[string]any{"kind": e.Kind, "subject_sha256s": e.SubjectHashes, "content_sha256s": shas,
		"objects": len(plan.Objects), "bytes": plan.Bytes, "successors": len(successors), "drafts_emptied": drafted,
		"models_to_retrain": len(plan.Models), "institution": e.Institution, "reference": e.Reference})
	ok, err := s.deps.Store.Transition(ctx, p.TenantID, id, []string{model.ErasurePlanned, model.ErasureApproved}, model.ErasureDeleting,
		func(e *model.Erasure) {
			e.Plan, e.Successors, e.ExecutedBy, e.ExecutedAt = plan, successors, p.UserID, &now
		})
	if err != nil {
		return e, err
	}
	if !ok {
		return e, model.ErrErasureState
	}
	return s.Get(ctx, p, id)
}

// requestDeletion asks the lifecycle to delete one object, through the
// catalog when it is a catalog version (so a current version stops
// being current first).
func (s *Service) requestDeletion(ctx context.Context, p model.Principal, o model.PlanObject) error {
	if o.Status == string(model.AssetStatusSoftDeleted) {
		return nil
	}
	if o.ObjectID != "" && o.Version > 0 && s.deps.Catalog != nil {
		_, err := s.deps.Catalog.DeleteVersion(ctx, p, o.ObjectID, o.Version)
		return err
	}
	_, err := s.deps.Lifecycle.RequestDeletion(ctx, p, o.AssetID)
	return err
}

// Progress follows the deletions of open erasures and completes those
// whose every file is gone. Run by the cleanup worker.
func (s *Service) Progress(ctx context.Context) (int, error) {
	open, err := s.deps.Store.Open(ctx, 50)
	if err != nil {
		return 0, err
	}
	done := 0
	for _, e := range open {
		finished, err := s.advance(ctx, e)
		if err != nil {
			return done, fmt.Errorf("erasure %s: %w", e.ID, err)
		}
		if finished {
			done++
		}
	}
	return done, nil
}

func (s *Service) advance(ctx context.Context, e model.Erasure) (bool, error) {
	st := s.deps.Store
	items, err := st.Items(ctx, e.TenantID, e.ID)
	if err != nil {
		return false, err
	}
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.AssetID
	}
	states, err := st.DeletionStates(ctx, e.TenantID, ids)
	if err != nil {
		return false, err
	}
	byAsset := map[string]postgres.DeletionState{}
	for _, d := range states {
		byAsset[d.AssetID] = d
	}
	now := s.now()
	service := model.ServicePrincipal(e.TenantID, "erasure")
	var retry []string
	for i := range items {
		it := &items[i]
		d, ok := byAsset[it.AssetID]
		it.DeletionID, it.CertificateID = d.DeletionID, d.CertificateID
		switch {
		case !ok || d.AssetStatus == string(model.AssetStatusDeleted) || d.State == string(model.DeletionCompleted):
			it.State, it.Reason = model.ErasureItemDestroyed, ""
			if it.DestroyedAt == nil {
				it.DestroyedAt = d.UpdatedAt
				if it.DestroyedAt == nil {
					it.DestroyedAt = &now
				}
			}
		case d.State == string(model.DeletionProtected):
			it.State, it.Reason = model.ErasureItemHeld, d.Detail
		case d.State == string(model.DeletionFailed):
			it.State, it.Reason = model.ErasureItemFailed, d.Detail
		case d.State == string(model.DeletionCancelled):
			it.State, it.Reason = model.ErasureItemFailed, "the deletion was cancelled (the file was restored): "+d.Detail
		case d.State == "":
			// No deletion yet (it was blocked while processing): ask again.
			o := model.PlanObject{AssetID: it.AssetID, ObjectID: it.ObjectID, Version: it.ObjectVersion, Status: d.AssetStatus}
			if err := s.requestDeletion(ctx, service, o); err != nil {
				it.State, it.Reason = model.ErasureItemPending, err.Error()
			} else {
				it.State, it.Reason = model.ErasureItemDeleting, ""
				retry = append(retry, it.AssetID)
			}
		default:
			it.State, it.Reason = model.ErasureItemDeleting, ""
		}
	}
	if len(retry) > 0 {
		if _, err := st.Expedite(ctx, e.TenantID, retry, e.ID, now); err != nil {
			return false, err
		}
	}
	if err := st.PutItems(ctx, e.TenantID, e.ID, items, now); err != nil {
		return false, err
	}
	p := progressOf(items)
	next := model.ErasureDeleting
	switch {
	case p.Destroyed == p.Total:
		return true, s.complete(ctx, e, items, now)
	case p.Pending > 0:
		next = model.ErasureDeleting
	case p.Held > 0:
		next = model.ErasureWaiting
	case p.Failed > 0:
		next = model.ErasureFailed
	}
	if next != e.State {
		if _, err := st.Transition(ctx, e.TenantID, e.ID, []string{e.State}, next, func(*model.Erasure) {}); err != nil {
			return false, err
		}
	}
	return false, nil
}

// certificateBody is what a certificate of destruction states.
type certificateBody struct {
	Type         string            `json:"type"`
	ErasureID    string            `json:"erasure_id"`
	Organization string            `json:"organization_id"`
	Institution  string            `json:"institution"`
	Kind         string            `json:"kind"`
	Reason       string            `json:"reason"`
	Reference    string            `json:"reference"`
	RequestedBy  string            `json:"requested_by"`
	ApprovedBy   string            `json:"approved_by,omitempty"`
	ExecutedBy   string            `json:"executed_by"`
	ExecutedAt   *time.Time        `json:"executed_at"`
	MatchedBy    map[string]any    `json:"matched_by"`
	Destroyed    []destroyedFile   `json:"destroyed"`
	Superseded   []model.Successor `json:"snapshots_superseded"`
	Models       []model.PlanModel `json:"models"`
	NotSearched  []string          `json:"not_searched"`
	Tombstone    map[string]any    `json:"audit_tombstone"`
	IssuedAt     time.Time         `json:"issued_at"`
	KeyID        string            `json:"key_id"`
}

type destroyedFile struct {
	SHA256            string     `json:"sha256"`
	SizeBytes         int64      `json:"size_bytes"`
	ConnectionID      string     `json:"connection_id"`
	Path              string     `json:"path"`
	DestroyedAt       *time.Time `json:"destroyed_at"`
	DispositionID     string     `json:"disposition_certificate_id,omitempty"`
	DispositionSHA256 string     `json:"disposition_certificate_sha256,omitempty"`
}

func (s *Service) complete(ctx context.Context, e model.Erasure, items []model.ErasureItem, now time.Time) error {
	st := s.deps.Store
	seq, err := st.TombstoneSeq(ctx, e.TenantID, e.ID)
	if err != nil {
		return err
	}
	body := certificateBody{Type: "osp.destruction-certificate/v1", ErasureID: e.ID, Organization: e.TenantID, Institution: e.Institution,
		Kind: e.Kind, Reason: e.Reason, Reference: e.Reference, RequestedBy: e.RequestedBy, ApprovedBy: e.ApprovedBy, ExecutedBy: e.ExecutedBy,
		ExecutedAt: e.ExecutedAt, MatchedBy: map[string]any{"subject_sha256s": e.SubjectHashes, "subject_key": e.Selector.SubjectKey,
			"sha256s": e.Selector.SHA256s, "object_ids": e.Selector.ObjectIDs, "exact_copies": true},
		Destroyed: []destroyedFile{}, Superseded: e.Successors, Models: e.Plan.Models, NotSearched: e.Plan.NotSearched,
		Tombstone: map[string]any{"action": "erasure.executed", "seq": seq}, IssuedAt: now, KeyID: s.deps.Signer.KeyID}
	for _, it := range items {
		f := destroyedFile{SHA256: it.SHA256, SizeBytes: it.SizeBytes, ConnectionID: it.ConnectionID, Path: it.Path, DestroyedAt: it.DestroyedAt,
			DispositionID: it.CertificateID}
		if it.CertificateID != "" {
			if f.DispositionSHA256, err = st.Certificate(ctx, e.TenantID, it.CertificateID); err != nil {
				return err
			}
		}
		body.Destroyed = append(body.Destroyed, f)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if len(s.deps.Signer.Key) == 0 {
		return errors.New("no certificate signing key (CERTIFICATE_SIGNING_KEY)")
	}
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(s.deps.Signer.Key, raw))
	if err := st.SetCertificate(ctx, e.TenantID, e.ID, string(raw), sig, s.deps.Signer.KeyID); err != nil {
		return err
	}
	// The subjects stay only as their hashes once the work is done.
	ok, err := st.Transition(ctx, e.TenantID, e.ID, []string{e.State}, model.ErasureCompleted, func(x *model.Erasure) {
		x.CompletedAt, x.Selector.SubjectIDs = &now, []string{}
	})
	if err != nil || !ok {
		return err
	}
	s.record(ctx, model.ServicePrincipal(e.TenantID, "erasure"), "erasure.completed", e.ID, map[string]any{
		"destroyed": len(items), "certificate_sha256": hex.EncodeToString(sha256Of(raw))})
	s.deps.Log.Info("erasure completed", "erasure_id", e.ID, "tenant_id", e.TenantID, "files", len(items))
	return nil
}

func sha256Of(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// Certificate returns the signed certificate of a completed erasure.
func (s *Service) Certificate(ctx context.Context, p model.Principal, id string) (model.ErasureCertificate, error) {
	perm := model.PermRetentionManage
	if !p.Permissions.Has(perm) {
		perm = model.PermAuditRead
	}
	if err := audit.Require(ctx, s.deps.Audit, p, perm, "erasure.certificate", "erasure", id); err != nil {
		return model.ErasureCertificate{}, err
	}
	body, sig, key, err := s.deps.Store.CertificateOf(ctx, p.TenantID, id)
	if err != nil {
		return model.ErasureCertificate{}, err
	}
	if body == "" {
		return model.ErasureCertificate{}, fmt.Errorf("%w: the certificate is issued when every file is destroyed", model.ErrErasureState)
	}
	return model.ErasureCertificate{Body: body, BodySHA256: hex.EncodeToString(sha256Of([]byte(body))), Signature: sig, KeyID: key,
		PublicKey: base64.StdEncoding.EncodeToString(s.deps.Signer.Key.Public().(ed25519.PublicKey))}, nil
}
