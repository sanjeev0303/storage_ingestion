// Package deltaupload lets a client send only what a file does not share with a
// snapshot it can read (docs/chunks.md). The server builds the file from those
// chunks and what it holds, into an ordinary upload, so the file goes through
// the same SHA-256 check, malware scan and publication as any other.
package deltaupload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"regexp"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/dto/request"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/chunkread"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
	"github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

const (
	maxChunkBytes = 16 << 20
	maxChunks     = 1_000_000
	// An upload nobody finishes gives its chunks' space back.
	openFor       = 24 * time.Hour
	assembleLease = 2 * time.Minute
	assembleTries = 3
)

// Router finds an organization's storage.
type Router interface {
	Locate(ctx context.Context, tenantID, connectionID string) (interfaces.StorageLocation, error)
}

type Deps struct {
	Store    *postgres.DeltaUploadRepository
	Datasets *dataset.Service
	Uploads  *uploadservice.Service
	Assets   interface {
		GetByID(ctx context.Context, tenantID, assetID string) (*model.Asset, error)
	}
	Packs  interfaces.PackLocator
	Router Router
	Audit  interfaces.AuditRecorder
	Log    *slog.Logger
}

type Service struct {
	deps Deps
	now  func() time.Time
}

func New(deps Deps) *Service {
	return &Service{deps: deps, now: func() time.Time { return time.Now().UTC() }}
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{model.ErrInvalidDelta}, args...)...)
}

var hexSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ChunkInput is one chunk of the new file, as the client cut it.
type ChunkInput struct {
	Offset int64  `json:"offset"`
	Length int    `json:"length"`
	SHA256 string `json:"sha256"`
}

// CreateInput describes the new file.
type CreateInput struct {
	DatasetID     string       `json:"dataset_id"`
	BaseVersionID string       `json:"base_version_id"`
	Path          string       `json:"path"`
	SizeBytes     int64        `json:"size_bytes"`
	SHA256        string       `json:"sha256"`
	ContentType   string       `json:"content_type"`
	Chunks        []ChunkInput `json:"chunks"`
}

// Created says which chunks the client has to send.
type Created struct {
	ID          string `json:"id"`
	Missing     []int  `json:"missing"`
	ReusedBytes int64  `json:"reused_bytes"`
	SentBytes   int64  `json:"sent_bytes"`
}

func checkInput(in CreateInput) error {
	switch {
	case in.Path == "" || len(in.Path) > 1024:
		return invalid("a path of 1 to 1024 characters")
	case in.SizeBytes <= 0:
		return invalid("size_bytes is above 0")
	case !hexSHA256.MatchString(in.SHA256):
		return invalid("sha256 is 64 lower-case hex characters")
	case len(in.Chunks) == 0 || len(in.Chunks) > maxChunks:
		return invalid("1 to %d chunks", maxChunks)
	}
	var at int64
	for i, c := range in.Chunks {
		if c.Offset != at || c.Length <= 0 || c.Length > maxChunkBytes || !hexSHA256.MatchString(c.SHA256) {
			return invalid("chunk %d: chunks follow each other from byte 0, are 1 to %d bytes, and have a SHA-256", i, maxChunkBytes)
		}
		at += int64(c.Length)
	}
	if at != in.SizeBytes {
		return invalid("the chunks cover %d bytes of %d", at, in.SizeBytes)
	}
	return nil
}

