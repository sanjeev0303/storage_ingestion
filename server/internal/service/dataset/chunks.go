package dataset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/fastcdc"
)

// The chunk index (docs/chunks.md): FastCDC chunks of each file, read by the
// worker on datasets that ask for it, so that dedup savings can be measured
// and a new version fetched as the difference from one already held.

// ChunkStore keeps the index.
type ChunkStore interface {
	SetEnabled(ctx context.Context, tenantID, datasetID string, on bool) error
	Enabled(ctx context.Context, tenantID, datasetID string) (bool, error)
	QueueNew(ctx context.Context, now time.Time) (int64, error)
	Requeue(ctx context.Context, tenantID, datasetID, versionID string, now time.Time) error
	Job(ctx context.Context, tenantID, versionID string) (model.ChunkJob, bool, error)
	Claim(ctx context.Context, owner string, lease time.Duration, maxAttempts int) (model.ChunkJob, bool, error)
	Extend(ctx context.Context, versionID, owner string, lease time.Duration) error
	Finish(ctx context.Context, owner string, j model.ChunkJob) error
	HasFile(ctx context.Context, tenantID, sha256 string) (bool, error)
	PutFile(ctx context.Context, tenantID, sha256 string, size int64, params string, chunks []model.RawChunk, now time.Time) error
	Manifest(ctx context.Context, tenantID, versionID, path string, after, limit int) (string, []model.FileChunk, bool, error)
	Savings(ctx context.Context, tenantID, datasetID string) (model.ChunkSavings, error)
	Delta(ctx context.Context, tenantID, fromID, toID string, largest int) (model.ChunkDelta, error)
	Prune(ctx context.Context) (int64, error)
}

// ChunkStorage turns chunk storage on and off, and says what it holds.
type ChunkStorage interface {
	SetEnabled(ctx context.Context, tenantID, datasetID string, on bool) error
	Stats(ctx context.Context, tenantID, datasetID string) (model.ChunkStorageStats, error)
	RehydrateDataset(ctx context.Context, tenantID, datasetID string, now time.Time) (int64, error)
	BlockingPolicy(ctx context.Context, tenantID, datasetID string) (string, error)
}

var ErrChunksOff = errors.New("the chunk index is not configured")

const (
	chunkLease    = 2 * time.Minute
	chunkAttempts = 5
)

func (s *Service) chunks() (ChunkStore, error) {
	if s.deps.Chunks == nil {
		return nil, ErrChunksOff
	}
	return s.deps.Chunks, nil
}

// SetChunking turns the index on or off for a dataset. On, the worker reads
// every file of every version once, which on a bucket of your own costs what
// reading it costs. Off keeps what was indexed and stops indexing more.
func (s *Service) SetChunking(ctx context.Context, p model.Principal, datasetID string, on bool) error {
	c, err := s.chunks()
	if err != nil {
		return err
	}
	if err := s.authorize(ctx, p, model.PermDatasetsManage, "dataset.chunking", "dataset", datasetID); err != nil {
		return err
	}
	if _, err := s.deps.Repo.GetDataset(ctx, p.TenantID, datasetID); err != nil {
		return err
	}
	if err := c.SetEnabled(ctx, p.TenantID, datasetID, on); err != nil {
		return err
	}
	s.record(ctx, p, "dataset.chunking", "dataset", datasetID, map[string]any{"enabled": on})
	return nil
}

// SetChunkStorage turns storing chunks once on or off for a dataset. Whatever
// must stay as one object where a provider's lock can reach it stays out.
func (s *Service) SetChunkStorage(ctx context.Context, p model.Principal, datasetID string, on bool) error {
	if s.deps.ChunkStorage == nil {
		return ErrChunksOff
	}
	if err := s.authorize(ctx, p, model.PermDatasetsManage, "dataset.chunk_storage", "dataset", datasetID); err != nil {
		return err
	}
	if _, err := s.deps.Repo.GetDataset(ctx, p.TenantID, datasetID); err != nil {
		return err
	}
	if on {
		reason, err := s.deps.ChunkStorage.BlockingPolicy(ctx, p.TenantID, datasetID)
		if err != nil {
			return err
		}
		if reason != "" {
			return fmt.Errorf("%w: %s, so its files stay whole objects", ErrInvalidRequest, reason)
		}
	}
	if err := s.deps.ChunkStorage.SetEnabled(ctx, p.TenantID, datasetID, on); err != nil {
		return err
	}
	s.record(ctx, p, "dataset.chunk_storage", "dataset", datasetID, map[string]any{"enabled": on})
	return nil
}

