package osp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Parts go straight to storage with presigned URLs, so a proxy's limit on
// request bodies never applies to them.
const (
	DefaultPartSize   = 8 << 20
	SingleUploadLimit = 64 << 20
	maxParts          = 10000
	partsAtOnce       = 4
)

// Progress is what is needed to continue an upload.
type Progress struct {
	UploadID string         `json:"upload_id"`
	PartSize int64          `json:"part_size"`
	Parts    map[int]string `json:"parts"`
}

// ProgressStore keeps upload progress across restarts. Save must be durable
// before it returns.
type ProgressStore interface {
	SaveUpload(uploadID string, partSize int64) error
	SavePart(part int, etag string) error
}

// Limiter caps the bytes per second of everything that shares it.
type Limiter struct {
	mu    sync.Mutex
	rate  int64
	next  time.Time
	sleep func(context.Context, time.Duration) error
}

// NewLimiter limits to rate bytes a second; zero or less means no limit.
func NewLimiter(rate int64) *Limiter {
	return &Limiter{rate: rate, sleep: func(ctx context.Context, d time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
			return nil
		}
	}}
}

// Wait returns when n more bytes may go.
func (l *Limiter) Wait(ctx context.Context, n int) error {
	if l == nil || l.rate <= 0 {
		return nil
	}
	l.mu.Lock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	at := l.next
	l.next = l.next.Add(time.Duration(int64(n)) * time.Second / time.Duration(l.rate))
	l.mu.Unlock()
	if d := at.Sub(now); d > 0 {
		return l.sleep(ctx, d)
	}
	return nil
}

type limitedReader struct {
	ctx context.Context
	r   io.Reader
	l   *Limiter
}

func (r *limitedReader) Read(b []byte) (int, error) {
	if len(b) > 256<<10 {
		b = b[:256<<10]
	}
	n, err := r.r.Read(b)
	if n > 0 {
		if werr := r.l.Wait(r.ctx, n); werr != nil {
			return n, werr
		}
	}
	return n, err
}

// UploadOptions says where a file goes and how.
type UploadOptions struct {
	Dataset     string
	Path        string
	ContentType string
	Metadata    map[string]string
	// SHA256 skips hashing when the caller has it already.
	SHA256   string
	PartSize int64
	// Resume continues an earlier upload; Store records progress.
	Resume  *Progress
	Store   ProgressStore
	Limiter *Limiter
	// Attempts per request to storage (default 5).
	Attempts int
}

// UploadResult is an upload whose bytes are all in.
type UploadResult struct {
	UploadID string
	SHA256   string
	Size     int64
}

