package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
)

// Samplers watch the run from outside: the job's progress, the servers'
// memory (expvar), PostgreSQL and the local containers. Each runs in one
// goroutine until the run's context ends.

type samplers struct {
	wg sync.WaitGroup
	mu sync.Mutex

	selfHeap uint64
	selfSys  uint64

	servers map[string]*serverMemory

	db        *dbSample
	dbBefore  dbCounters
	dbAfter   dbCounters
	dbErr     string
	container map[string]*containerUse

	// activity counts, per wait and statement, the active PostgreSQL
	// sessions seen at each of activitySamples readings.
	activity        map[string]int
	activitySamples int

	progress progressSample
}

type serverMemory struct {
	PeakHeapInuse uint64           `json:"peak_heap_inuse_bytes"`
	PeakSys       uint64           `json:"peak_sys_bytes"`
	Ingestion     map[string]int64 `json:"ingestion_counters,omitempty"`
	Error         string           `json:"error,omitempty"`
}

type dbSample struct {
	PeakConnections    int     `json:"peak_connections"`
	PeakActive         int     `json:"peak_active_queries"`
	PeakOutboxPending  int     `json:"peak_outbox_pending"`
	PeakProcessingJobs int     `json:"peak_processing_pending"`
	OldestOutboxAge    float64 `json:"oldest_pending_outbox_seconds"`
	OldestJobAge       float64 `json:"oldest_pending_processing_seconds"`
}

type dbCounters struct {
	Commits   int64 `json:"commits"`
	Rollbacks int64 `json:"rollbacks"`
	Inserted  int64 `json:"rows_inserted"`
	Updated   int64 `json:"rows_updated"`
	Deleted   int64 `json:"rows_deleted"`
	BlksRead  int64 `json:"blocks_read"`
	BlksHit   int64 `json:"blocks_hit"`
	SizeBytes int64 `json:"database_bytes"`
}

type containerUse struct {
	PeakCPU    float64 `json:"peak_cpu_percent"`
	MeanCPU    float64 `json:"mean_cpu_percent"`
	PeakMemory int64   `json:"peak_memory_bytes"`
	samples    int
	cpuTotal   float64
}

// progressSample tracks the job while it runs.
type progressSample struct {
	PeakAwaitingProcessing int64 `json:"peak_awaiting_processing"`
	PeakInFlight           int64 `json:"peak_uploads_in_flight"`
	PeakRetryable          int64 `json:"peak_failed_retryable"`
	// firstPublished and lastPublished bound the processing window.
	firstPublished time.Time
	lastPublished  time.Time
	published      int64
}

func newSamplers() *samplers {
	return &samplers{servers: map[string]*serverMemory{}, container: map[string]*containerUse{}}
}

func (s *samplers) every(ctx context.Context, interval time.Duration, fn func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			fn()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// self samples this process's own memory.
func (s *samplers) self(ctx context.Context) {
	s.every(ctx, 500*time.Millisecond, func() {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		s.mu.Lock()
		s.selfHeap = max(s.selfHeap, m.HeapInuse)
		s.selfSys = max(s.selfSys, m.Sys)
		s.mu.Unlock()
	})
}

// expvar samples each server's /debug/vars for memory and its ingestion
// counters.
func (s *samplers) expvar(ctx context.Context, urls []string) {
	client := &http.Client{Timeout: 5 * time.Second}
	for _, url := range urls {
		s.servers[url] = &serverMemory{}
		s.every(ctx, time.Second, func() {
			var vars struct {
				Memstats struct {
					HeapInuse uint64
					Sys       uint64
				} `json:"memstats"`
				Ingestion map[string]int64 `json:"ingestion"`
			}
			resp, err := client.Get(url)
			if err == nil {
				err = json.NewDecoder(resp.Body).Decode(&vars)
				resp.Body.Close()
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			m := s.servers[url]
			if err != nil {
				m.Error = err.Error()
				return
			}
			m.PeakHeapInuse = max(m.PeakHeapInuse, vars.Memstats.HeapInuse)
			m.PeakSys = max(m.PeakSys, vars.Memstats.Sys)
			if vars.Ingestion != nil {
				m.Ingestion = vars.Ingestion
			}
		})
	}
}

// database samples PostgreSQL: connections, the outbox and processing
// backlogs, and the database's activity counters at the start and end.
func (s *samplers) database(ctx context.Context, url string) error {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	if s.dbBefore, err = readCounters(ctx, conn); err != nil {
		conn.Close(context.Background())
		return err
	}
	s.db = &dbSample{}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer conn.Close(context.Background())
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			var d dbSample
			err := conn.QueryRow(ctx, `
				SELECT (SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()),
				       (SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND state = 'active'),
				       (SELECT count(*) FROM outbox_events WHERE status = 'PENDING'),
				       (SELECT count(*) FROM processing_jobs WHERE state = 'PENDING'),
				       COALESCE((SELECT extract(epoch FROM now() - min(created_at)) FROM outbox_events WHERE status = 'PENDING'), 0),
				       COALESCE((SELECT extract(epoch FROM now() - min(created_at)) FROM processing_jobs WHERE state = 'PENDING'), 0)
			`).Scan(&d.PeakConnections, &d.PeakActive, &d.PeakOutboxPending, &d.PeakProcessingJobs, &d.OldestOutboxAge, &d.OldestJobAge)
			s.mu.Lock()
			if err == nil {
				s.db.PeakConnections = max(s.db.PeakConnections, d.PeakConnections)
				s.db.PeakActive = max(s.db.PeakActive, d.PeakActive)
				s.db.PeakOutboxPending = max(s.db.PeakOutboxPending, d.PeakOutboxPending)
				s.db.PeakProcessingJobs = max(s.db.PeakProcessingJobs, d.PeakProcessingJobs)
				s.db.OldestOutboxAge = max(s.db.OldestOutboxAge, d.OldestOutboxAge)
				s.db.OldestJobAge = max(s.db.OldestJobAge, d.OldestJobAge)
			} else if ctx.Err() == nil {
				s.dbErr = err.Error()
			}
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				// The run is over: one last reading of the counters.
				final, err := readCounters(context.Background(), conn)
				s.mu.Lock()
				if err == nil {
					s.dbAfter = final
				} else {
					s.dbErr = err.Error()
				}
				s.mu.Unlock()
				return
			case <-ticker.C:
			}
		}
	}()
	return nil
}

