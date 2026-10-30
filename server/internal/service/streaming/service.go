// Package streaming serves snapshots to training jobs: shard streams that
// read each pack once, signed snapshot manifests, an organization's pack
// size and repacks, and a run's verification reports and checkpoints
// (docs/streaming.md).
package streaming

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
	"github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/delivery"
)

// Store is what the service keeps and reads.
type Store interface {
	Layout(ctx context.Context, tenantID, versionID, afterPath string, afterOrdinal int64, limit int) ([]model.LayoutRow, error)
	MembersAt(ctx context.Context, tenantID, versionID string, ordinals []int64) ([]model.LayoutRow, error)
	PutVerification(ctx context.Context, v model.RunVerification) error
	Verifications(ctx context.Context, tenantID, runID string, limit int) ([]model.RunVerification, error)
	PutCheckpoint(ctx context.Context, c model.RunCheckpoint) error
	Checkpoint(ctx context.Context, tenantID, id string) (model.RunCheckpoint, error)
	Checkpoints(ctx context.Context, tenantID, runID string, limit int) ([]model.RunCheckpoint, error)
	VerifyCheckpoint(ctx context.Context, tenantID, id, host, verifyError string, now time.Time) error
}

// Packs is the part of the pack repository the API uses.
type Packs interface {
	TargetPackBytes(ctx context.Context, tenantID string) (int64, error)
	SetTargetPackBytes(ctx context.Context, tenantID string, bytes int64, by string, now time.Time) error
	CreateRepack(ctx context.Context, r model.RepackRequest) error
	Repacks(ctx context.Context, tenantID, datasetID string, limit int) ([]model.RepackRequest, error)
	CancelRepack(ctx context.Context, tenantID, id string, now time.Time) error
}

// Signer signs snapshot manifests; a nil Key leaves them unsigned.
type Signer struct {
	KeyID string
	Key   ed25519.PrivateKey
}

type Deps struct {
	Datasets *dataset.Service
	Delivery *delivery.Service
	Store    Store
	Packs    Packs
	Audit    interfaces.AuditRecorder
	Signer   Signer
}

type Service struct {
	deps Deps
	now  func() time.Time
}

func New(deps Deps) *Service {
	return &Service{deps: deps, now: func() time.Time { return time.Now().UTC() }}
}

var ErrInvalid = dataset.ErrInvalidRequest

const (
	DefaultShardBytes = 128 << 20
	// MaxShardFiles bounds one stream request.
	MaxShardFiles = 10000
	layoutPage    = 20000
)