// ChunkStorageStats says what chunk storage holds for a dataset.
func (s *Service) ChunkStorageStats(ctx context.Context, p model.Principal, datasetID string) (model.ChunkStorageStats, error) {
	if s.deps.ChunkStorage == nil {
		return model.ChunkStorageStats{}, ErrChunksOff
	}
	if _, err := s.Get(ctx, p, datasetID); err != nil {
		return model.ChunkStorageStats{}, err
	}
	return s.deps.ChunkStorage.Stats(ctx, p.TenantID, datasetID)
}

// RehydrateDataset makes a dataset's chunked files whole objects again, and
// stops new conversions of it. Answers how many were asked for.
func (s *Service) RehydrateDataset(ctx context.Context, p model.Principal, datasetID string) (int64, error) {
	if s.deps.ChunkStorage == nil {
		return 0, ErrChunksOff
	}
	if err := s.authorize(ctx, p, model.PermDatasetsManage, "dataset.chunk_rehydrate", "dataset", datasetID); err != nil {
		return 0, err
	}
	if _, err := s.deps.Repo.GetDataset(ctx, p.TenantID, datasetID); err != nil {
		return 0, err
	}
	n, err := s.deps.ChunkStorage.RehydrateDataset(ctx, p.TenantID, datasetID, s.now())
	if err != nil {
		return n, err
	}
	s.record(ctx, p, "dataset.chunk_rehydrate", "dataset", datasetID, map[string]any{"files": n})
	return n, nil
}

// ChunkSavings is the report: what a dataset's versions would take with chunks
// stored once. It measures; bytes stay stored as whole objects.
func (s *Service) ChunkSavings(ctx context.Context, p model.Principal, datasetID string) (model.ChunkSavings, error) {
	c, err := s.chunks()
	if err != nil {
		return model.ChunkSavings{}, err
	}
	if _, err := s.Get(ctx, p, datasetID); err != nil {
		return model.ChunkSavings{}, err
	}
	out, err := c.Savings(ctx, p.TenantID, datasetID)
	if err != nil {
		return out, err
	}
	out.Enabled, err = c.Enabled(ctx, p.TenantID, datasetID)
	return out, err
}

// ChunkJobOf answers how indexing a version went.
func (s *Service) ChunkJobOf(ctx context.Context, p model.Principal, datasetID, versionID string) (model.ChunkJob, bool, error) {
	c, err := s.chunks()
	if err != nil {
		return model.ChunkJob{}, false, err
	}
	v, err := s.Version(ctx, p, datasetID, versionID)
	if err != nil {
		return model.ChunkJob{}, false, err
	}
	return c.Job(ctx, p.TenantID, v.ID)
}

// IndexVersion queues one version, whether or not the dataset has chunking on.
func (s *Service) IndexVersion(ctx context.Context, p model.Principal, datasetID, versionID string) error {
	c, err := s.chunks()
	if err != nil {
		return err
	}
	if err := s.authorize(ctx, p, model.PermDatasetsManage, "dataset.chunk_index", "dataset_version", versionID); err != nil {
		return err
	}
	v, err := s.Version(ctx, p, datasetID, versionID)
	if err != nil {
		return err
	}
	if err := c.Requeue(ctx, p.TenantID, v.DatasetID, v.ID, s.now()); err != nil {
		return err
	}
	s.record(ctx, p, "dataset.chunk_index", "dataset_version", v.ID, nil)
	return nil
}

// ChunkManifest answers a file's chunks, a page at a time. indexed is false
// when the file has not been indexed yet.
func (s *Service) ChunkManifest(ctx context.Context, p model.Principal, datasetID, versionID, path string, after, limit int) (string, []model.FileChunk, bool, error) {
	c, err := s.chunks()
	if err != nil {
		return "", nil, false, err
	}
	v, err := s.Version(ctx, p, datasetID, versionID)
	if err != nil {
		return "", nil, false, err
	}
	if limit <= 0 || limit > 10000 {
		limit = 10000
	}
	return c.Manifest(ctx, p.TenantID, v.ID, path, max(after, 0), limit)
}

// ChunkDelta counts what moving to a version from another of the organization's
// versions needs.
func (s *Service) ChunkDelta(ctx context.Context, p model.Principal, datasetID, versionID, fromID string) (model.ChunkDelta, error) {
	c, err := s.chunks()
	if err != nil {
		return model.ChunkDelta{}, err
	}
	v, err := s.Version(ctx, p, datasetID, versionID)
	if err != nil {
		return model.ChunkDelta{}, err
	}
	if fromID == "" {
		return model.ChunkDelta{}, fmt.Errorf("%w: name the version to compare from", ErrInvalidRequest)
	}
	from, err := s.Version(ctx, p, "", fromID)
	if err != nil {
		return model.ChunkDelta{}, err
	}
	return c.Delta(ctx, p.TenantID, from.ID, v.ID, 50)
}

