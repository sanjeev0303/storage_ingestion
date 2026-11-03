package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePlatform plays the API and storage.
type fakePlatform struct {
	mu        sync.Mutex
	srv       *httptest.Server
	objects   map[string]string // path -> sha256 of what arrived
	puts      int
	failPuts  int
	partPuts  map[string]int
	state     map[string]string // path -> confirm answer override
	chunks    map[string][]map[string]any
	closed    map[string]map[string]any
	beats     []map[string]any
	order     []string
	starts    int
	uploads   map[string]map[string]any
	closeWith string
	refuse    string
}

func newFake(t *testing.T) *fakePlatform {
	t.Helper()
	f := &fakePlatform{objects: map[string]string{}, partPuts: map[string]int{}, state: map[string]string{}, chunks: map[string][]map[string]any{},
		closed: map[string]map[string]any{}, uploads: map[string]map[string]any{}, closeWith: "COMPLETE"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePlatform) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	var body map[string]any
	if strings.HasPrefix(r.URL.Path, "/api/") && r.Method == http.MethodPost {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	switch {
	case r.URL.Path == "/api/v1/edge/me":
		reply(map[string]string{"namespace": "instruments", "path_prefix": "devices/novaseq/"})
	case r.URL.Path == "/api/v1/edge/heartbeat":
		f.beats = append(f.beats, body)
	case r.URL.Path == "/api/v1/uploads":
		if f.refuse != "" && strings.HasSuffix(body["path"].(string), f.refuse) {
			w.WriteHeader(http.StatusBadRequest)
			reply(map[string]string{"error": "refused"})
			return
		}
		id := fmt.Sprintf("u%d", len(f.uploads)+1)
		f.uploads[id] = body
		f.order = append(f.order, body["path"].(string))
		reply(map[string]any{"upload_id": id, "upload_url": f.srv.URL + "/storage/" + id})
	case r.URL.Path == "/api/v1/uploads/multipart":
		f.starts++
		id := fmt.Sprintf("m%d", len(f.uploads)+1)
		f.uploads[id] = body
		reply(map[string]any{"upload_id": id})
	case strings.HasSuffix(r.URL.Path, "/parts/urls"):
		id := strings.Split(r.URL.Path, "/")[4]
		var parts []map[string]any
		for _, n := range body["part_numbers"].([]any) {
			parts = append(parts, map[string]any{"part_number": int(n.(float64)), "url": fmt.Sprintf("%s/storage/%s?part=%d", f.srv.URL, id, int(n.(float64)))})
		}
		reply(map[string]any{"parts": parts})
	case strings.HasSuffix(r.URL.Path, "/parts/confirm"):
		reply(map[string]any{})
	case strings.HasSuffix(r.URL.Path, "/complete") && strings.HasPrefix(r.URL.Path, "/api/v1/uploads/"):
		id := strings.Split(r.URL.Path, "/")[4]
		up := f.uploads[id]
		f.objects[up["path"].(string)] = up["checksum_sha256"].(string)
		reply(map[string]any{})
	case strings.HasPrefix(r.URL.Path, "/storage/"):
		if f.failPuts > 0 {
			f.failPuts--
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		f.puts++
		f.partPuts[r.URL.RawQuery]++
		w.Header().Set("ETag", fmt.Sprintf(`"e%d"`, f.puts))
	case r.URL.Path == "/api/v1/edge/confirm":
		p := r.URL.Query().Get("path")
		s := f.state[p]
		if s == "" {
			s = "MISSING"
			if f.objects[p] == r.URL.Query().Get("sha256") {
				s = "AVAILABLE"
			}
		}
		reply(map[string]string{"state": s, "reason": "malware"})
	case r.URL.Path == "/api/v1/edge/recordings":
		reply(map[string]string{"id": "rec-" + body["name"].(string)})
	case strings.HasSuffix(r.URL.Path, "/chunks"):
		id := strings.Split(r.URL.Path, "/")[5]
		f.chunks[id] = append(f.chunks[id], body)
		reply(map[string]any{})
	case strings.HasSuffix(r.URL.Path, "/close"):
		id := strings.Split(r.URL.Path, "/")[5]
		f.closed[id] = body
		state := f.closeWith
		if m, _ := body["missing"].([]any); len(m) > 0 {
			state = "INCOMPLETE"
		}
		reply(map[string]string{"state": state})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func write(t *testing.T, file, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newAgent(t *testing.T, f *fakePlatform, state string, watches []Watch, change func(*Config)) *Agent {
	t.Helper()
	cfg := Config{API: f.srv.URL, Token: "t", StateDir: state, Watches: watches, Concurrency: 1}
	if change != nil {
		change(&cfg)
	}
	a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func tick(t *testing.T, a *Agent) {
	t.Helper()
	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestARunWaitsForItsMarkerThenCloses(t *testing.T) {
	f := newFake(t)
	root, state := t.TempDir(), t.TempDir()
	write(t, filepath.Join(root, "run-1", "L001.fastq"), "reads")
	a := newAgent(t, f, state, []Watch{{Path: root, Mode: ModeRun, Marker: "CopyComplete.txt"}}, nil)
	tick(t, a)
	if len(f.objects) != 0 {
		t.Fatalf("sent before the marker: %v", f.objects)
	}
	write(t, filepath.Join(root, "run-1", "L002.fastq"), "more reads")
	write(t, filepath.Join(root, "run-1", "CopyComplete.txt"), "")
	tick(t, a)
	if len(f.objects) != 2 || f.objects["devices/novaseq/run-1/L001.fastq"] == "" { // the empty marker is not sent
		t.Fatalf("objects: %v", f.objects)
	}
	got := f.closed["rec-run-1"]
	if got == nil || got["expected"].(float64) != 2 || len(f.chunks["rec-run-1"]) != 2 {
		t.Fatalf("close: %+v chunks %d", got, len(f.chunks["rec-run-1"]))
	}
	tick(t, a)
	if f.puts != 2 {
		t.Fatalf("sent again: %d", f.puts)
	}
}

func TestARunWithoutItsMarkerIsReportedIncomplete(t *testing.T) {
	f := newFake(t)
	root := t.TempDir()
	write(t, filepath.Join(root, "run-2", "L001.fastq"), "reads")
	a := newAgent(t, f, t.TempDir(), []Watch{{Path: root, Mode: ModeRun, Marker: "CopyComplete.txt", IncompleteAfterHours: 48}}, nil)
	clock := time.Now()
	a.now = func() time.Time { return clock }
	tick(t, a)
	clock = clock.Add(49 * time.Hour)
	tick(t, a)
	m := f.closed["rec-run-2"]["missing"].([]any)
	if len(m) != 1 || !strings.Contains(m[0].(string), "no CopyComplete.txt") {
		t.Fatalf("missing: %v", m)
	}
}

func TestALocalCopyGoesOnlyAfterConfirmation(t *testing.T) {
	f := newFake(t)
	root := t.TempDir()
	keep := filepath.Join(root, "bad.bin")
	gone := filepath.Join(root, "good.bin")
	wait := filepath.Join(root, "wait.bin")
	write(t, keep, "virus")
	write(t, gone, "fine")
	write(t, wait, "slow")
	f.state["devices/novaseq/bad.bin"] = "REJECTED"
	f.state["devices/novaseq/wait.bin"] = "PROCESSING"
	a := newAgent(t, f, t.TempDir(), []Watch{{Path: root, Mode: ModeFiles, SettleSeconds: 1, DeleteAfterConfirm: true}}, nil)
	clock := time.Now()
	a.now = func() time.Time { return clock }
	tick(t, a)
	clock = clock.Add(2 * time.Second)
	tick(t, a)
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Fatal("a confirmed copy stayed")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("a rejected file's local copy was deleted")
	}
	if _, err := os.Stat(wait); err != nil {
		t.Fatal("a file still being checked was deleted")
	}
	if s := a.Status(); s.Failed != 1 || s.Files != 1 {
		t.Fatalf("status: %+v", s)
	}
	delete(f.state, "devices/novaseq/wait.bin")
	f.objects["devices/novaseq/wait.bin"] = hexSHA("slow")
	tick(t, a)
	if _, err := os.Stat(wait); !os.IsNotExist(err) {
		t.Fatal("confirmed later, still not deleted")
	}
}

func hexSHA(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestAFileStillBeingWrittenWaits(t *testing.T) {
	f := newFake(t)
	root := t.TempDir()
	file := filepath.Join(root, "a.bin")
	write(t, file, "one")
	a := newAgent(t, f, t.TempDir(), []Watch{{Path: root, Mode: ModeFiles, SettleSeconds: 30}}, nil)
	clock := time.Now()
	a.now = func() time.Time { return clock }
	tick(t, a)
	clock = clock.Add(20 * time.Second)
	write(t, file, "one two")
	_ = os.Chtimes(file, clock, clock)
	tick(t, a)
	clock = clock.Add(20 * time.Second)
	tick(t, a)
	if len(f.objects) != 0 {
		t.Fatal("sent while it was changing")
	}
	clock = clock.Add(20 * time.Second)
	tick(t, a)
	if f.objects["devices/novaseq/a.bin"] != hexSHA("one two") {
		t.Fatalf("objects: %v", f.objects)
	}
}

func TestPriorityRulesAndWindows(t *testing.T) {
	f := newFake(t)
	root := t.TempDir()
	write(t, filepath.Join(root, "a-routine.bin"), "a")
	write(t, filepath.Join(root, "b-routine.bin"), "b")
	write(t, filepath.Join(root, "c-event.mcap"), "c")
	write(t, filepath.Join(root, "d-flagged.bin"), "d")
	write(t, filepath.Join(root, "d-flagged.bin.keep"), "")
	a := newAgent(t, f, t.TempDir(), []Watch{{Path: root, Mode: ModeFiles, SettleSeconds: 1}}, func(c *Config) {
		c.Rules = []Rule{{Match: "*.mcap", Priority: 5}, {FlagSuffix: ".keep", Priority: 9}}
		c.Windows = []UploadWindow{{From: "22:00", To: "06:00"}}
		c.WindowBypassPriority = 9
	})
	noon := time.Date(2026, 11, 2, 12, 0, 0, 0, time.Local)
	clock := noon
	a.now = func() time.Time { return clock }
	tick(t, a)
	clock = clock.Add(2 * time.Second)
	tick(t, a)
	// Outside the window, only what bypasses it goes.
	if len(f.order) != 1 || !strings.HasSuffix(f.order[0], "d-flagged.bin") {
		t.Fatalf("outside the window: %v", f.order)
	}
	clock = time.Date(2026, 11, 2, 23, 0, 0, 0, time.Local)
	tick(t, a)
	if len(f.order) < 4 || !strings.HasSuffix(f.order[1], "c-event.mcap") {
		t.Fatalf("inside the window, by priority: %v", f.order)
	}
}

// Markers are usually empty, and the platform stores nothing empty; and a file
// it refuses must not hold the others up.
func TestAnEmptyMarkerAndARefusedFileDoNotBlockARun(t *testing.T) {
	f := newFake(t)
	f.refuse = "too-big.bin"
	root := t.TempDir()
	write(t, filepath.Join(root, "run-3", "a.fastq"), "reads")
	write(t, filepath.Join(root, "run-3", "too-big.bin"), "refused")
	write(t, filepath.Join(root, "run-3", "CopyComplete.txt"), "")
	a := newAgent(t, f, t.TempDir(), []Watch{{Path: root, Mode: ModeRun, Marker: "CopyComplete.txt"}}, nil)
	tick(t, a)
	if f.objects["devices/novaseq/run-3/a.fastq"] == "" {
		t.Fatalf("objects: %v", f.objects)
	}
	got := f.closed["rec-run-3"]
	if got == nil || got["expected"].(float64) != 2 {
		t.Fatalf("close: %+v", got)
	}
	if m := got["missing"].([]any); len(m) != 1 || !strings.Contains(m[0].(string), "too-big.bin") {
		t.Fatalf("missing: %v", m)
	}
	if s := a.Status(); s.Failed != 1 || s.Files != 0 {
		t.Fatalf("status: %+v", s)
	}
}

func TestWindows(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 1, 1, h, m, 0, 0, time.Local) }
	night := []UploadWindow{{From: "22:00", To: "06:00"}}
	for _, c := range []struct {
		t    time.Time
		want bool
	}{{at(23, 0), true}, {at(2, 0), true}, {at(6, 0), false}, {at(12, 0), false}, {at(22, 0), true}} {
		if inWindow(night, c.t) != c.want {
			t.Errorf("%v", c.t)
		}
	}
	if !inWindow(nil, at(12, 0)) {
		t.Error("no windows means always")
	}
}

// A reboot in the middle of a large file: the journal says which parts went,
// and only the others are sent.
func TestAnInterruptedUploadContinuesAfterARestart(t *testing.T) {
	f := newFake(t)
	root, state := t.TempDir(), t.TempDir()
	big := filepath.Join(root, "big.bin")
	data := strings.Repeat("x", 80<<20) // 10 parts of 8 MiB
	write(t, big, data)
	watches := []Watch{{Path: root, Mode: ModeFiles, SettleSeconds: 1}}
	first := newAgent(t, f, state, watches, nil)
	clock := time.Now()
	first.now = func() time.Time { return clock }
	tick(t, first)
	clock = clock.Add(2 * time.Second)
	// Storage starts refusing after three parts: the pass fails midway.
	f.mu.Lock()
	f.failPuts = 0
	f.mu.Unlock()
	go func() {
		for {
			f.mu.Lock()
			if f.puts >= 3 {
				f.failPuts = 1000
				f.mu.Unlock()
				return
			}
			f.mu.Unlock()
			time.Sleep(time.Millisecond)
		}
	}()
	if err := first.Tick(context.Background()); err == nil {
		t.Fatal("the pass should have failed")
	}
	_ = first.Close()
	f.mu.Lock()
	f.failPuts = 0
	sent := f.puts
	starts := f.starts
	f.mu.Unlock()
	if sent >= 10 || sent < 3 {
		t.Fatalf("parts before the crash: %d", sent)
	}

	second := newAgent(t, f, state, watches, nil)
	tick(t, second)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.starts != starts {
		t.Fatalf("started a new upload: %d", f.starts)
	}
	if f.puts != 10 {
		t.Fatalf("parts in all: %d, want 10 with none sent twice", f.puts)
	}
	for part, n := range f.partPuts {
		if n != 1 {
			t.Fatalf("part %s sent %d times", part, n)
		}
	}
}

func TestAJournalCutShortStillReads(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Put(&Entry{File: "a", State: StateQueued, Size: 3}); err != nil {
		t.Fatal(err)
	}
	if err := entryStoreFor(j, "a").SaveUpload("u1", 8); err != nil {
		t.Fatal(err)
	}
	if err := entryStoreFor(j, "a").SavePart(2, "etag2"); err != nil {
		t.Fatal(err)
	}
	_ = j.Close()
	log, _ := os.OpenFile(filepath.Join(dir, "journal.log"), os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = log.WriteString(`{"e":{"file":"b","sta`) // power cut mid-write
	_ = log.Close()
	again, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	e := again.Get("a")
	if e == nil || e.UploadID != "u1" || e.Parts[2] != "etag2" {
		t.Fatalf("after replay: %+v", e)
	}
	if again.Get("b") != nil {
		t.Fatal("a half-written record was believed")
	}
}

func entryStoreFor(j *Journal, file string) entryStore { return entryStore{j: j, file: file} }

func TestRecordingChunksAndHeartbeat(t *testing.T) {
	f := newFake(t)
	root := t.TempDir()
	write(t, filepath.Join(root, "drive_0001.mcap"), "one")
	write(t, filepath.Join(root, "drive_0002.mcap"), "two")
	write(t, filepath.Join(root, "other.txt"), "ignored")
	a := newAgent(t, f, t.TempDir(), []Watch{{Path: root, Mode: ModeRecording, RecordingPattern: `^(.+)_\d+\.mcap$`, SettleSeconds: 1, ChunkSeconds: 60,
		CloseAfterIdleSeconds: 100, DeleteAfterConfirm: true}}, nil)
	clock := time.Now()
	a.now = func() time.Time { return clock }
	tick(t, a)
	clock = clock.Add(2 * time.Second)
	tick(t, a)
	chunks := f.chunks["rec-drive"]
	if len(chunks) != 2 || chunks[0]["starts_at"] == nil {
		t.Fatalf("chunks: %+v", chunks)
	}
	if _, err := os.Stat(filepath.Join(root, "drive_0001.mcap")); !os.IsNotExist(err) {
		t.Fatal("a confirmed chunk stayed")
	}
	if f.closed["rec-drive"] != nil {
		t.Fatal("closed while still recording")
	}
	clock = clock.Add(101 * time.Second)
	tick(t, a)
	if f.closed["rec-drive"] == nil {
		t.Fatal("a quiet recording did not close")
	}
	if err := a.Heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.beats[0]["agent_version"] != Version || f.beats[0]["pending_files"].(float64) != 0 {
		t.Fatalf("beat: %+v", f.beats[0])
	}
}

func TestConfigIsChecked(t *testing.T) {
	base := Config{API: "http://x", Token: "t", StateDir: "s"}
	for name, change := range map[string]func(*Config){
		"run without a marker":  func(c *Config) { c.Watches = []Watch{{Path: "p", Mode: ModeRun}} },
		"pattern without group": func(c *Config) { c.Watches = []Watch{{Path: "p", Mode: ModeRecording, RecordingPattern: `.*`}} },
		"unknown mode":          func(c *Config) { c.Watches = []Watch{{Path: "p", Mode: "x"}} },
		"bad window": func(c *Config) {
			c.Watches = []Watch{{Path: "p", Mode: ModeFiles}}
			c.Windows = []UploadWindow{{From: "late", To: "06:00"}}
		},
		"no watches": func(c *Config) {},
	} {
		c := base
		change(&c)
		if c.Validate() == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