// ShardPage is one page of a snapshot's shards and the cursor of the next.
type ShardPage struct {
	Shards     []model.Shard `json:"shards"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

// readable refuses a snapshot an erasure superseded: training reads its
// successor instead (docs/erasure.md).
func (s *Service) readable(ctx context.Context, p model.Principal, datasetID, versionID string) error {
	v, err := s.deps.Datasets.Version(ctx, p, datasetID, versionID)
	if err != nil {
		return err
	}
	if v.SupersededBy != "" {
		return fmt.Errorf("%w (%s): read its successor %s", model.ErrVersionSuperseded, v.SupersededReason, v.SupersededBy)
	}
	return nil
}

// Shards splits a snapshot, in path order, into shards of about
// targetBytes: a shard ends between samples (files sharing a path stem),
// so a sample is always read whole.
func (s *Service) Shards(ctx context.Context, p model.Principal, datasetID, versionID, cursor string, targetBytes int64) (ShardPage, error) {
	if err := s.readable(ctx, p, datasetID, versionID); err != nil {
		return ShardPage{}, err
	}
	if targetBytes == 0 {
		targetBytes = DefaultShardBytes
	}
	if targetBytes < 1<<20 || targetBytes > 4<<30 {
		return ShardPage{}, fmt.Errorf("%w: target_bytes is from 1 MiB to 4 GiB", ErrInvalid)
	}
	afterPath, afterOrdinal, err := parseCursor(cursor)
	if err != nil {
		return ShardPage{}, err
	}
	rows, err := s.deps.Store.Layout(ctx, p.TenantID, versionID, afterPath, afterOrdinal, layoutPage)
	if err != nil {
		return ShardPage{}, err
	}
	shards, ends := splitShards(rows, targetBytes, MaxShardFiles)
	page := ShardPage{Shards: shards}
	if len(rows) == layoutPage && len(shards) > 0 {
		// The last shard may carry on past this page: it is made again,
		// whole, on the next one.
		if len(shards) > 1 {
			page.Shards, ends = shards[:len(shards)-1], ends[:len(ends)-1]
		}
		last := rows[ends[len(ends)-1]]
		page.NextCursor = strconv.FormatInt(last.Ordinal, 10) + ":" + last.Path
	}
	return page, nil
}

// splitShards cuts rows into shards; ends holds each shard's last row.
func splitShards(rows []model.LayoutRow, target int64, maxFiles int) ([]model.Shard, []int) {
	var shards []model.Shard
	var ends []int
	var cur model.Shard
	packs := map[string]bool{}
	for i, r := range rows {
		if cur.Files > 0 {
			full := cur.Bytes+r.SizeBytes > target
			if cur.Files == maxFiles || (full && sampleKey(rows[i-1].Path) != sampleKey(r.Path)) {
				shards, ends = append(shards, cur), append(ends, i-1)
				cur, packs = model.Shard{}, map[string]bool{}
			}
		}
		if cur.Files == 0 {
			cur.FirstPath = r.Path
		}
		cur.Ordinals = append(cur.Ordinals, r.Ordinal)
		cur.Files++
		cur.Bytes += r.SizeBytes
		cur.LastPath = r.Path
		if r.PackID != "" && !packs[r.PackID] {
			packs[r.PackID] = true
			cur.Packs++
		}
	}
	if cur.Files > 0 {
		shards, ends = append(shards, cur), append(ends, len(rows)-1)
	}
	return shards, ends
}

// sampleKey is a file's sample, as WebDataset names it: its path up to
// the first dot of its name.
func sampleKey(path string) string {
	dir, name := "", path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		dir, name = path[:i+1], path[i+1:]
	}
	if j := strings.Index(name, "."); j > 0 {
		name = name[:j]
	}
	return dir + name
}

func parseCursor(cursor string) (string, int64, error) {
	if cursor == "" {
		return "", 0, nil
	}
	n, path, ok := strings.Cut(cursor, ":")
	ordinal, err := strconv.ParseInt(n, 10, 64)
	if !ok || err != nil {
		return "", 0, fmt.Errorf("%w: cursor is not one this listing gave", ErrInvalid)
	}
	return path, ordinal, nil
}

// StreamFile is one file of a stream, with its place in the snapshot.
type StreamFile struct {
	Ordinal int64
	Path    string
	delivery.ShardFile
}

// Stream serves the members at ordinals, in that order, through each.
// Every member is authorized and audited before the first byte.
func (s *Service) Stream(ctx context.Context, p model.Principal, datasetID, versionID string, ordinals []int64, requestID string, each func(StreamFile) error) error {
	if err := s.readable(ctx, p, datasetID, versionID); err != nil {
		return err
	}
	if len(ordinals) == 0 || len(ordinals) > MaxShardFiles {
		return fmt.Errorf("%w: a stream holds 1 to %d files", ErrInvalid, MaxShardFiles)
	}
	rows, err := s.deps.Store.MembersAt(ctx, p.TenantID, versionID, ordinals)
	if err != nil {
		return err
	}
	byOrdinal := make(map[int64]model.LayoutRow, len(rows))
	for _, r := range rows {
		byOrdinal[r.Ordinal] = r
	}
	ids := make([]string, len(ordinals))
	for i, o := range ordinals {
		r, ok := byOrdinal[o]
		if !ok {
			return fmt.Errorf("%w: the snapshot has no member %d", ErrInvalid, o)
		}
		ids[i] = r.AssetID
	}
	i := 0
	return s.deps.Delivery.StreamShard(ctx, p, ids, requestID, func(f delivery.ShardFile) error {
		r := byOrdinal[ordinals[i]]
		i++
		return each(StreamFile{Ordinal: r.Ordinal, Path: r.Path, ShardFile: f})
	})
}

// SignedManifest is a snapshot's identity, signed: its canonical
// manifest's SHA-256 pins every member's path, size and SHA-256.
type SignedManifest struct {
	// Body is the signed bytes, exactly; the fields are inside.
	Body      string `json:"body"`
	Algorithm string `json:"algorithm,omitempty"`
	KeyID     string `json:"key_id,omitempty"`
	Signature string `json:"signature,omitempty"`
}

type manifestBody struct {
	Type           string    `json:"type"`
	TenantID       string    `json:"organization_id"`
	DatasetID      string    `json:"dataset_id"`
	VersionID      string    `json:"version_id"`
	Version        int       `json:"version"`
	MemberCount    int64     `json:"member_count"`
	TotalBytes     int64     `json:"total_bytes"`
	ManifestSHA256 string    `json:"manifest_sha256"`
	ManifestFormat string    `json:"manifest_format"`
	CreatedAt      time.Time `json:"created_at"`
	SignedAt       time.Time `json:"signed_at"`
}

func (s *Service) SignedManifest(ctx context.Context, p model.Principal, datasetID, versionID string) (SignedManifest, error) {
	v, err := s.deps.Datasets.Version(ctx, p, datasetID, versionID)
	if err != nil {
		return SignedManifest{}, err
	}
	raw, err := json.Marshal(manifestBody{Type: "snapshot-manifest/v1", TenantID: v.TenantID, DatasetID: v.DatasetID,
		VersionID: v.ID, Version: v.Version, MemberCount: v.MemberCount, TotalBytes: v.TotalBytes,
		ManifestSHA256: v.ManifestSHA256, ManifestFormat: v.ManifestFormat, CreatedAt: v.CreatedAt, SignedAt: s.now()})
	if err != nil {
		return SignedManifest{}, err
	}
	out := SignedManifest{Body: string(raw)}
	if s.deps.Signer.Key != nil {
		out.Algorithm, out.KeyID = "Ed25519", s.deps.Signer.KeyID
		out.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(s.deps.Signer.Key, raw))
	}
	return out, nil
}

// PackingSettings is an organization's own packing.
type PackingSettings struct {
	// TargetPackBytes is 0 when the platform's default applies.
	TargetPackBytes int64 `json:"target_pack_bytes"`
}

func (s *Service) PackingSettings(ctx context.Context, p model.Principal) (PackingSettings, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermOrgRead, "packing.read", "organization", p.TenantID); err != nil {
		return PackingSettings{}, err
	}
	n, err := s.deps.Packs.TargetPackBytes(ctx, p.TenantID)
	return PackingSettings{TargetPackBytes: n}, err
}

func (s *Service) SetPackingSettings(ctx context.Context, p model.Principal, in PackingSettings) (PackingSettings, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermOrgManage, "packing.update", "organization", p.TenantID); err != nil {
		return PackingSettings{}, err
	}
	if in.TargetPackBytes != 0 && (in.TargetPackBytes < 1<<20 || in.TargetPackBytes > 10<<30) {
		return PackingSettings{}, fmt.Errorf("%w: target_pack_bytes is 0 (the default) or 1 MiB to 10 GiB", ErrInvalid)
	}
	if err := s.deps.Packs.SetTargetPackBytes(ctx, p.TenantID, in.TargetPackBytes, p.UserID, s.now()); err != nil {
		return PackingSettings{}, err
	}
	s.record(ctx, p, "packing.update", "organization", p.TenantID, map[string]any{"target_pack_bytes": in.TargetPackBytes})
	return in, nil
}

// RequestRepack queues a repack of the dataset's packed files.
func (s *Service) RequestRepack(ctx context.Context, p model.Principal, datasetID string) (model.RepackRequest, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsManage, "dataset.repack", "dataset", datasetID); err != nil {
		return model.RepackRequest{}, err
	}
	if _, err := s.deps.Datasets.Get(ctx, p, datasetID); err != nil {
		return model.RepackRequest{}, err
	}
	q := model.RepackRequest{ID: newID(), TenantID: p.TenantID, DatasetID: datasetID, State: "PENDING",
		RequestedBy: p.UserID, CreatedAt: s.now()}
	q.UpdatedAt = q.CreatedAt
	if err := s.deps.Packs.CreateRepack(ctx, q); err != nil {
		return model.RepackRequest{}, err
	}
	s.record(ctx, p, "dataset.repack", "dataset", datasetID, map[string]any{"repack_id": q.ID})
	return q, nil
}

func (s *Service) Repacks(ctx context.Context, p model.Principal, datasetID string) ([]model.RepackRequest, error) {
	if _, err := s.deps.Datasets.Get(ctx, p, datasetID); err != nil {
		return nil, err
	}
	out, err := s.deps.Packs.Repacks(ctx, p.TenantID, datasetID, 20)
	if out == nil {
		out = []model.RepackRequest{}
	}
	return out, err
}

func (s *Service) CancelRepack(ctx context.Context, p model.Principal, datasetID, id string) error {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsManage, "dataset.repack_cancel", "dataset", datasetID); err != nil {
		return err
	}
	if _, err := s.deps.Datasets.Get(ctx, p, datasetID); err != nil {
		return err
	}
	return s.deps.Packs.CancelRepack(ctx, p.TenantID, id, s.now())
}

// VerificationInput is a data loader's report of what it re-hashed.
type VerificationInput struct {
	VersionID  string                `json:"version_id"`
	Host       string                `json:"host"`
	SampleRate float64               `json:"sample_rate"`
	Checked    int64                 `json:"checked"`
	Failed     int64                 `json:"failed"`
	Failures   []model.VerifyFailure `json:"failures"`
}

// maxFailures is how many failures a report keeps; Failed counts all.
const maxFailures = 100

func (s *Service) RecordVerification(ctx context.Context, p model.Principal, runID string, in VerificationInput) (model.RunVerification, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsWrite, "training_run.verification", "training_run", runID); err != nil {
		return model.RunVerification{}, err
	}
	if _, err := s.deps.Datasets.TrainingRun(ctx, p, runID); err != nil {
		return model.RunVerification{}, err
	}
	host := strings.TrimSpace(in.Host)
	switch {
	case host == "" || len(host) > 255:
		return model.RunVerification{}, fmt.Errorf("%w: host names the machine that read the bytes", ErrInvalid)
	case in.SampleRate <= 0 || in.SampleRate > 1:
		return model.RunVerification{}, fmt.Errorf("%w: sample_rate is above 0 and at most 1", ErrInvalid)
	case in.Checked < 0 || in.Failed < 0 || in.Failed > in.Checked:
		return model.RunVerification{}, fmt.Errorf("%w: checked and failed are counts, failed at most checked", ErrInvalid)
	}
	if in.VersionID != "" {
		if _, err := s.deps.Datasets.Version(ctx, p, "", in.VersionID); err != nil {
			return model.RunVerification{}, err
		}
	}
	failures := in.Failures
	if len(failures) > maxFailures {
		failures = failures[:maxFailures]
	}
	for i := range failures {
		failures[i].Path, failures[i].Reason = clip(failures[i].Path, 1024), clip(failures[i].Reason, 256)
	}
	if failures == nil {
		failures = []model.VerifyFailure{}
	}
	v := model.RunVerification{ID: newID(), TenantID: p.TenantID, RunID: runID, VersionID: in.VersionID, Host: host,
		SampleRate: in.SampleRate, Checked: in.Checked, Failed: in.Failed, Failures: failures, CreatedBy: p.UserID, CreatedAt: s.now()}
	if err := s.deps.Store.PutVerification(ctx, v); err != nil {
		return model.RunVerification{}, err
	}
	if v.Failed > 0 {
		s.record(ctx, p, "training_run.verification_failed", "training_run", runID,
			map[string]any{"host": host, "failed": v.Failed, "checked": v.Checked, "version_id": v.VersionID})
	}
	return v, nil
}

func (s *Service) Verifications(ctx context.Context, p model.Principal, runID string) ([]model.RunVerification, error) {
	if _, err := s.deps.Datasets.TrainingRun(ctx, p, runID); err != nil {
		return nil, err
	}
	return s.deps.Store.Verifications(ctx, p.TenantID, runID, 50)
}

// CheckpointInput records a checkpoint: the snapshot holding its files
// and the host that wrote them.
type CheckpointInput struct {
	Step      int64  `json:"step"`
	VersionID string `json:"version_id"`
	Host      string `json:"host"`
}

func (s *Service) AddCheckpoint(ctx context.Context, p model.Principal, runID string, in CheckpointInput) (model.RunCheckpoint, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsWrite, "training_run.checkpoint", "training_run", runID); err != nil {
		return model.RunCheckpoint{}, err
	}
	if _, err := s.deps.Datasets.TrainingRun(ctx, p, runID); err != nil {
		return model.RunCheckpoint{}, err
	}
	host := strings.TrimSpace(in.Host)
	if host == "" || len(host) > 255 || in.Step < 0 || in.VersionID == "" {
		return model.RunCheckpoint{}, fmt.Errorf("%w: a checkpoint has a step, the snapshot of its files and the host that wrote them", ErrInvalid)
	}
	if _, err := s.deps.Datasets.Version(ctx, p, "", in.VersionID); err != nil {
		return model.RunCheckpoint{}, err
	}
	c := model.RunCheckpoint{ID: newID(), TenantID: p.TenantID, RunID: runID, Step: in.Step, VersionID: in.VersionID,
		WrittenBy: host, CreatedBy: p.UserID, CreatedAt: s.now()}
	if err := s.deps.Store.PutCheckpoint(ctx, c); err != nil {
		return model.RunCheckpoint{}, err
	}
	return c, nil
}

// CheckpointCheck is one host's re-read of a checkpoint's files.
type CheckpointCheck struct {
	Host     string                `json:"host"`
	Failures []model.VerifyFailure `json:"failures"`
}

// VerifyCheckpoint records a check made on a host other than the writer.
func (s *Service) VerifyCheckpoint(ctx context.Context, p model.Principal, runID, id string, in CheckpointCheck) (model.RunCheckpoint, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsWrite, "training_run.checkpoint_verify", "training_run", runID); err != nil {
		return model.RunCheckpoint{}, err
	}
	c, err := s.deps.Store.Checkpoint(ctx, p.TenantID, id)
	if err != nil || c.RunID != runID {
		return model.RunCheckpoint{}, model.ErrCheckpointNotFound
	}
	host := strings.TrimSpace(in.Host)
	if host == "" || len(host) > 255 {
		return model.RunCheckpoint{}, fmt.Errorf("%w: host names the machine that re-read the files", ErrInvalid)
	}
	if strings.EqualFold(host, c.WrittenBy) {
		return model.RunCheckpoint{}, model.ErrSameHost
	}
	verifyError := ""
	if len(in.Failures) > 0 {
		verifyError = clip(fmt.Sprintf("%d files did not read back: %s (%s)", len(in.Failures), in.Failures[0].Path, in.Failures[0].Reason), 1024)
	}
	if err := s.deps.Store.VerifyCheckpoint(ctx, p.TenantID, id, host, verifyError, s.now()); err != nil {
		return model.RunCheckpoint{}, err
	}
	s.record(ctx, p, "training_run.checkpoint_verify", "training_run", runID,
		map[string]any{"checkpoint_id": id, "host": host, "ok": verifyError == ""})
	return s.deps.Store.Checkpoint(ctx, p.TenantID, id)
}

// Checkpoints lists a run's checkpoints, newest step first, and the last
// verified one: the step to roll back to.
func (s *Service) Checkpoints(ctx context.Context, p model.Principal, runID string) ([]model.RunCheckpoint, *model.RunCheckpoint, error) {
	if _, err := s.deps.Datasets.TrainingRun(ctx, p, runID); err != nil {
		return nil, nil, err
	}
	list, err := s.deps.Store.Checkpoints(ctx, p.TenantID, runID, 200)
	if err != nil {
		return nil, nil, err
	}
	for i := range list {
		if list[i].VerifiedAt != nil {
			return list, &list[i], nil
		}
	}
	return list, nil, nil
}

func (s *Service) record(ctx context.Context, p model.Principal, action, kind, id string, details map[string]any) {
	if s.deps.Audit == nil {
		return
	}
	e := audit.From(ctx, p, action, kind, id)
	e.Details = details
	s.deps.Audit.Record(ctx, e)
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// IsInvalid reports a request the caller can fix.
func IsInvalid(err error) bool { return errors.Is(err, ErrInvalid) }
