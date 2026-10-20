// Command ingestion-loadgen drives the bulk-ingestion API with a synthetic
// dataset and reports what it measured.
//
// Files are generated from a seed as they are needed, so a run of any size
// needs no prepared data and the same seed gives the same dataset. In
// upload mode their bytes go to the object store through the URLs the API
// grants, and the run waits for processing to publish them. In synthetic
// mode nothing is uploaded: the manifest is read, entries are scheduled
// until a limit holds them back, and the job is cancelled. Its numbers
// measure the control plane, never storage throughput.
//
// It signs in to the local API as -email, with the password from
// LOADGEN_PASSWORD (never a flag, so it stays out of process listings),
// and works in the organization -org. The account needs a role with
// datasets.write, ingestion.write and uploads.write: developer or above.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/request"
	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
)

type options struct {
	api         string
	email       string
	tenant      string
	mode        string
	manifest    string
	classes     string
	files       int
	seed        uint64
	concurrency int
	claim       int
	partSize    int64
	wait        time.Duration
	vars        string
	db          string
	containers  string
	out         string
	manifestOut string
}

func main() {
	var o options
	var partSize string
	flag.StringVar(&o.api, "api", "http://127.0.0.1:8080", "API base URL")
	flag.StringVar(&o.email, "email", "", "account to sign in as (password from LOADGEN_PASSWORD)")
	flag.StringVar(&o.tenant, "org", "", "organization ID to work in (optional when the account has only one)")
	flag.StringVar(&o.mode, "mode", "upload", "upload: upload every file and wait for processing; synthetic: schedule without uploading")
	flag.StringVar(&o.manifest, "manifest", "auto", "inline, staged, or auto (staged above 16 MiB)")
	flag.StringVar(&o.classes, "classes", "small=85:1KiB-256KiB,medium=13:1MiB-8MiB,large=2:20MiB-48MiB", "size classes: name=percent:min-max,...")
	flag.IntVar(&o.files, "files", 500, "number of files")
	flag.Uint64Var(&o.seed, "seed", 1, "seed of the synthetic dataset")
	flag.IntVar(&o.concurrency, "concurrency", 16, "files uploaded at once")
	flag.IntVar(&o.claim, "claim", 32, "entries asked for per claim")
	flag.StringVar(&partSize, "part-size", "8MiB", "multipart part size")
	flag.DurationVar(&o.wait, "wait", 30*time.Minute, "how long to wait for the job to finish")
	flag.StringVar(&o.vars, "vars", "", "comma-separated /debug/vars URLs of the API and workers, for their memory")
	flag.StringVar(&o.db, "db", "", "PostgreSQL URL to sample (local only; never read from the environment)")
	flag.StringVar(&o.containers, "containers", "", "sample `docker stats` for containers with this name prefix")
	flag.StringVar(&o.out, "out", "", "also write the report as JSON to this file")
	flag.StringVar(&o.manifestOut, "manifest-out", "", "only write the manifest to this file and exit (real checksums in upload mode)")
	flag.Parse()

	var err error
	if o.partSize, err = parseSize(partSize); err != nil || o.partSize < 5<<20 {
		fmt.Fprintln(os.Stderr, "loadgen: -part-size must be at least 5MiB")
		os.Exit(2)
	}
	if o.email == "" || os.Getenv("LOADGEN_PASSWORD") == "" {
		fmt.Fprintln(os.Stderr, "loadgen: -email and LOADGEN_PASSWORD are required")
		os.Exit(2)
	}
	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}
}

// report is everything one run measured.
type report struct {
	Mode        string    `json:"mode"`
	Tenant      string    `json:"tenant"`
	JobID       string    `json:"job_id"`
	Files       int       `json:"files"`
	Classes     string    `json:"classes"`
	Seed        uint64    `json:"seed"`
	Concurrency int       `json:"concurrency"`
	Started     time.Time `json:"started"`

	ByClass       map[string]int `json:"files_by_class"`
	ExpectedBytes int64          `json:"expected_bytes"`
	HashSeconds   float64        `json:"client_hash_seconds,omitempty"`

	ManifestSource  string  `json:"manifest_source"`
	ManifestBytes   int64   `json:"manifest_bytes"`
	ManifestSeconds float64 `json:"manifest_seconds"`
	ManifestLines   int64   `json:"manifest_lines"`

	Upload     *uploadReport     `json:"upload,omitempty"`
	Scheduling *schedulingReport `json:"scheduling,omitempty"`

	FinalState string                     `json:"final_state"`
	Progress   response.IngestionProgress `json:"final_progress"`
	EndToEnd   float64                    `json:"end_to_end_seconds"`
	// ProcessingRate is files published per second between the first and
	// the last publication.
	ProcessingRate float64 `json:"published_files_per_second"`

	Requests  map[string]int `json:"requests"`
	Failures  map[string]int `json:"request_failures"`
	ErrorRate float64        `json:"request_error_rate"`
	Latency   map[string]any `json:"latency_ms"`
	Errors    []string       `json:"error_samples,omitempty"`
	Backlog   progressSample `json:"job_backlog"`
	Self      map[string]any `json:"loadgen_memory"`
	Servers   map[string]any `json:"server_memory,omitempty"`
	Database  map[string]any `json:"database,omitempty"`
	Container map[string]any `json:"containers,omitempty"`
	Notes     []string       `json:"notes"`
}

type uploadReport struct {
	Uploaded      int64          `json:"files_uploaded"`
	Bytes         int64          `json:"bytes_uploaded"`
	Seconds       float64        `json:"seconds"`
	FilesPerSec   float64        `json:"files_per_second"`
	MiBPerSec     float64        `json:"mib_per_second"`
	ByStrategy    map[string]int `json:"by_strategy"`
	FailedEntries int            `json:"failed_entries"`
	Pending202    int            `json:"completions_pending"`
	LimitedBy     map[string]int `json:"claims_limited_by"`
}

type schedulingReport struct {
	Granted       int            `json:"entries_granted"`
	Claims        int            `json:"claims"`
	HeldBy        string         `json:"held_back_by"`
	Seconds       float64        `json:"seconds"`
	CancelSeconds float64        `json:"cancel_seconds"`
	LimitedBy     map[string]int `json:"claims_limited_by"`
}

func run(o options) error {
	classes, err := parseClasses(o.classes)
	if err != nil {
		return err
	}
	r := &report{
		Mode: o.mode, Tenant: o.tenant, Files: o.files, Classes: o.classes, Seed: o.seed,
		Concurrency: o.concurrency, Started: time.Now().UTC(), ByClass: map[string]int{},
	}
	specs := plan(o.files, classes, o.seed)
	for _, f := range specs {
		r.ByClass[f.class]++
		r.ExpectedBytes += f.size
	}

	switch o.mode {
	case "upload":
		started := time.Now()
		hashAll(specs, o.seed, max(o.concurrency, 4))
		r.HashSeconds = time.Since(started).Seconds()
	case "synthetic":
		fakeSums(specs, o.seed)
	default:
		return fmt.Errorf("-mode must be upload or synthetic")
	}
	if o.manifestOut != "" {
		f, err := os.Create(o.manifestOut)
		if err != nil {
			return err
		}
		if err := writeManifest(f, specs); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}

	ctx := context.Background()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = o.concurrency * 2
	a := &api{base: strings.TrimSuffix(o.api, "/"), org: o.tenant, client: &http.Client{Transport: transport}, stats: newRequestStats()}
	if err := a.signIn(ctx, o.email, os.Getenv("LOADGEN_PASSWORD")); err != nil {
		return fmt.Errorf("sign in: %w", err)
	}

	sampleCtx, stopSampling := context.WithCancel(ctx)
	s := newSamplers()
	s.self(sampleCtx)
	if o.vars != "" {
		s.expvar(sampleCtx, strings.Split(o.vars, ","))
	}
	if o.db != "" {
		if err := s.database(sampleCtx, o.db); err != nil {
			stopSampling()
			return fmt.Errorf("database sampling: %w", err)
		}
	}
	if o.containers != "" {
		s.containers(sampleCtx, o.containers)
	}

	runErr := drive(ctx, o, a, s, specs, r)
	stopSampling()
	s.wait()
	r.collect(a, s)
	r.print(os.Stdout)
	if o.out != "" {
		raw, err := json.MarshalIndent(r, "", "  ")
		if err == nil {
			err = os.WriteFile(o.out, raw, 0o644)
		}
		if err != nil {
			return fmt.Errorf("write report: %w", err)
		}
	}
	return runErr
}

// drive runs the job: dataset, manifest, finalize, then uploads or the
// scheduling probe.
func drive(ctx context.Context, o options, a *api, s *samplers, specs []fileSpec, r *report) error {
	started := time.Now()
	dataset, err := a.createDataset(ctx, fmt.Sprintf("loadgen-%d-%d", o.seed, started.UnixNano()))
	if err != nil {
		return err
	}
	job, err := a.createJob(ctx, dataset.ID, fmt.Sprintf("loadgen-%d", started.UnixNano()))
	if err != nil {
		return err
	}
	r.JobID = job.ID

	size, sum := manifestSize(specs)
	r.ManifestBytes = size
	r.ManifestSource = o.manifest
	if o.manifest == "auto" {
		r.ManifestSource = "inline"
		if size > 16<<20 {
			r.ManifestSource = "staged"
		}
	}
	manifestStarted := time.Now()
	if err := submitManifest(ctx, a, job.ID, r.ManifestSource, specs, size, sum); err != nil {
		return err
	}
	r.ManifestSeconds = time.Since(manifestStarted).Seconds()
	job, err = a.finalize(ctx, job.ID)
	if err != nil {
		return err
	}
	r.ManifestLines = job.ParsedLines

	jobCtx, stopWatching := context.WithCancel(ctx)
	defer stopWatching()
	s.watchJob(jobCtx, a, job.ID)

	if o.mode == "synthetic" {
		r.Scheduling, err = probeScheduling(ctx, a, job.ID, o.claim)
	} else {
		r.Upload, err = uploadAll(ctx, o, a, specs, job.ID)
		if err == nil {
			err = waitForJob(ctx, a, job.ID, o.wait)
		}
	}
	final, jobErr := a.job(ctx, job.ID)
	if jobErr == nil {
		s.noteProgress(final)
		r.FinalState, r.Progress = final.State, final.Progress
	}
	r.EndToEnd = time.Since(started).Seconds()
	return err
}

func submitManifest(ctx context.Context, a *api, jobID, source string, specs []fileSpec, size int64, sum string) error {
	if source == "inline" {
		body := manifestBody(specs)
		defer body.Close()
		report, err := a.putManifest(ctx, jobID, body, size, sum)
		if err != nil {
			return err
		}
		if !report.Complete {
			return fmt.Errorf("the manifest was not read in full: %+v", report)
		}
		return nil
	}
	staged, err := a.stageManifest(ctx, jobID, size)
	if err != nil {
		return err
	}
	body := manifestBody(specs)
	defer body.Close()
	if _, err := a.put(ctx, staged.UploadURL, "application/x-ndjson", body, size); err != nil {
		return fmt.Errorf("upload staged manifest: %w", err)
	}
	if err := a.submitStaged(ctx, jobID, sum); err != nil {
		return err
	}
	// The upload worker reads it; wait until it has.
	for {
		job, err := a.job(ctx, jobID)
		if err != nil {
			return err
		}
		if job.ManifestComplete {
			return nil
		}
		if job.State == "FAILED" || job.State == "CANCELLED" || job.State == "CANCELLING" {
			return fmt.Errorf("the staged manifest was refused: %s", job.Error)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// uploadAll claims entries and uploads them with o.concurrency workers. A
// claim asks only for what the workers can take, so grants never wait in
// memory for long. It ends once the job has nothing left to upload.
func uploadAll(ctx context.Context, o options, a *api, specs []fileSpec, jobID string) (*uploadReport, error) {
	u := &uploadReport{ByStrategy: map[string]int{}, LimitedBy: map[string]int{}}
	var mu sync.Mutex
	work := make(chan response.GrantResponse, o.concurrency)
	finished := make(chan struct{}, o.concurrency)
	var outstanding atomic.Int64
	var uploaded, uploadedBytes atomic.Int64

	var wg sync.WaitGroup
	for w := 0; w < o.concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for g := range work {
				status, err := uploadOne(ctx, o, a, specs[g.Entry.Position-1], jobID, g)
				mu.Lock()
				switch {
				case err != nil:
					u.FailedEntries++
				default:
					u.ByStrategy[g.Entry.Strategy]++
					if status == http.StatusAccepted {
						u.Pending202++
					}
				}
				mu.Unlock()
				if err == nil {
					uploaded.Add(1)
					uploadedBytes.Add(g.Entry.SizeBytes)
				}
				outstanding.Add(-1)
				select {
				case finished <- struct{}{}:
				default:
				}
			}
		}()
	}

	started := time.Now()
	var runErr error
	failedClaims := 0
	for {
		free := o.concurrency - int(outstanding.Load())
		if free <= 0 {
			<-finished
			continue
		}
		claim, err := a.claim(ctx, jobID, min(free, o.claim))
		if err != nil {
			failedClaims++
			if failedClaims == 10 {
				runErr = fmt.Errorf("10 claims in a row failed; last: %w", err)
				break
			}
			time.Sleep(time.Second)
			continue
		}
		failedClaims = 0
		outstanding.Add(int64(len(claim.Grants)))
		for _, g := range claim.Grants {
			work <- g
		}
		if len(claim.Grants) > 0 {
			continue
		}
		u.LimitedBy[claim.LimitedBy]++
		if claim.LimitedBy == "job_not_running" {
			break
		}
		if claim.LimitedBy == "no_pending_entries" && outstanding.Load() == 0 {
			job, err := a.job(ctx, jobID)
			if err == nil && job.Progress.Pending == 0 && job.Progress.FailedRetryable == 0 && job.Progress.Scheduled+job.Progress.Uploading == 0 {
				break
			}
		}
		// Held back: wait for a worker to finish or for the suggested time.
		pause := time.Duration(max(claim.RetryAfterSeconds, 1)) * time.Second
		select {
		case <-finished:
		case <-time.After(pause):
		}
	}
	close(work)
	wg.Wait()

	elapsed := time.Since(started).Seconds()
	u.Uploaded, u.Bytes, u.Seconds = uploaded.Load(), uploadedBytes.Load(), elapsed
	if elapsed > 0 {
		u.FilesPerSec = float64(u.Uploaded) / elapsed
		u.MiBPerSec = float64(u.Bytes) / (1 << 20) / elapsed
	}
	return u, runErr
}

// uploadOne uploads one granted file and completes its entry, retrying a
// failed storage PUT twice.
func uploadOne(ctx context.Context, o options, a *api, f fileSpec, jobID string, g response.GrantResponse) (int, error) {
	pos := g.Entry.Position
	if g.Entry.Strategy == "SINGLE" {
		err := withRetries(func() error {
			_, err := a.put(ctx, g.UploadURL, g.Entry.ContentType, newContent(o.seed, f), f.size)
			return err
		})
		if err != nil {
			return 0, err
		}
		return completeWithRetries(ctx, a, jobID, pos, nil)
	}

	count := int32((f.size + o.partSize - 1) / o.partSize)
	numbers := make([]int32, 0, count)
	for n := int32(1); n <= count; n++ {
		numbers = append(numbers, n)
	}
	var urls []response.PresignedPartURL
	for start := 0; start < len(numbers); start += 100 {
		batch, err := a.partURLs(ctx, jobID, pos, numbers[start:min(start+100, len(numbers))])
		if err != nil {
			return 0, err
		}
		urls = append(urls, batch...)
	}

	stream := newContent(o.seed, f)
	var recorded []request.RecordPartRequest
	var completed []request.CompletedPartRequest
	for i, u := range urls {
		length := min(o.partSize, f.size-int64(i)*o.partSize)
		part := make([]byte, length)
		if _, err := io.ReadFull(stream, part); err != nil {
			return 0, err
		}
		var etag string
		err := withRetries(func() error {
			var err error
			etag, err = a.put(ctx, u.URL, "", bytes.NewReader(part), length)
			return err
		})
		if err != nil {
			return 0, err
		}
		recorded = append(recorded, request.RecordPartRequest{PartNumber: u.PartNumber, ETag: etag, SizeBytes: length})
		completed = append(completed, request.CompletedPartRequest{PartNumber: u.PartNumber, ETag: etag})
	}
	for start := 0; start < len(recorded); start += 100 {
		if err := a.confirmParts(ctx, jobID, pos, recorded[start:min(start+100, len(recorded))]); err != nil {
			return 0, err
		}
	}
	return completeWithRetries(ctx, a, jobID, pos, completed)
}

// completeWithRetries completes an entry, again after a failure:
// completing is idempotent.
func completeWithRetries(ctx context.Context, a *api, jobID string, pos int64, parts []request.CompletedPartRequest) (int, error) {
	var status int
	err := withRetries(func() error {
		var err error
		status, err = a.complete(ctx, jobID, pos, parts)
		return err
	})
	return status, err
}

func withRetries(fn func() error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
	}
	return err
}

// waitForJob polls until the job ends.
func waitForJob(ctx context.Context, a *api, jobID string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		job, err := a.job(ctx, jobID)
		if err == nil {
			switch job.State {
			case "COMPLETED", "PARTIALLY_COMPLETED", "FAILED", "CANCELLED":
				return nil
			}
		}
		time.Sleep(time.Second)
	}
	return errors.New("the job did not finish within -wait")
}

// probeScheduling claims without uploading until a limit holds the job
// back, then cancels it and waits for the cancellation to finish.
func probeScheduling(ctx context.Context, a *api, jobID string, max int) (*schedulingReport, error) {
	p := &schedulingReport{LimitedBy: map[string]int{}}
	started := time.Now()
	for {
		claim, err := a.claim(ctx, jobID, max)
		if err != nil {
			return p, err
		}
		p.Claims++
		p.Granted += len(claim.Grants)
		if claim.LimitedBy != "" {
			p.LimitedBy[claim.LimitedBy]++
		}
		if len(claim.Grants) == 0 {
			p.HeldBy = claim.LimitedBy
			break
		}
	}
	p.Seconds = time.Since(started).Seconds()

	cancelled := time.Now()
	if err := a.cancel(ctx, jobID); err != nil {
		return p, err
	}
	for {
		job, err := a.job(ctx, jobID)
		if err != nil {
			return p, err
		}
		if job.State == "CANCELLED" {
			break
		}
		if time.Since(cancelled) > 30*time.Minute {
			return p, errors.New("the cancellation did not finish in 30 minutes")
		}
		time.Sleep(500 * time.Millisecond)
	}
	p.CancelSeconds = time.Since(cancelled).Seconds()
	return p, nil
}

// collect gathers the samplers' and request statistics into the report.
func (r *report) collect(a *api, s *samplers) {
	st := a.stats
	st.mu.Lock()
	r.Requests, r.Failures, r.Errors = st.calls, st.failures, st.samples
	total, failed := 0, 0
	for k, n := range st.calls {
		total += n
		failed += st.failures[k]
	}
	kinds := make([]string, 0, len(st.calls))
	for k := range st.calls {
		kinds = append(kinds, k)
	}
	st.mu.Unlock()
	if total > 0 {
		r.ErrorRate = float64(failed) / float64(total)
	}
	r.Latency = map[string]any{}
	for _, k := range kinds {
		r.Latency[k] = map[string]float64{
			"p50": ms(st.percentile(k, 0.50)), "p95": ms(st.percentile(k, 0.95)), "p99": ms(st.percentile(k, 0.99)),
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	r.Backlog = s.progress
	if !s.progress.firstPublished.IsZero() && s.progress.lastPublished.After(s.progress.firstPublished) {
		r.ProcessingRate = float64(s.progress.published) / s.progress.lastPublished.Sub(s.progress.firstPublished).Seconds()
	}
	r.Self = map[string]any{"peak_heap_inuse_bytes": s.selfHeap, "peak_sys_bytes": s.selfSys}
	if len(s.servers) > 0 {
		r.Servers = map[string]any{}
		for url, m := range s.servers {
			r.Servers[url] = m
		}
	}
	if s.db != nil {
		after := s.dbAfter
		before := s.dbBefore
		hit := float64(0)
		if reads := (after.BlksHit - before.BlksHit) + (after.BlksRead - before.BlksRead); reads > 0 {
			hit = float64(after.BlksHit-before.BlksHit) / float64(reads)
		}
		r.Database = map[string]any{
			"sampled":         s.db,
			"commits":         after.Commits - before.Commits,
			"rollbacks":       after.Rollbacks - before.Rollbacks,
			"rows_inserted":   after.Inserted - before.Inserted,
			"rows_updated":    after.Updated - before.Updated,
			"rows_deleted":    after.Deleted - before.Deleted,
			"cache_hit_ratio": hit,
			"size_growth":     after.SizeBytes - before.SizeBytes,
			"error":           s.dbErr,
		}
	}
	if len(s.container) > 0 {
		r.Container = map[string]any{}
		for name, c := range s.container {
			r.Container[name] = c
		}
	}
	if r.Mode == "synthetic" {
		r.Notes = append(r.Notes, "synthetic mode: no file bytes were uploaded; the numbers measure manifest reading, scheduling and cancellation only")
	} else {
		r.Notes = append(r.Notes, "upload throughput is measured at the client: bytes PUT to the object store through presigned URLs")
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func (r *report) print(w io.Writer) {
	p := func(format string, args ...any) { fmt.Fprintf(w, format+"\n", args...) }
	p("ingestion load test: mode=%s tenant=%s job=%s", r.Mode, r.Tenant, r.JobID)
	p("  dataset: %d files %v, %.1f MiB declared, seed %d, concurrency %d", r.Files, r.ByClass, float64(r.ExpectedBytes)/(1<<20), r.Seed, r.Concurrency)
	if r.HashSeconds > 0 {
		p("  client checksums: %.1fs", r.HashSeconds)
	}
	rate := 0.0
	if r.ManifestSeconds > 0 {
		rate = float64(r.ManifestLines) / r.ManifestSeconds
	}
	p("  manifest (%s): %d lines, %.1f MiB in %.2fs = %.0f lines/s", r.ManifestSource, r.ManifestLines, float64(r.ManifestBytes)/(1<<20), r.ManifestSeconds, rate)
	if u := r.Upload; u != nil {
		p("  uploads: %d files, %.1f MiB in %.1fs = %.1f files/s, %.1f MiB/s to the object store; by strategy %v; failed entries %d; claims held back %v",
			u.Uploaded, float64(u.Bytes)/(1<<20), u.Seconds, u.FilesPerSec, u.MiBPerSec, u.ByStrategy, u.FailedEntries, u.LimitedBy)
	}
	if s := r.Scheduling; s != nil {
		p("  scheduling: %d entries granted in %d claims (%.2fs) before %q held the job; cancellation of the rest took %.2fs",
			s.Granted, s.Claims, s.Seconds, s.HeldBy, s.CancelSeconds)
	}
	p("  job: %s published=%d failed=%d invalid=%d cancelled=%d; end to end %.1fs; published %.1f files/s",
		r.FinalState, r.Progress.Published, r.Progress.Failed, r.Progress.Invalid, r.Progress.Cancelled, r.EndToEnd, r.ProcessingRate)
	p("  backlog: peak awaiting processing %d, peak uploads in flight %d, peak retryable %d",
		r.Backlog.PeakAwaitingProcessing, r.Backlog.PeakInFlight, r.Backlog.PeakRetryable)
	p("  requests: %v failures %v (error rate %.4f)", r.Requests, r.Failures, r.ErrorRate)
	for kind, l := range r.Latency {
		p("    %s latency ms %v", kind, l)
	}
	for _, e := range r.Errors {
		p("    error: %s", e)
	}
	p("  loadgen memory: %v", r.Self)
	for url, m := range r.Servers {
		mem := m.(*serverMemory)
		p("  server %s: peak heap in use %.1f MiB, peak sys %.1f MiB %s", url, float64(mem.PeakHeapInuse)/(1<<20), float64(mem.PeakSys)/(1<<20), mem.Error)
	}
	if r.Database != nil {
		raw, _ := json.Marshal(r.Database)
		p("  database: %s", raw)
	}
	for name, c := range r.Container {
		use := c.(*containerUse)
		p("  container %s: cpu peak %.1f%% mean %.1f%%, memory peak %.1f MiB", name, use.PeakCPU, use.MeanCPU, float64(use.PeakMemory)/(1<<20))
	}
	for _, n := range r.Notes {
		p("  note: %s", n)
	}
}
