// Package edge serves instruments and robots that upload through an agent
// (docs/edge.md): devices with upload-only credentials confined to a path,
// runs that snapshot only when every hash matches, recordings that grow chunk
// by chunk with their gaps known, and heartbeats that say what is waiting.
package edge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/access"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
	"github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
)

type Deps struct {
	Store    *postgres.EdgeRepository
	Access   *access.Service
	Datasets *dataset.Service
	Audit    interfaces.AuditRecorder
	Log      *slog.Logger
}

type Service struct {
	deps Deps
	now  func() time.Time
}

func New(deps Deps) *Service {
	return &Service{deps: deps, now: func() time.Time { return time.Now().UTC() }}
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{model.ErrInvalidEdge}, args...)...)
}

func (s *Service) record(ctx context.Context, p model.Principal, action, kind, id string, details map[string]any) {
	if s.deps.Audit == nil {
		return
	}
	e := audit.From(ctx, p, action, kind, id)
	e.Details = details
	s.deps.Audit.Record(ctx, e)
}

// A device's credential lasts a year, and is replaced by registering again.
const deviceTokenLife = 365 * 24 * time.Hour

// Register makes a device and its credential: it may upload, only into its
// dataset's namespace and only under its path prefix. It can read nothing
// and delete nothing. The token is shown once.
func (s *Service) Register(ctx context.Context, p model.Principal, name, datasetID, prefix string) (model.EdgeDevice, string, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermOrgManage, "edge.device_register", "edge_device", ""); err != nil {
		return model.EdgeDevice{}, "", err
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return model.EdgeDevice{}, "", invalid("a name of 1 to 100 characters")
	}
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		prefix = "devices/" + regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(name, "-")
	}
	if strings.Contains(prefix, "..") || len(prefix) > 500 {
		return model.EdgeDevice{}, "", invalid("a path prefix without .., of at most 500 characters")
	}
	ds, err := s.deps.Datasets.Get(ctx, p, datasetID)
	if err != nil {
		return model.EdgeDevice{}, "", err
	}
	t, secret, err := s.deps.Access.CreateTokenFor(ctx, p, access.TokenInput{Name: "Device: " + name, Permissions: []string{"uploads.write"},
		Datasets: []string{ds.Name}, PathPrefix: prefix + "/", ExpiresIn: deviceTokenLife})
	if err != nil {
		return model.EdgeDevice{}, "", err
	}
	d := model.EdgeDevice{ID: event.NewID(), TenantID: p.TenantID, Name: name, DatasetID: ds.ID, Namespace: ds.Name, PathPrefix: prefix + "/", TokenID: t.ID,
		CreatedBy: p.UserID, CreatedAt: s.now()}
	if err := s.deps.Store.AddDevice(ctx, d); err != nil {
		_ = s.deps.Access.RevokeTokenFor(ctx, p, t.ID)
		return d, "", err
	}
	s.record(ctx, p, "edge.device_register", "edge_device", d.ID, map[string]any{"name": name, "dataset_id": ds.ID, "path_prefix": d.PathPrefix})
	return d, secret, nil
}

func (s *Service) Devices(ctx context.Context, p model.Principal) ([]model.EdgeDevice, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermOrgRead, "edge.read", "edge_device", ""); err != nil {
		return nil, err
	}
	return s.deps.Store.Devices(ctx, p.TenantID)
}

// Revoke ends a device: its credential stops working at once.
func (s *Service) Revoke(ctx context.Context, p model.Principal, id string) error {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermOrgManage, "edge.device_revoke", "edge_device", id); err != nil {
		return err
	}
	d, err := s.deps.Store.Device(ctx, p.TenantID, id)
	if err != nil {
		return err
	}
	if err := s.deps.Access.RevokeTokenFor(ctx, p, d.TokenID); err != nil && !errors.Is(err, model.ErrAPITokenNotFound) {
		return err
	}
	if err := s.deps.Store.RevokeDevice(ctx, p.TenantID, id); err != nil {
		return err
	}
	s.record(ctx, p, "edge.device_revoke", "edge_device", id, nil)
	return nil
}