// statements samples PostgreSQL's active sessions ten times a second: each
// one's statement (its opening text) and what it waits on, or CPU. Over a
// run the counts show where the database's time goes, lock waits included.
func (s *samplers) statements(ctx context.Context, url string) error {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	s.activity = map[string]int{}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer conn.Close(context.Background())
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			rows, err := conn.Query(ctx, `
				SELECT COALESCE(wait_event_type || ':' || wait_event, 'CPU'), left(regexp_replace(query, '\s+', ' ', 'g'), 70)
				FROM pg_stat_activity
				WHERE datname = current_database() AND state = 'active' AND pid <> pg_backend_pid()`)
			if err == nil {
				seen := map[string]int{}
				for rows.Next() {
					var wait, statement string
					if rows.Scan(&wait, &statement) == nil {
						seen[wait+"\t"+statement]++
					}
				}
				rows.Close()
				s.mu.Lock()
				s.activitySamples++
				for k, n := range seen {
					s.activity[k] += n
				}
				s.mu.Unlock()
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return nil
}

func readCounters(ctx context.Context, conn *pgx.Conn) (dbCounters, error) {
	var c dbCounters
	err := conn.QueryRow(ctx, `
		SELECT xact_commit, xact_rollback, tup_inserted, tup_updated, tup_deleted, blks_read, blks_hit,
		       pg_database_size(current_database())
		FROM pg_stat_database WHERE datname = current_database()
	`).Scan(&c.Commits, &c.Rollbacks, &c.Inserted, &c.Updated, &c.Deleted, &c.BlksRead, &c.BlksHit, &c.SizeBytes)
	return c, err
}

// containers samples `docker stats` for containers whose name starts with
// prefix. One reading takes about two seconds.
func (s *samplers) containers(ctx context.Context, prefix string) {
	s.every(ctx, 3*time.Second, func() {
		out, err := exec.CommandContext(ctx, "docker", "stats", "--no-stream", "--format", "{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}").Output()
		if err != nil {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			fields := strings.Split(line, "\t")
			if len(fields) != 3 || !strings.HasPrefix(fields[0], prefix) {
				continue
			}
			cpu, _ := strconv.ParseFloat(strings.TrimSuffix(fields[1], "%"), 64)
			used, _, _ := strings.Cut(fields[2], " / ")
			c := s.container[fields[0]]
			if c == nil {
				c = &containerUse{}
				s.container[fields[0]] = c
			}
			c.samples++
			c.cpuTotal += cpu
			c.MeanCPU = c.cpuTotal / float64(c.samples)
			c.PeakCPU = max(c.PeakCPU, cpu)
			if n, err := parseDockerSize(used); err == nil {
				c.PeakMemory = max(c.PeakMemory, n)
			}
		}
	})
}

// parseDockerSize reads docker's sizes such as 512KiB, 1.5GiB or 300MB.
func parseDockerSize(s string) (int64, error) {
	units := []struct {
		suffix string
		factor float64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"GB", 1e9}, {"MB", 1e6}, {"kB", 1e3}, {"B", 1}}
	s = strings.TrimSpace(s)
	for _, u := range units {
		if number, ok := strings.CutSuffix(s, u.suffix); ok {
			f, err := strconv.ParseFloat(number, 64)
			return int64(f * u.factor), err
		}
	}
	return strconv.ParseInt(s, 10, 64)
}

// watchJob polls the job's progress.
func (s *samplers) watchJob(ctx context.Context, a *api, jobID string) {
	s.every(ctx, time.Second, func() {
		job, err := a.job(ctx, jobID)
		if err != nil {
			return
		}
		s.noteProgress(job)
	})
}

func (s *samplers) noteProgress(job response.IngestionJobResponse) {
	p := job.Progress
	s.mu.Lock()
	defer s.mu.Unlock()
	s.progress.PeakAwaitingProcessing = max(s.progress.PeakAwaitingProcessing, p.Uploaded+p.Processing)
	s.progress.PeakInFlight = max(s.progress.PeakInFlight, p.Scheduled+p.Uploading)
	s.progress.PeakRetryable = max(s.progress.PeakRetryable, p.FailedRetryable)
	if p.Published > s.progress.published {
		now := time.Now()
		if s.progress.published == 0 {
			s.progress.firstPublished = now
		}
		s.progress.lastPublished = now
		s.progress.published = p.Published
	}
}

func (s *samplers) wait() { s.wg.Wait() }
