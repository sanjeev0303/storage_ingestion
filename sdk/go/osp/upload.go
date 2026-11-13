package osp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Parts go straight to storage with presigned URLs, so a proxy's limit on
// request bodies never applies to them.
const (
	DefaultPartSize   = 8 << 20
	SingleUploadLimit = 64 << 20
	maxParts          = 10000
	partsAtOnce       = 4
	// DefaultStallTimeout is how long a request to storage may send nothing
	// before it is dropped and tried again.
	DefaultStallTimeout = 30 * time.Second
	maxBackoff          = 30 * time.Second
)

// Progress is what is needed to continue an upload. SHA256 and Size name the
// file it is for: it is only continued for that same file.
type Progress struct {
	UploadID string         `json:"upload_id"`
	PartSize int64          `json:"part_size"`
	Parts    map[int]string `json:"parts"`
	SHA256   string         `json:"sha256"`
	Size     int64          `json:"size"`
}

// ProgressStore keeps upload progress across restarts. Save must be durable
// before it returns. SaveUpload is given a new upload with no parts yet.
type ProgressStore interface {
	SaveUpload(p Progress) error
	SavePart(part int, etag string) error
}

// ProgressFile keeps one upload's progress in a JSON file, so running the
// same upload again continues from its confirmed parts: pass Resume() as
// UploadOptions.Resume and the file itself as Store. Remove it once the
// upload is complete. Progress for a file that has changed is not used, and
// a saved upload whose session has ended is replaced by a new one.
type ProgressFile struct {
	path string
	mu   sync.Mutex
	p    Progress
}

// OpenProgressFile reads the file at path, if there is one.
func OpenProgressFile(path string) (*ProgressFile, error) {
	f := &ProgressFile{path: path, p: Progress{Parts: map[int]string{}}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &f.p); err != nil {
		return nil, fmt.Errorf("progress file %s: %w", path, err)
	}
	if f.p.Parts == nil {
		f.p.Parts = map[int]string{}
	}
	return f, nil
}

// Resume is the saved progress, or nil when there is none yet.
func (f *ProgressFile) Resume() *Progress {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.p.UploadID == "" {
		return nil
	}
	parts := make(map[int]string, len(f.p.Parts))
	for n, etag := range f.p.Parts {
		parts[n] = etag
	}
	return &Progress{UploadID: f.p.UploadID, PartSize: f.p.PartSize, Parts: parts, SHA256: f.p.SHA256, Size: f.p.Size}
}

func (f *ProgressFile) SaveUpload(p Progress) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.p = Progress{UploadID: p.UploadID, PartSize: p.PartSize, Parts: map[int]string{}, SHA256: p.SHA256, Size: p.Size}
	return f.write()
}

func (f *ProgressFile) SavePart(part int, etag string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.p.Parts[part] = etag
	return f.write()
}

// Remove deletes the file.
func (f *ProgressFile) Remove() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.p = Progress{Parts: map[int]string{}}
	if err := os.Remove(f.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// write replaces the file whole and syncs it, so a crash leaves either the
// old progress or the new, never half of one.
func (f *ProgressFile) write() error {
	raw, err := json.Marshal(f.p)
	if err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := out.Write(raw); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, f.path)
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
	// waiting, when set, is told when a wait for the limit starts and ends,
	// so a stall watchdog does not count the time the limit holds bytes back.
	waiting func(bool)
}

func (r *limitedReader) Read(b []byte) (int, error) {
	if len(b) > 256<<10 {
		b = b[:256<<10]
	}
	n, err := r.r.Read(b)
	if n > 0 {
		if r.waiting != nil {
			r.waiting(true)
			defer r.waiting(false)
		}
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
	// Resume continues an earlier upload of the same file (same SHA256 and
	// size); Store records progress.
	Resume  *Progress
	Store   ProgressStore
	Limiter *Limiter
	// Attempts per request to storage and to the API's upload routes
	// (default 5). Network errors, stalls, 408, 429 and 5xx are tried again
	// with backoff; an expired part URL is replaced; other refusals are not.
	Attempts int
	// StallTimeout drops a request to storage that sends nothing for this
	// long (default DefaultStallTimeout). A slow link that keeps moving is
	// never cut off.
	StallTimeout time.Duration
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
	if opts.StallTimeout <= 0 {
		opts.StallTimeout = DefaultStallTimeout
	}
	result := UploadResult{SHA256: sum, Size: size}
	single := size <= SingleUploadLimit && opts.PartSize == 0 && opts.Resume == nil
	send := func() (string, error) {
		if single {
			return c.uploadSingle(ctx, file, size, contentType, body, opts)
		}
		return c.uploadParts(ctx, file, size, sum, body, opts)
	}
	var err error
	result.UploadID, err = send()
	if sessionGone(err) {
		// The session ended, and its idempotency key would hand the same
		// session back: once more from the start, under a new key.
		body["idempotency_key"] = fmt.Sprintf("%s-%08x", body["idempotency_key"], rand.Uint32())
		opts.Resume = nil
		result.UploadID, err = send()
	}
	return result, err
}

// sessionGone reports whether the API said an upload session has ended or
// never existed: 404, or 409 "not active" (a multipart upload that is not
// active, or a replayed start whose session ended). Other 409s (a start
// still pending, an idempotency conflict) are not this.
func sessionGone(err error) bool {
	var refused *APIError
	if !errors.As(err, &refused) {
		return false
	}
	switch refused.Status {
	case http.StatusNotFound, http.StatusGone:
		return true
	case http.StatusConflict:
		return strings.Contains(refused.Message, "not active")
	}
	return false
}

func (c *Client) uploadSingle(ctx context.Context, file string, size int64, contentType string, body map[string]any, opts UploadOptions) (string, error) {
	var created struct {
		UploadID      string            `json:"upload_id"`
		Status        string            `json:"status"`
		UploadURL     string            `json:"upload_url"`
		UploadHeaders map[string]string `json:"upload_headers"`
	}
	if err := c.doRetry(ctx, opts, http.MethodPost, "/api/v1/uploads", body, &created); err != nil {
		return "", err
	}
	if created.UploadURL == "" && (created.Status == "COMPLETED" || created.Status == "VERIFIED") {
		// A start sent again after its upload completed: nothing is left to send.
		return created.UploadID, nil
	}
	headers := map[string]string{"Content-Type": contentType}
	for k, v := range created.UploadHeaders {
		headers[k] = v
	}
	// The same idempotency key hands back the same upload with a new URL.
	refresh := func() (string, error) {
		err := c.doRetry(ctx, opts, http.MethodPost, "/api/v1/uploads", body, &created)
		return created.UploadURL, err
	}
	if _, err := c.put(ctx, created.UploadURL, refresh, file, 0, size, headers, opts); err != nil {
		return created.UploadID, err
	}
	return created.UploadID, c.doRetry(ctx, opts, http.MethodPost, "/api/v1/uploads/"+created.UploadID+"/complete", nil, nil)
}

func (c *Client) uploadParts(ctx context.Context, file string, size int64, sum string, body map[string]any, opts UploadOptions) (string, error) {
	progress := opts.Resume
	wanted := partSizeFor(size, opts.PartSize)
	sameFile := progress != nil && progress.SHA256 == sum && progress.Size == size
	if !sameFile || progress.UploadID == "" || progress.PartSize != wanted {
		var started struct {
			UploadID string `json:"upload_id"`
		}
		if err := c.doRetry(ctx, opts, http.MethodPost, "/api/v1/uploads/multipart", body, &started); err != nil {
			return "", err
		}
		progress = &Progress{UploadID: started.UploadID, PartSize: wanted, Parts: map[int]string{}, SHA256: sum, Size: size}
		if opts.Store != nil {
			if err := opts.Store.SaveUpload(*progress); err != nil {
				return "", err
			}
		}
	}
	if progress.Parts == nil {
		progress.Parts = map[int]string{} // saved before any part was
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
		urls, err := c.partURLs(ctx, opts, base, batch)
		if err != nil {
			return progress.UploadID, fmt.Errorf("part links: %w", err)
		}
		// One connection to a far bucket is slow; several parts at once fill the link.
		send, cancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		var mu sync.Mutex
		var once sync.Once
		var sendErr error
		slots := make(chan struct{}, partsAtOnce)
		for _, part := range urls {
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
	return progress.UploadID, c.doRetry(ctx, opts, http.MethodPost, base+"/complete", map[string]any{"parts": completed, "checksum_sha256": sum}, nil)
}

type partURL struct {
	PartNumber int    `json:"part_number"`
	URL        string `json:"url"`
}

func (c *Client) partURLs(ctx context.Context, opts UploadOptions, base string, numbers []int) ([]partURL, error) {
	var out struct {
		Parts []partURL `json:"parts"`
	}
	err := c.doRetry(ctx, opts, http.MethodPost, base+"/parts/urls", map[string]any{"part_numbers": numbers}, &out)
	return out.Parts, err
}

// sendPart puts one part, confirms it and records it in progress.
func (c *Client) sendPart(ctx context.Context, base, file, url string, number int, wanted, size int64, opts UploadOptions, mu *sync.Mutex, progress *Progress) error {
	offset := int64(number-1) * wanted
	length := min(wanted, size-offset)
	// Part links last about 15 minutes; one that expired before its turn is replaced.
	refresh := func() (string, error) {
		urls, err := c.partURLs(ctx, opts, base, []int{number})
		if err != nil || len(urls) == 0 {
			return "", fmt.Errorf("new link for part %d: %w", number, err)
		}
		return urls[0].URL, nil
	}
	etag, err := c.put(ctx, url, refresh, file, offset, length, nil, opts)
	if err != nil {
		return fmt.Errorf("part %d: %w", number, err)
	}
	if err := c.doRetry(ctx, opts, http.MethodPost, base+"/parts/confirm", map[string]any{
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

var (
	errStorage = errors.New("storage refused the upload")
	errStalled = errors.New("no bytes reached storage within the stall timeout")
	// What S3, GCS and Azure say when a presigned URL has expired (or the
	// clock is off). S3 sends an expired URL as AccessDenied, so the code
	// alone means a real refusal.
	expiredURL = regexp.MustCompile(`(?i)Request has expired|ExpiredToken|token has expired|signature (has )?expired|not valid in the specified time frame|RequestTimeTooSkewed`)
)

// backoff is exponential with full jitter, capped at 30 seconds: anywhere
// from nothing up to the doubled delay, so clients that failed together do
// not all come back at once.
func backoff(attempt int) time.Duration {
	limit := min(maxBackoff, time.Second<<min(attempt, 10))
	return time.Duration(rand.Int64N(int64(limit) + 1))
}

func retryableStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

// retryAfter reads Retry-After in seconds, at most a minute.
func retryAfter(h http.Header) time.Duration {
	seconds, err := strconv.Atoi(h.Get("Retry-After"))
	if err != nil || seconds <= 0 {
		return 0
	}
	return min(time.Duration(seconds)*time.Second, time.Minute)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// doRetry is Do for the upload routes, which are safe to send again (starts
// carry an idempotency key; links, confirmations and completion repeat
// safely): network errors, 408, 429 and 5xx are tried again with backoff.
func (c *Client) doRetry(ctx context.Context, opts UploadOptions, method, path string, body, out any) error {
	for attempt := 1; ; attempt++ {
		err := c.Do(ctx, method, path, body, out)
		if err == nil || ctx.Err() != nil || attempt >= opts.Attempts {
			return err
		}
		var refused *APIError
		if errors.As(err, &refused) && !retryableStatus(refused.Status) {
			return err
		}
		if err := sleepCtx(ctx, backoff(attempt-1)); err != nil {
			return err
		}
	}
}

// put sends a range of a file to a presigned URL, retrying what can pass.
// refresh answers a new URL when the current one has expired.
func (c *Client) put(ctx context.Context, target string, refresh func() (string, error), file string, offset, length int64, headers map[string]string, opts UploadOptions) (string, error) {
	var last error
	for attempt := 1; attempt <= opts.Attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		res := c.putOnce(ctx, target, file, offset, length, headers, opts)
		if res.err == nil {
			return res.etag, nil
		}
		if res.expired && refresh != nil {
			fresh, err := refresh()
			if err != nil {
				return "", err
			}
			target, last = fresh, res.err
			continue
		}
		if !res.retry {
			return "", res.err
		}
		last = res.err
		wait := res.wait
		if wait == 0 {
			wait = backoff(attempt - 1)
		}
		if attempt < opts.Attempts {
			if err := sleepCtx(ctx, wait); err != nil {
				return "", err
			}
		}
	}
	return "", last
}

type putResult struct {
	etag    string
	err     error
	retry   bool
	expired bool
	wait    time.Duration
}

// progressReader calls onRead whenever bytes are taken from it, which the
// transport does as fast as the connection accepts them.
type progressReader struct {
	r      io.Reader
	onRead func()
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.onRead()
	}
	return n, err
}

func (c *Client) putOnce(ctx context.Context, target, file string, offset, length int64, headers map[string]string, opts UploadOptions) putResult {
	f, err := os.Open(file)
	if err != nil {
		return putResult{err: err}
	}
	defer f.Close()

	// A watchdog on progress rather than a deadline on the whole request:
	// each read restarts it, and a silent connection is cancelled.
	send, cancel := context.WithCancel(ctx)
	defer cancel()
	var stalled atomic.Bool
	watchdog := time.AfterFunc(opts.StallTimeout, func() {
		stalled.Store(true)
		cancel()
	})
	defer watchdog.Stop()

	var body io.Reader = io.NewSectionReader(f, offset, length)
	if opts.Limiter != nil {
		body = &limitedReader{ctx: send, r: body, l: opts.Limiter, waiting: func(waiting bool) {
			if waiting {
				watchdog.Stop()
			} else {
				watchdog.Reset(opts.StallTimeout)
			}
		}}
	}
	body = &progressReader{r: body, onRead: func() { watchdog.Reset(opts.StallTimeout) }}
	req, err := http.NewRequestWithContext(send, http.MethodPut, target, body)
	if err != nil {
		return putResult{err: err}
	}
	req.ContentLength = length
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return putResult{err: ctx.Err()}
		}
		if stalled.Load() {
			return putResult{err: errStalled, retry: true}
		}
		return putResult{err: err, retry: true}
	}
	defer resp.Body.Close()
	text, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 == 2 {
		return putResult{etag: resp.Header.Get("ETag")}
	}
	err = fmt.Errorf("%w: HTTP %d", errStorage, resp.StatusCode)
	if resp.StatusCode == http.StatusForbidden && expiredURL.Match(text) {
		return putResult{err: err, expired: true}
	}
	return putResult{err: err, retry: retryableStatus(resp.StatusCode), wait: retryAfter(resp.Header)}
}