// RecordingDetail is a recording with its chunks and gaps.
type RecordingDetail struct {
	model.EdgeRecording
	ChunkList []model.EdgeChunk `json:"chunk_list"`
	Gaps      []model.EdgeGap   `json:"gaps"`
}

func (s *Service) Recordings(ctx context.Context, p model.Principal, deviceID string) ([]model.EdgeRecording, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermOrgRead, "edge.read", "edge_recording", ""); err != nil {
		return nil, err
	}
	return s.deps.Store.Recordings(ctx, p.TenantID, deviceID, 200)
}

func (s *Service) Recording(ctx context.Context, p model.Principal, id string) (RecordingDetail, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermOrgRead, "edge.read", "edge_recording", id); err != nil {
		return RecordingDetail{}, err
	}
	return s.detail(ctx, p.TenantID, id)
}

func (s *Service) detail(ctx context.Context, tenantID, id string) (RecordingDetail, error) {
	e, err := s.deps.Store.Recording(ctx, tenantID, id)
	if err != nil {
		return RecordingDetail{}, err
	}
	chunks, err := s.deps.Store.Chunks(ctx, tenantID, id)
	if err != nil {
		return RecordingDetail{}, err
	}
	return RecordingDetail{EdgeRecording: e, ChunkList: chunks, Gaps: gapsOf(chunks, time.Duration(e.GapSeconds)*time.Second)}, nil
}

// gapsOf finds time no chunk covers. Chunks come ordered by start.
func gapsOf(chunks []model.EdgeChunk, tolerance time.Duration) []model.EdgeGap {
	gaps := []model.EdgeGap{}
	var end *time.Time
	for _, c := range chunks {
		if c.StartsAt == nil || c.EndsAt == nil {
			continue
		}
		if end != nil && c.StartsAt.Sub(*end) > tolerance {
			gaps = append(gaps, model.EdgeGap{From: *end, To: *c.StartsAt})
		}
		if end == nil || c.EndsAt.After(*end) {
			end = c.EndsAt
		}
	}
	return gaps
}

// ─── What a device calls, with its own credential ─────────────────────

func (s *Service) deviceOf(ctx context.Context, p model.Principal) (model.EdgeDevice, error) {
	if p.Scope.TokenID == "" {
		return model.EdgeDevice{}, model.ErrNotADevice
	}
	d, err := s.deps.Store.DeviceOfToken(ctx, p.TenantID, p.Scope.TokenID)
	if errors.Is(err, model.ErrDeviceNotFound) {
		return d, model.ErrNotADevice
	}
	return d, err
}

// Me is the device a credential belongs to: its namespace and prefix.
func (s *Service) Me(ctx context.Context, p model.Principal) (model.EdgeDevice, error) {
	return s.deviceOf(ctx, p)
}

func (s *Service) Heartbeat(ctx context.Context, p model.Principal, h model.EdgeHeartbeat) error {
	d, err := s.deviceOf(ctx, p)
	if err != nil {
		return err
	}
	if len(h.AgentVersion) > 50 {
		h.AgentVersion = h.AgentVersion[:50]
	}
	if len(h.Note) > 500 {
		h.Note = h.Note[:500]
	}
	return s.deps.Store.Heartbeat(ctx, d, h)
}

// FileState is what a device may do with its local copy.
type FileState struct {
	State  string `json:"state"` // AVAILABLE, PROCESSING, REJECTED, MISSING
	Reason string `json:"reason,omitempty"`
}

// Confirm says whether a file arrived with exactly these bytes. A device
// deletes its local copy only on AVAILABLE, which means the platform computed
// this SHA-256 from the stored bytes and its checks passed.
func (s *Service) Confirm(ctx context.Context, p model.Principal, path, sha256 string) (FileState, error) {
	d, err := s.deviceOf(ctx, p)
	if err != nil {
		return FileState{}, err
	}
	if !strings.HasPrefix(path, d.PathPrefix) {
		return FileState{}, model.ErrOutsideDevicePath
	}
	f, found, err := s.deps.Store.FileAt(ctx, p.TenantID, d.Namespace, path, sha256)
	if err != nil {
		return FileState{}, err
	}
	if !found {
		return FileState{State: "MISSING"}, nil
	}
	switch model.AssetStatus(f.Status) {
	case model.AssetStatusAvailable:
		return FileState{State: "AVAILABLE"}, nil
	case model.AssetStatusRejected, model.AssetStatusBlocked:
		// A blocked file is refused like a rejected one: sending it again
		// gives the same bytes.
		return FileState{State: "REJECTED", Reason: f.Reason}, nil
	case model.AssetStatusSoftDeleted:
		return FileState{State: "MISSING"}, nil
	}
	return FileState{State: "PROCESSING"}, nil
}