// Create starts a delta upload into the base version's dataset. Reading the
// base snapshot is what entitles a client to name its chunks: a hash found
// anywhere else gets nothing, so knowing one gives no way to its bytes.
func (s *Service) Create(ctx context.Context, p model.Principal, in CreateInput) (Created, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermUploadsWrite, "delta_upload.create", "delta_upload", ""); err != nil {
		return Created{}, err
	}
	if err := checkInput(in); err != nil {
		return Created{}, err
	}
	ds, err := s.deps.Datasets.Get(ctx, p, in.DatasetID)
	if err != nil {
		return Created{}, err
	}
	base, err := s.deps.Datasets.Version(ctx, p, ds.ID, in.BaseVersionID)
	if err != nil {
		return Created{}, err
	}
	if !p.Scope.AllowsUpload(ds.Name, in.Path) {
		return Created{}, model.ErrForbidden
	}
	hashes := make([][]byte, 0, len(in.Chunks))
	seen := map[string]bool{}
	for _, c := range in.Chunks {
		if seen[c.SHA256] {
			continue
		}
		seen[c.SHA256] = true
		raw, _ := hex.DecodeString(c.SHA256) // checked above
		hashes = append(hashes, raw)
	}
	sources, err := s.deps.Store.ReuseSources(ctx, p.TenantID, base.ID, hashes)
	if err != nil {
		return Created{}, err
	}
	connection := ds.StorageConnectionID
	if connection == "" {
		connection = model.PlatformConnectionID
	}
	now := s.now()
	contentType := in.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	d := model.DeltaUpload{ID: event.NewID(), TenantID: p.TenantID, OwnerID: p.UserID, DatasetID: ds.ID, Namespace: ds.Name, BaseVersionID: base.ID,
		Path: in.Path, SizeBytes: in.SizeBytes, SHA256: in.SHA256, ContentType: contentType, ConnectionID: connection, CreatedAt: now, ExpiresAt: now.Add(openFor)}
	chunks := make([]model.DeltaChunk, len(in.Chunks))
	out := Created{ID: d.ID, Missing: []int{}}
	for i, c := range in.Chunks {
		chunks[i] = model.DeltaChunk{Ordinal: i, Offset: c.Offset, Length: c.Length, SHA256: c.SHA256}
		if src, ok := sources[c.SHA256]; ok {
			chunks[i].SourceAsset, chunks[i].SourceOffset = src.AssetID, src.Offset
			d.ReusedBytes += int64(c.Length)
			continue
		}
		d.SentBytes += int64(c.Length)
		out.Missing = append(out.Missing, i)
	}
	out.ReusedBytes, out.SentBytes = d.ReusedBytes, d.SentBytes
	if err := s.deps.Store.Create(ctx, d, chunks); err != nil {
		return Created{}, err
	}
	s.record(ctx, p, "delta_upload.create", d.ID, map[string]any{"dataset_id": ds.ID, "path": in.Path, "base_version_id": base.ID,
		"size_bytes": in.SizeBytes, "reused_bytes": d.ReusedBytes, "sent_bytes": d.SentBytes})
	return out, nil
}

func (s *Service) record(ctx context.Context, p model.Principal, action, id string, details map[string]any) {
	if s.deps.Audit == nil {
		return
	}
	e := audit.From(ctx, p, action, "delta_upload", id)
	e.Details = details
	s.deps.Audit.Record(ctx, e)
}

// mine finds one of p's own open uploads: someone else's is not found.
func (s *Service) mine(ctx context.Context, p model.Principal, id string) (model.DeltaUpload, error) {
	if err := p.Require(model.PermUploadsWrite); err != nil {
		return model.DeltaUpload{}, err
	}
	d, err := s.deps.Store.Get(ctx, p.TenantID, id)
	if err != nil {
		return d, err
	}
	if d.OwnerID != p.UserID {
		return d, model.ErrDeltaNotFound
	}
	return d, nil
}

func stagedKey(prefix string, d model.DeltaUpload, ordinal int) string {
	return fmt.Sprintf("%s/%s/delta/%s/%d", prefix, d.TenantID, d.ID, ordinal)
}

// PutChunk takes one chunk the server asked for, refused unless it is exactly
// the chunk promised.
func (s *Service) PutChunk(ctx context.Context, p model.Principal, id string, ordinal int, body io.Reader) error {
	d, err := s.mine(ctx, p, id)
	if err != nil {
		return err
	}
	if d.State != model.DeltaOpen {
		return fmt.Errorf("%w: this upload is %s", model.ErrDeltaState, d.State)
	}
	c, err := s.deps.Store.Chunk(ctx, id, ordinal)
	if err != nil {
		return err
	}
	if c.SourceAsset != "" {
		return invalid("chunk %d is not needed", ordinal)
	}
	data, err := io.ReadAll(io.LimitReader(body, int64(c.Length)+1))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if len(data) != c.Length || hex.EncodeToString(sum[:]) != c.SHA256 {
		return invalid("chunk %d is not the %d bytes with the SHA-256 promised", ordinal, c.Length)
	}
	loc, err := s.deps.Router.Locate(ctx, d.TenantID, d.ConnectionID)
	if err != nil {
		return err
	}
	if _, err := loc.Storage.WriteObject(ctx, loc.StagingBucket, stagedKey(loc.StagingPrefix, d, ordinal), bytes.NewReader(data), int64(len(data)), "application/octet-stream"); err != nil {
		return fmt.Errorf("store chunk: %w", err)
	}
	return s.deps.Store.MarkReceived(ctx, id, ordinal)
}

// Complete hands the upload to the worker once every chunk has arrived.
func (s *Service) Complete(ctx context.Context, p model.Principal, id string) (model.DeltaUpload, error) {
	d, err := s.mine(ctx, p, id)
	if err != nil {
		return d, err
	}
	if d.State != model.DeltaOpen {
		return d, nil // completing again answers where it got to
	}
	missing, err := s.deps.Store.MissingChunks(ctx, id)
	if err != nil {
		return d, err
	}
	if len(missing) > 0 {
		return d, fmt.Errorf("%w: %d chunks have not arrived", model.ErrDeltaState, len(missing))
	}
	if err := s.deps.Store.Queue(ctx, id); err != nil {
		return d, err
	}
	s.record(ctx, p, "delta_upload.complete", id, nil)
	d.State = model.DeltaQueued
	return d, nil
}