// ChunkOnce indexes the oldest queued version, if any. Run by the dataset worker.
func (s *Service) ChunkOnce(ctx context.Context, owner string) (bool, error) {
	c, err := s.chunks()
	if err != nil {
		return false, err
	}
	if _, err := c.QueueNew(ctx, s.now()); err != nil {
		return false, err
	}
	job, ok, err := c.Claim(ctx, owner, chunkLease, chunkAttempts)
	if err != nil || !ok {
		return false, err
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(chunkLease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_ = c.Extend(ctx, job.VersionID, owner, chunkLease)
			}
		}
	}()
	out, err := s.chunkVersion(ctx, c, job)
	if err != nil {
		// The lease runs out and another pass tries again.
		return true, fmt.Errorf("index chunks of version %s: %w", job.VersionID, err)
	}
	if err := c.Finish(ctx, owner, out); err != nil {
		return true, err
	}
	s.deps.Log.Info("version chunks indexed", "version_id", job.VersionID, "files", out.FilesIndexed, "bytes_read", out.BytesRead, "skipped", len(out.Skipped))
	return true, nil
}

// PruneChunks forgets the chunks of files no object holds any more.
func (s *Service) PruneChunks(ctx context.Context) (int64, error) {
	c, err := s.chunks()
	if err != nil {
		return 0, err
	}
	return c.Prune(ctx)
}

func (s *Service) chunkVersion(ctx context.Context, c ChunkStore, job model.ChunkJob) (model.ChunkJob, error) {
	out := job
	out.FilesIndexed, out.BytesRead, out.Skipped = 0, 0, []model.SkippedFile{}
	skip := func(path, reason string) {
		out.Skipped = append(out.Skipped, model.SkippedFile{Path: path, Reason: reason})
	}
	after := int64(0)
	for {
		batch, err := s.deps.Repo.MemberEvidence(ctx, job.TenantID, job.VersionID, after, 500)
		if err != nil {
			return out, err
		}
		for _, m := range batch {
			after = m.Ordinal
			switch {
			case m.Availability != "" && m.Availability != model.MemberAvailable:
				skip(m.Path, "its bytes are "+strings.ToLower(strings.ReplaceAll(m.Availability, "_", " ")))
				continue
			case m.AssetStatus != model.AssetStatusAvailable || m.ObjectKey == "":
				skip(m.Path, "the object is not available")
				continue
			case m.ChecksumSHA256 == "":
				skip(m.Path, "it has no SHA-256 to check against")
				continue
			}
			known, err := c.HasFile(ctx, job.TenantID, m.ChecksumSHA256)
			if err != nil {
				return out, err
			}
			if known {
				out.FilesIndexed++
				continue
			}
			// A file this small is one chunk, whatever it holds: no need to read it.
			if m.SizeBytes <= fastcdc.MinSize {
				raw, err := hex.DecodeString(m.ChecksumSHA256)
				if err != nil {
					skip(m.Path, "its SHA-256 is not valid")
					continue
				}
				if err := c.PutFile(ctx, job.TenantID, m.ChecksumSHA256, m.SizeBytes, fastcdc.Params,
					[]model.RawChunk{{Length: int(m.SizeBytes), SHA256: raw}}, s.now()); err != nil {
					return out, err
				}
				out.FilesIndexed++
				continue
			}
			read, err := s.chunkFile(ctx, c, job.TenantID, m)
			out.BytesRead += read
			if err != nil {
				if ctx.Err() != nil {
					return out, ctx.Err()
				}
				skip(m.Path, err.Error())
				continue
			}
			out.FilesIndexed++
		}
		if len(batch) < 500 {
			return out, nil
		}
	}
}

// chunkFile reads one file and records its chunks, only if the bytes read are
// the ones the snapshot pinned.
func (s *Service) chunkFile(ctx context.Context, c ChunkStore, tenantID string, m model.MemberEvidence) (int64, error) {
	src, err := s.memberSource(ctx, tenantID, m)
	if err != nil {
		return 0, fmt.Errorf("could not be read: %w", err)
	}
	body, err := src.open(ctx)
	if err != nil {
		return 0, fmt.Errorf("could not be read: %w", err)
	}
	defer body.Close()
	whole := sha256.New()
	chunker := fastcdc.New(io.TeeReader(body, whole))
	chunks := []model.RawChunk{}
	var offset int64
	for {
		data, err := chunker.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return offset, fmt.Errorf("could not be read: %w", err)
		}
		sum := sha256.Sum256(data)
		chunks = append(chunks, model.RawChunk{Offset: offset, Length: len(data), SHA256: sum[:]})
		offset += int64(len(data))
	}
	if got := hex.EncodeToString(whole.Sum(nil)); got != m.ChecksumSHA256 {
		return offset, errors.New("the bytes read do not hash to the SHA-256 the snapshot pinned")
	}
	return offset, c.PutFile(ctx, tenantID, m.ChecksumSHA256, offset, fastcdc.Params, chunks, s.now())
}
