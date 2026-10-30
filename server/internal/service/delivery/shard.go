package delivery

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/compaction"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// ShardFile is one file of a shard stream. Body yields exactly the
// file's bytes and ends with an error, not a clean end, if they are not
// the ones published.
type ShardFile struct {
	AssetID string
	Size    int64
	SHA256  string
	Body    io.Reader
}

// maxShardGap is how far apart two entries of one pack may be and still
// be read with one request; the bytes between are read and dropped.
const maxShardGap = 1 << 20

// Files read on their own are fetched shardReadAhead at a time, so their
// times to first byte overlap. One up to aheadInMemory bytes is read whole
// while it waits its turn; a larger one waits with its body open.
const (
	shardReadAhead = 8
	aheadInMemory  = 8 << 20
)

type fetched struct {
	body io.ReadCloser
	err  error
}

// StreamShard serves many files in one response (docs/streaming.md).
// Every file is authorized, audited and counted against the egress
// budget before the first byte; a failure after that ends the stream.
// Consecutive files packed side by side in one pack are read with one
// ranged request; the others on their own, a few ahead at a time, with
// failover to a replica. Each file is checked against its SHA-256 as it
// streams. each must read a file's Body to its end.
func (s *Service) StreamShard(ctx context.Context, principal model.Principal, assetIDs []string, requestID string, each func(ShardFile) error) error {
	type member struct {
		asset model.Asset
		ready readyStorage
	}
	started := time.Now()
	members := make([]member, len(assetIDs))
	var total int64
	for i, id := range assetIDs {
		asset, r, err := s.deliverable(ctx, principal, id, model.DeliveryOpProxy, requestID)
		if err != nil {
			return err
		}
		members[i] = member{asset, r}
		total += asset.SizeBytes
	}
	if s.governance != nil {
		if err := s.governance.Egress(ctx, principal, total); err != nil {
			s.deny(ctx, principal, assetIDs[0], model.DeliveryOpProxy, requestID, "egress_budget")
			return err
		}
	}
	if err := s.budget(ctx, principal, assetIDs[0], model.DeliveryOpProxy, requestID, total); err != nil {
		return err
	}
	for _, m := range members {
		if err := s.record(ctx, s.event(principal, m.asset.ID, model.DeliveryOpProxy, model.DeliveryAllowed, requestID)); err != nil {
			return ErrAuditUnavailable
		}
	}
	s.cfg.Metrics.Add("shard_streams", 1)
	authorized := time.Since(started)

	// Runs: a file and the ones after it that follow it in the same pack.
	var runs [][2]int
	for i := 0; i < len(members); {
		end := i + 1
		if p := members[i].ready.packed; p != nil {
			for end < len(members) {
				q, prev := members[end].ready.packed, members[end-1].ready.packed
				if q == nil || q.PackID != p.PackID || q.Offset < prev.Offset+prev.Length || q.Offset-(prev.Offset+prev.Length) > maxShardGap {
					break
				}
				end++
			}
		}
		runs = append(runs, [2]int{i, end})
		i = end
	}

	ctx, cancel := context.WithCancel(ctx)
	ahead := make(map[int]chan fetched)
	for _, r := range runs {
		if r[1]-r[0] == 1 {
			ahead[r[0]] = make(chan fetched, 1)
		}
	}
	slots := make(chan struct{}, shardReadAhead)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, r := range runs {
			if r[1]-r[0] != 1 {
				continue
			}
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				m := members[i]
				body, err := s.openWhole(ctx, principal, m.asset, m.ready, requestID)
				if err == nil && m.asset.SizeBytes <= aheadInMemory {
					data, rerr := io.ReadAll(body)
					body.Close()
					body, err = io.NopCloser(bytes.NewReader(data)), rerr
				}
				ahead[i] <- fetched{body, err}
			}(r[0])
		}
	}()
	// On the way out, whatever was fetched and not sent is closed.
	defer func() {
		cancel()
		wg.Wait()
		for _, ch := range ahead {
			select {
			case f := <-ch:
				if f.body != nil {
					f.body.Close()
				}
			default:
			}
		}
	}()

	for _, r := range runs {
		i, end := r[0], r[1]
		if end-i == 1 {
			m := members[i]
			var f fetched
			select {
			case f = <-ahead[i]:
			case <-ctx.Done():
				return ctx.Err()
			}
			<-slots
			if f.err != nil {
				return f.err
			}
			err := each(ShardFile{AssetID: m.asset.ID, Size: m.asset.SizeBytes, SHA256: m.asset.ChecksumSHA256,
				Body: compaction.VerifiedEntry(f.body, m.asset.SizeBytes, m.asset.ChecksumSHA256)})
			f.body.Close()
			if err != nil {
				return err
			}
			continue
		}

		first, last := members[i].ready.packed, members[end-1].ready.packed
		body, err := members[i].ready.storage.OpenRange(ctx, first.Bucket, first.ObjectKey, first.Offset, last.Offset+last.Length-1,
			interfaces.ReadCondition{IfMatch: first.StorageETag, VersionID: first.StorageVersionID})
		if err != nil {
			return s.storageError(ctx, principal, members[i].asset, model.DeliveryOpProxy, requestID, err)
		}
		s.cfg.Metrics.Add("packed_read_bytes", last.Offset+last.Length-first.Offset)
		at := first.Offset
		for j := i; j < end; j++ {
			m := members[j]
			if _, err := io.CopyN(io.Discard, body, m.ready.packed.Offset-at); err != nil {
				body.Close()
				return fmt.Errorf("read pack %s: %w", first.PackID, err)
			}
			err := each(ShardFile{AssetID: m.asset.ID, Size: m.asset.SizeBytes, SHA256: m.asset.ChecksumSHA256,
				Body: compaction.VerifiedEntry(io.NopCloser(io.LimitReader(body, m.ready.packed.Length)), m.asset.SizeBytes, m.asset.ChecksumSHA256)})
			if err != nil {
				body.Close()
				return err
			}
			at = m.ready.packed.Offset + m.ready.packed.Length
		}
		body.Close()
	}
	s.log.Info("shard stream served", "tenant_id", principal.TenantID, "files", len(members), "runs", len(runs),
		"authorize_ms", authorized.Milliseconds(), "duration_ms", time.Since(started).Milliseconds(), "request_id", requestID)
	return nil
}

// openWhole opens a file's bytes from wherever they are, failing over to
// a verified replica.
func (s *Service) openWhole(ctx context.Context, principal model.Principal, asset model.Asset, r readyStorage, requestID string) (io.ReadCloser, error) {
	if asset.SizeBytes == 0 {
		return io.NopCloser(eofReader{}), nil
	}
	cond, err := s.readCondition(ctx, principal, asset, r, model.DeliveryOpProxy, DownloadRequest{RequestID: requestID})
	if err != nil {
		return nil, err
	}
	// Ranged reads of the whole file: the caller checks the bytes.
	last := asset.SizeBytes - 1
	var body io.ReadCloser
	if r.packed != nil {
		body, err = s.openPacked(ctx, asset, r, 0, last, true, cond)
	} else {
		body, err = s.openReady(ctx, asset, r, 0, last, true, cond)
	}
	if err != nil && r.replica == nil && ctx.Err() == nil {
		if rr, ok := s.replicaReady(ctx, asset); ok {
			body, err = s.openReady(ctx, asset, rr, 0, last, true, interfaces.ReadCondition{VersionID: rr.replica.VersionID})
		}
	}
	if err != nil {
		return nil, s.storageError(ctx, principal, asset, model.DeliveryOpProxy, requestID, err)
	}
	return body, nil
}

type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }
