package processing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sync"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/rangeread"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// scanOverlap is how far each segment of a large object reaches back into
// the one before, so a signature that straddles a boundary is still read
// whole by one scan. ClamAV's byte signatures are far shorter than this.
const scanOverlap = 1 << 20

// archiveTypes are unpacked by the scanner. A slice of an archive cannot
// be unpacked, so one too big to scan whole cannot be scanned at all.
var archiveTypes = map[string]bool{
	"application/zip": true, "application/x-gzip": true, "application/gzip": true,
	"application/x-rar-compressed": true, "application/x-7z-compressed": true,
}

// scanWhole streams the object to the scanner in one piece, hashing and
// counting exactly what the scanner reads.
func (s *Service) scanWhole(ctx context.Context, storage interfaces.MultipartStorage, bucket, key string) (interfaces.ScanResult, int64, string, error) {
	body, err := storage.OpenObject(ctx, bucket, key)
	if err != nil {
		return interfaces.ScanResult{}, 0, "", fmt.Errorf("open object to scan: %w", err)
	}
	defer body.Close()

	hasher := sha256.New()
	var read byteCount
	scanCtx, cancel := context.WithTimeout(ctx, s.cfg.ScanTimeout)
	defer cancel()
	result := s.deps.Scanner.Scan(scanCtx, io.TeeReader(body, io.MultiWriter(hasher, &read)))
	return result, int64(read), hex.EncodeToString(hasher.Sum(nil)), nil
}

// scanSegments scans an object too big for one scanner stream as
// overlapping segments, ScanParallelism of them at once, while one more
// read hashes the whole object. Every read is pinned to the object's ETag,
// so what was scanned and what was hashed are the same bytes. The first
// INFECTED segment, or the first one that could not be scanned, stops the
// rest; INFECTED always wins.
func (s *Service) scanSegments(ctx context.Context, storage interfaces.MultipartStorage, bucket, key, detected string) (interfaces.ScanResult, int64, string, error) {
	if archiveTypes[detected] {
		return interfaces.ScanResult{Outcome: model.ScanUnsupported, Detail: "an archive larger than one scan segment cannot be scanned"}, 0, "", nil
	}
	ranger, ok := storage.(rangeread.Ranger)
	if !ok {
		return interfaces.ScanResult{Outcome: model.ScanUnsupported, Detail: "the storage cannot serve ranges, so a large object cannot be scanned"}, 0, "", nil
	}
	meta, err := storage.HeadObject(ctx, bucket, key)
	if err != nil {
		return interfaces.ScanResult{}, 0, "", fmt.Errorf("inspect object to scan: %w", err)
	}
	cond := interfaces.ReadCondition{IfMatch: meta.ETag, VersionID: meta.VersionID}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type hashed struct {
		read int64
		sum  string
		err  error
	}
	hashDone := make(chan hashed, 1)
	go func() {
		body := rangeread.Open(ctx, ranger, bucket, key, meta.SizeBytes, cond, rangeread.ChunkBytes, rangeread.Parallel)
		defer body.Close()
		h := sha256.New()
		n, err := io.Copy(h, body)
		hashDone <- hashed{read: n, sum: hex.EncodeToString(h.Sum(nil)), err: err}
	}()

	var mu sync.Mutex
	verdict := interfaces.ScanResult{Outcome: model.ScanClean}
	record := func(result interfaces.ScanResult) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case result.Outcome == model.ScanInfected && verdict.Outcome != model.ScanInfected:
			verdict = result
			cancel()
		case result.Outcome != model.ScanClean && verdict.Outcome == model.ScanClean:
			// Later failures are only the cancellation this one causes.
			verdict = result
			cancel()
		}
	}

	slots := make(chan struct{}, s.cfg.ScanParallelism)
	var wg sync.WaitGroup
	for start := int64(0); start < meta.SizeBytes; start += s.cfg.ScanSegmentBytes {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		first := max(start-scanOverlap, 0)
		last := min(start+s.cfg.ScanSegmentBytes, meta.SizeBytes) - 1
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			record(s.scanRange(ctx, ranger, bucket, key, first, last, cond))
		}()
	}
	wg.Wait()

	if verdict.Outcome != model.ScanClean {
		return verdict, 0, "", nil
	}
	if ctx.Err() != nil {
		// Cancelled from outside (lease lost, shutdown): nothing was decided.
		return interfaces.ScanResult{Outcome: model.ScanError, Detail: "scan cancelled"}, 0, "", nil
	}
	h := <-hashDone
	if h.err != nil {
		return interfaces.ScanResult{Outcome: model.ScanError, Detail: "could not read the whole object to hash it"}, 0, "", nil
	}
	return verdict, h.read, h.sum, nil
}

// scanRange scans bytes first..last on their own scanner connection.
func (s *Service) scanRange(ctx context.Context, ranger rangeread.Ranger, bucket, key string, first, last int64, cond interfaces.ReadCondition) interfaces.ScanResult {
	scanCtx, cancel := context.WithTimeout(ctx, s.cfg.ScanTimeout)
	defer cancel()
	body, err := ranger.OpenRange(scanCtx, bucket, key, first, last, cond)
	if err != nil {
		return interfaces.ScanResult{Outcome: model.ScanError, Detail: "could not read a segment to scan"}
	}
	defer body.Close()

	var read byteCount
	result := s.deps.Scanner.Scan(scanCtx, io.TeeReader(body, &read))
	if result.Outcome == model.ScanClean && int64(read) != last-first+1 {
		return interfaces.ScanResult{Outcome: model.ScanError, Detail: "the scanner did not read a whole segment"}
	}
	return result
}