// Status is how an upload is going, with the asset it became.
type Status struct {
	model.DeltaUpload
	Asset *model.Asset `json:"-"`
}

// Get answers how an upload is going, and the asset it became.
func (s *Service) Get(ctx context.Context, p model.Principal, id string) (model.DeltaUpload, *model.Asset, error) {
	d, err := s.mine(ctx, p, id)
	if err != nil {
		return d, nil, err
	}
	if d.UploadID == "" {
		return d, nil, nil
	}
	asset, err := s.deps.Uploads.AssetOf(ctx, p.TenantID, d.UploadID)
	return d, asset, err
}

// RunOnce assembles the oldest queued upload, and clears out abandoned ones.
// Answers whether there was one to assemble.
func (s *Service) RunOnce(ctx context.Context, owner string) (bool, error) {
	expired, err := s.deps.Store.Expired(ctx, s.now(), 50)
	if err != nil {
		return false, err
	}
	for _, d := range expired {
		s.dropStaged(ctx, d)
		if err := s.deps.Store.Finish(ctx, d.ID, model.DeltaExpired, "", "nobody finished it in time", s.now()); err != nil {
			return false, err
		}
	}
	d, ok, err := s.deps.Store.Claim(ctx, owner, assembleLease, assembleTries)
	if err != nil || !ok {
		return false, err
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(assembleLease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_ = s.deps.Store.Extend(ctx, d.ID, owner, assembleLease)
			}
		}
	}()
	uploadID, err := s.assemble(ctx, d)
	if err != nil {
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		// Bytes that do not hash as promised will not on a second try either;
		// anything else may. The lease runs out and another pass tries.
		if errors.Is(err, model.ErrChunkCorrupt) || d.Attempts >= assembleTries {
			s.dropStaged(ctx, d)
			return true, s.deps.Store.Finish(ctx, d.ID, model.DeltaFailed, "", err.Error(), s.now())
		}
		return true, fmt.Errorf("assemble %s: %w", d.ID, err)
	}
	s.dropStaged(ctx, d)
	return true, s.deps.Store.Finish(ctx, d.ID, model.DeltaDone, uploadID, "", s.now())
}

func (s *Service) dropStaged(ctx context.Context, d model.DeltaUpload) {
	chunks, err := s.deps.Store.Chunks(ctx, d.ID)
	if err != nil {
		s.deps.Log.Warn("could not list a delta upload's chunks", "id", d.ID, "error", err)
		return
	}
	loc, err := s.deps.Router.Locate(ctx, d.TenantID, d.ConnectionID)
	if err != nil {
		s.deps.Log.Warn("could not reach a delta upload's chunks", "id", d.ID, "error", err)
		return
	}
	for _, c := range chunks {
		if c.SourceAsset != "" || !c.Received {
			continue
		}
		if err := loc.Storage.DeleteObject(ctx, loc.StagingBucket, stagedKey(loc.StagingPrefix, d, c.Ordinal)); err != nil {
			s.deps.Log.Warn("could not delete a delta upload's chunk", "id", d.ID, "error", err)
		}
	}
}

// assemble streams the new file into an ordinary upload, checking every chunk
// as it goes and the whole at the end. A bad byte fails the write, and nothing
// is completed.
func (s *Service) assemble(ctx context.Context, d model.DeltaUpload) (string, error) {
	connection := d.ConnectionID
	if model.IsPlatformConnection(connection) {
		connection = ""
	}
	created, err := s.deps.Uploads.Create(ctx, uploadservice.CreateInput{TenantID: d.TenantID, OwnerID: d.OwnerID, Dataset: d.Namespace,
		Request: request.CreateUploadRequest{FileName: path.Base(d.Path), ContentType: d.ContentType, SizeBytes: d.SizeBytes, ChecksumSHA256: d.SHA256,
			IdempotencyKey: "delta-" + d.ID, Dataset: d.Namespace, Path: d.Path, StorageConnectionID: connection}})
	if err != nil {
		return "", fmt.Errorf("start the upload: %w", err)
	}
	session := created.Upload
	if session.Status == model.UploadStatusPending {
		dst, err := s.deps.Router.Locate(ctx, d.TenantID, session.StagingConnectionID)
		if err != nil {
			return "", err
		}
		chunks, err := s.deps.Store.Chunks(ctx, d.ID)
		if err != nil {
			return "", err
		}
		reader, writer := io.Pipe()
		built := make(chan error, 1)
		go func() {
			err := s.stream(ctx, d, chunks, writer)
			_ = writer.CloseWithError(err) // nil closes it cleanly
			built <- err
		}()
		_, writeErr := dst.Storage.WriteObject(ctx, dst.StagingBucket, session.ObjectKey, reader, d.SizeBytes, d.ContentType)
		_ = reader.CloseWithError(writeErr)
		if buildErr := <-built; buildErr != nil && !errors.Is(buildErr, io.ErrClosedPipe) {
			return "", buildErr
		}
		if writeErr != nil {
			return "", fmt.Errorf("write the file: %w", writeErr)
		}
	}
	if _, err := s.deps.Uploads.Complete(ctx, d.TenantID, session.ID); err != nil {
		return "", fmt.Errorf("complete the upload: %w", err)
	}
	return session.ID, nil
}

func (s *Service) stream(ctx context.Context, d model.DeltaUpload, chunks []model.DeltaChunk, w io.Writer) error {
	loc, err := s.deps.Router.Locate(ctx, d.TenantID, d.ConnectionID)
	if err != nil {
		return err
	}
	whole := sha256.New()
	out := io.MultiWriter(w, whole)
	buf := make([]byte, 0, 8<<20)
	var open io.ReadCloser
	var openAsset string
	var openNext int64
	closeOpen := func() {
		if open != nil {
			open.Close()
			open = nil
		}
	}
	defer closeOpen()
	for _, c := range chunks {
		if cap(buf) < c.Length {
			buf = make([]byte, c.Length)
		}
		buf = buf[:c.Length]
		if c.SourceAsset == "" {
			closeOpen()
			body, err := loc.Storage.OpenRange(ctx, loc.StagingBucket, stagedKey(loc.StagingPrefix, d, c.Ordinal), 0, -1, interfaces.ReadCondition{})
			if err != nil {
				return fmt.Errorf("read a received chunk: %w", err)
			}
			_, err = io.ReadFull(body, buf)
			body.Close()
			if err != nil {
				return fmt.Errorf("read a received chunk: %w", err)
			}
		} else {
			// Chunks in a row from one place are one read, however many.
			if open == nil || openAsset != c.SourceAsset || openNext != c.SourceOffset {
				closeOpen()
				body, err := s.openAsset(ctx, d.TenantID, c.SourceAsset, c.SourceOffset)
				if err != nil {
					return fmt.Errorf("read the base snapshot: %w", err)
				}
				open, openAsset = body, c.SourceAsset
			}
			if _, err := io.ReadFull(open, buf); err != nil {
				return fmt.Errorf("read the base snapshot: %w", err)
			}
			openNext = c.SourceOffset + int64(c.Length)
		}
		sum := sha256.Sum256(buf)
		if hex.EncodeToString(sum[:]) != c.SHA256 {
			return fmt.Errorf("%w: the bytes for chunk %d are not the ones promised", model.ErrChunkCorrupt, c.Ordinal)
		}
		if _, err := out.Write(buf); err != nil {
			return err
		}
	}
	if got := hex.EncodeToString(whole.Sum(nil)); got != d.SHA256 {
		return fmt.Errorf("%w: the file assembled hashes to %s", model.ErrChunkCorrupt, got)
	}
	return nil
}

// openAsset reads a published file from first on, wherever its bytes are.
func (s *Service) openAsset(ctx context.Context, tenantID, assetID string, first int64) (io.ReadCloser, error) {
	asset, err := s.deps.Assets.GetByID(ctx, tenantID, assetID)
	if err != nil {
		return nil, err
	}
	if asset.Status != model.AssetStatusAvailable {
		return nil, fmt.Errorf("the file is %s", asset.Status)
	}
	loc, err := s.deps.Router.Locate(ctx, tenantID, asset.StorageConnectionID)
	if err != nil {
		return nil, err
	}
	if s.deps.Packs != nil {
		packed, ok, err := s.deps.Packs.PackedLocation(ctx, tenantID, assetID)
		if err != nil {
			return nil, err
		}
		if ok {
			start, end, err := packed.Range(first, -1)
			if err != nil {
				return nil, err
			}
			return loc.Storage.OpenRange(ctx, packed.Bucket, packed.ObjectKey, start, end, interfaces.ReadCondition{IfMatch: packed.StorageETag, VersionID: packed.StorageVersionID})
		}
		extents, chunked, err := interfaces.ChunkedExtents(ctx, s.deps.Packs, tenantID, assetID)
		if err != nil {
			return nil, err
		}
		if chunked {
			return chunkread.Open(ctx, loc.Storage, loc.ReadyBucket, extents, first, -1)
		}
	}
	return loc.Storage.OpenRange(ctx, loc.ReadyBucket, asset.ObjectKey, first, asset.SizeBytes-1, interfaces.ReadCondition{VersionID: asset.StorageVersionID})
}