// OpenRecording starts a run or recording, or answers the one with its name.
func (s *Service) OpenRecording(ctx context.Context, p model.Principal, name, kind string, gapSeconds int) (model.EdgeRecording, error) {
	d, err := s.deviceOf(ctx, p)
	if err != nil {
		return model.EdgeRecording{}, err
	}
	name, kind = strings.TrimSpace(name), strings.ToUpper(strings.TrimSpace(kind))
	if name == "" || len(name) > 300 {
		return model.EdgeRecording{}, invalid("a name of 1 to 300 characters")
	}
	if kind != model.RecordingRun && kind != model.RecordingRecording {
		return model.EdgeRecording{}, invalid("kind is RUN or RECORDING")
	}
	if gapSeconds < 0 {
		return model.EdgeRecording{}, invalid("gap_seconds must not be negative")
	}
	if gapSeconds == 0 {
		gapSeconds = 5
	}
	return s.deps.Store.OpenRecording(ctx, model.EdgeRecording{ID: event.NewID(), TenantID: p.TenantID, DeviceID: d.ID, Name: name, Kind: kind,
		GapSeconds: gapSeconds, CreatedAt: s.now()})
}

func (s *Service) ownRecording(ctx context.Context, p model.Principal, id string) (model.EdgeDevice, model.EdgeRecording, error) {
	d, err := s.deviceOf(ctx, p)
	if err != nil {
		return d, model.EdgeRecording{}, err
	}
	e, err := s.deps.Store.Recording(ctx, p.TenantID, id)
	if err != nil {
		return d, e, err
	}
	if e.DeviceID != d.ID {
		return d, e, model.ErrDeviceNotFound
	}
	return d, e, nil
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// AddChunk adds a file to a run, or a time window to a recording. It may
// arrive late and out of order, and sent twice it counts once. A chunk that
// closes a gap sends edge.backfill_closed.
func (s *Service) AddChunk(ctx context.Context, p model.Principal, recordingID string, c model.EdgeChunk) (RecordingDetail, error) {
	d, e, err := s.ownRecording(ctx, p, recordingID)
	if err != nil {
		return RecordingDetail{}, err
	}
	switch {
	case e.State == model.RecordingComplete:
		return RecordingDetail{}, model.ErrRecordingComplete
	case !strings.HasPrefix(c.Path, d.PathPrefix):
		return RecordingDetail{}, model.ErrOutsideDevicePath
	case !sha256Hex.MatchString(c.SHA256) || c.SizeBytes < 0:
		return RecordingDetail{}, invalid("a chunk has a SHA-256 in hex and a size")
	case (c.StartsAt == nil) != (c.EndsAt == nil) || (c.StartsAt != nil && c.EndsAt.Before(*c.StartsAt)):
		return RecordingDetail{}, invalid("a time window has a start and an end after it")
	}
	before, err := s.detail(ctx, p.TenantID, recordingID)
	if err != nil {
		return before, err
	}
	c.AddedAt = s.now()
	if err := s.deps.Store.AddChunk(ctx, p.TenantID, recordingID, c); err != nil {
		return before, err
	}
	after, err := s.detail(ctx, p.TenantID, recordingID)
	if err != nil {
		return after, err
	}
	if len(after.Gaps) < len(before.Gaps) {
		if err := s.deps.Store.QueueBackfill(ctx, p.TenantID, e, c, len(after.Gaps)); err != nil {
			return after, err
		}
		s.record(ctx, p, "edge.backfill_closed", "edge_recording", recordingID, map[string]any{"path": c.Path, "gaps_left": len(after.Gaps)})
	}
	return after, nil
}

// Close ends a run or recording. It becomes COMPLETE, and its dataset gets a
// snapshot, only when the device says nothing is missing and every chunk
// is published with exactly its SHA-256. Otherwise it is INCOMPLETE, saying
// what is missing, with no snapshot; closing it again later may complete it.
func (s *Service) Close(ctx context.Context, p model.Principal, recordingID string, expected int64, missing []string) (RecordingDetail, error) {
	d, e, err := s.ownRecording(ctx, p, recordingID)
	if err != nil {
		return RecordingDetail{}, err
	}
	if e.State == model.RecordingComplete {
		return s.detail(ctx, p.TenantID, recordingID)
	}
	chunks, err := s.deps.Store.Chunks(ctx, p.TenantID, recordingID)
	if err != nil {
		return RecordingDetail{}, err
	}
	problems := append([]string{}, missing...)
	if expected > 0 && int64(len(chunks)) != expected {
		problems = append(problems, fmt.Sprintf("%d files expected, %d reported", expected, len(chunks)))
	}
	if len(chunks) == 0 {
		problems = append(problems, "no files")
	}
	service := model.ServicePrincipal(p.TenantID, "edge:"+d.ID)
	inputs := make([]dataset.MemberInput, 0, len(chunks))
	for _, c := range chunks {
		f, found, err := s.deps.Store.FileAt(ctx, p.TenantID, d.Namespace, c.Path, c.SHA256)
		if err != nil {
			return RecordingDetail{}, err
		}
		switch {
		case !found:
			problems = append(problems, c.Path+": not uploaded")
		case model.AssetStatus(f.Status) != model.AssetStatusAvailable:
			problems = append(problems, c.Path+": "+strings.ToLower(f.Status)+" "+f.Reason)
		default:
			inputs = append(inputs, dataset.MemberInput{ObjectID: f.ObjectID, VersionID: f.AssetID,
				Metadata: map[string]string{"edge_device": d.Name, "edge_recording": e.Name}})
		}
	}
	now := s.now()
	e.Missing, e.ClosedAt = problems, &now
	if len(problems) > 0 {
		if len(e.Missing) > 1000 {
			e.Missing = e.Missing[:1000]
		}
		e.State = model.RecordingIncomplete
		if err := s.deps.Store.CloseRecording(ctx, e, len(chunks)); errors.Is(err, model.ErrRecordingComplete) {
			// Another close completed it meanwhile.
			return s.detail(ctx, p.TenantID, recordingID)
		} else if err != nil {
			return RecordingDetail{}, err
		}
		s.record(ctx, p, "edge.recording_incomplete", "edge_recording", recordingID, map[string]any{"problems": len(problems)})
		return s.detail(ctx, p.TenantID, recordingID)
	}
	if _, err := s.deps.Datasets.AddMembers(ctx, service, d.DatasetID, inputs, model.DuplicateReplace); err != nil {
		return RecordingDetail{}, err
	}
	// The key names the chunk count too: a close run again after a chunk
	// arrived gets a snapshot with it, not the one made without it.
	v, _, err := s.deps.Datasets.Snapshot(ctx, service, d.DatasetID, fmt.Sprintf("edge-%s-%d", e.ID, len(chunks)))
	if err != nil {
		return RecordingDetail{}, err
	}
	// Two closes racing here make one snapshot (the idempotency key is the
	// recording's), and the second finds the recording COMPLETE.
	e.State, e.VersionID, e.Missing = model.RecordingComplete, v.ID, []string{}
	if err := s.deps.Store.CloseRecording(ctx, e, len(chunks)); errors.Is(err, model.ErrRecordingComplete) {
		return s.detail(ctx, p.TenantID, recordingID)
	} else if errors.Is(err, model.ErrRecordingChanged) {
		// A chunk arrived after the chunks were read: close again with it.
		return s.Close(ctx, p, recordingID, expected, missing)
	} else if err != nil {
		return RecordingDetail{}, err
	}
	s.record(ctx, p, "edge.recording_complete", "edge_recording", recordingID, map[string]any{"version_id": v.ID, "files": len(chunks)})
	return s.detail(ctx, p.TenantID, recordingID)
}