// SHA256File hashes a file.
func SHA256File(file string) (string, int64, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func partSizeFor(size, wanted int64) int64 {
	if wanted <= 0 {
		wanted = DefaultPartSize
	}
	if least := (size + maxParts - 1) / maxParts; least > wanted {
		return least
	}
	return wanted
}

// UploadFile sends a file, continuing from opts.Resume if given. It returns
// once the upload is complete; the platform then verifies and scans it.
func (c *Client) UploadFile(ctx context.Context, file string, opts UploadOptions) (UploadResult, error) {
	sum, size := opts.SHA256, int64(0)
	if sum == "" {
		var err error
		if sum, size, err = SHA256File(file); err != nil {
			return UploadResult{}, err
		}
	} else {
		info, err := os.Stat(file)
		if err != nil {
			return UploadResult{}, err
		}
		size = info.Size()
	}
	contentType := opts.ContentType
	if contentType == "" {
		contentType = mime.TypeByExtension(strings.ToLower(filepath.Ext(file)))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	objectPath := opts.Path
	if objectPath == "" {
		objectPath = filepath.Base(file)
	}
	keySum := sha256.Sum256([]byte(opts.Dataset + "/" + objectPath))
	body := map[string]any{"file_name": path.Base(objectPath), "content_type": contentType, "size_bytes": size, "checksum_sha256": sum,
		"idempotency_key": "go-" + sum[:40] + "-" + hex.EncodeToString(keySum[:12]), "dataset": opts.Dataset, "path": objectPath, "metadata": opts.Metadata}
	if opts.Attempts <= 0 {
		opts.Attempts = 5
	}
	result := UploadResult{SHA256: sum, Size: size}
	var err error
	if size <= SingleUploadLimit && opts.PartSize == 0 && opts.Resume == nil {
		result.UploadID, err = c.uploadSingle(ctx, file, size, contentType, body, opts)
	} else {
		result.UploadID, err = c.uploadParts(ctx, file, size, sum, body, opts)
	}
	return result, err
}

func (c *Client) uploadSingle(ctx context.Context, file string, size int64, contentType string, body map[string]any, opts UploadOptions) (string, error) {
	var created struct {
		UploadID      string            `json:"upload_id"`
		UploadURL     string            `json:"upload_url"`
		UploadHeaders map[string]string `json:"upload_headers"`
	}
	if err := c.Do(ctx, http.MethodPost, "/api/v1/uploads", body, &created); err != nil {
		return "", err
	}
	headers := map[string]string{"Content-Type": contentType}
	for k, v := range created.UploadHeaders {
		headers[k] = v
	}
	if _, err := c.put(ctx, created.UploadURL, file, 0, size, headers, opts); err != nil {
		return created.UploadID, err
	}
	return created.UploadID, c.Do(ctx, http.MethodPost, "/api/v1/uploads/"+created.UploadID+"/complete", nil, nil)
}

func (c *Client) uploadParts(ctx context.Context, file string, size int64, sum string, body map[string]any, opts UploadOptions) (string, error) {
	progress := opts.Resume
	wanted := partSizeFor(size, opts.PartSize)
	if progress == nil || progress.UploadID == "" || progress.PartSize != wanted {
		var started struct {
			UploadID string `json:"upload_id"`
		}
		if err := c.Do(ctx, http.MethodPost, "/api/v1/uploads/multipart", body, &started); err != nil {
			return "", err
		}
		progress = &Progress{UploadID: started.UploadID, PartSize: wanted, Parts: map[int]string{}}
		if opts.Store != nil {
			if err := opts.Store.SaveUpload(progress.UploadID, wanted); err != nil {
				return "", err
			}
		}
	}
	base := "/api/v1/uploads/" + progress.UploadID
	count := int((size + wanted - 1) / wanted)
	if count == 0 {
		count = 1
	}
	var missing []int
	for n := 1; n <= count; n++ {
		if progress.Parts[n] == "" {
			missing = append(missing, n)
		}
	}
	for start := 0; start < len(missing); start += 16 {
		batch := missing[start:min(start+16, len(missing))]
		var urls struct {
			Parts []struct {
				PartNumber int    `json:"part_number"`
				URL        string `json:"url"`
			} `json:"parts"`
		}
		if err := c.Do(ctx, http.MethodPost, base+"/parts/urls", map[string]any{"part_numbers": batch}, &urls); err != nil {
			return progress.UploadID, fmt.Errorf("part links: %w", err)
		}
		// One connection to a far bucket is slow; several parts at once fill the link.
		send, cancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		var mu sync.Mutex
		var once sync.Once
		var sendErr error
		slots := make(chan struct{}, partsAtOnce)
		for _, part := range urls.Parts {
			select {
			case slots <- struct{}{}:
			case <-send.Done():
			}
			if send.Err() != nil {
				break
			}
			wg.Add(1)
			go func(number int, url string) {
				defer wg.Done()
				defer func() { <-slots }()
				err := c.sendPart(send, base, file, url, number, wanted, size, opts, &mu, progress)
				if err != nil {
					once.Do(func() { sendErr = err; cancel() })
				}
			}(part.PartNumber, part.URL)
		}
		wg.Wait()
		cancel()
		if sendErr != nil {
			return progress.UploadID, sendErr
		}
		if err := ctx.Err(); err != nil {
			return progress.UploadID, err
		}
	}
	numbers := make([]int, 0, len(progress.Parts))
	for n := range progress.Parts {
		numbers = append(numbers, n)
	}
	sort.Ints(numbers)
	completed := make([]map[string]any, len(numbers))
	for i, n := range numbers {
		completed[i] = map[string]any{"part_number": n, "etag": progress.Parts[n]}
	}
	return progress.UploadID, c.Do(ctx, http.MethodPost, base+"/complete", map[string]any{"parts": completed, "checksum_sha256": sum}, nil)
}

// sendPart puts one part, confirms it and records it in progress.
func (c *Client) sendPart(ctx context.Context, base, file, url string, number int, wanted, size int64, opts UploadOptions, mu *sync.Mutex, progress *Progress) error {
	offset := int64(number-1) * wanted
	length := min(wanted, size-offset)
	etag, err := c.put(ctx, url, file, offset, length, nil, opts)
	if err != nil {
		return fmt.Errorf("part %d: %w", number, err)
	}
	if err := c.Do(ctx, http.MethodPost, base+"/parts/confirm", map[string]any{
		"parts": []map[string]any{{"part_number": number, "etag": etag, "size_bytes": length}}}, nil); err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	progress.Parts[number] = etag
	if opts.Store != nil {
		return opts.Store.SavePart(number, etag)
	}
	return nil
}

var errStorage = errors.New("storage refused the upload")

// put sends a range of a file to a presigned URL, retrying what can pass.
func (c *Client) put(ctx context.Context, target, file string, offset, length int64, headers map[string]string, opts UploadOptions) (string, error) {
	var last error
	for attempt := 1; attempt <= opts.Attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		etag, retry, err := c.putOnce(ctx, target, file, offset, length, headers, opts.Limiter)
		if err == nil {
			return etag, nil
		}
		if !retry {
			return "", err
		}
		last = err
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Duration(attempt*attempt) * 200 * time.Millisecond):
		}
	}
	return "", last
}

func (c *Client) putOnce(ctx context.Context, target, file string, offset, length int64, headers map[string]string, limiter *Limiter) (string, bool, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	var body io.Reader = io.NewSectionReader(f, offset, length)
	if limiter != nil {
		body = &limitedReader{ctx: ctx, r: body, l: limiter}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, body)
	if err != nil {
		return "", false, err
	}
	req.ContentLength = length
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", true, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 == 2 {
		return resp.Header.Get("ETag"), false, nil
	}
	retry := resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
	return "", retry, fmt.Errorf("%w: HTTP %d", errStorage, resp.StatusCode)
}
