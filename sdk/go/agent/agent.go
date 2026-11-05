// Package agent watches folders on an instrument, robot or workstation and
// sends what appears to the platform (docs/edge.md). It keeps a durable
// journal so a reboot loses nothing, sends the most important files first
// inside upload windows and a bandwidth cap, deletes a local copy only after
// the platform has confirmed its SHA-256, and says how much is waiting.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/sdk/go/osp"
)

// Version is reported in heartbeats.
const Version = "0.1.0"

// Watch modes.
const (
	ModeRun       = "run"       // subfolders are runs, finished when a marker appears
	ModeFiles     = "files"     // each file stands alone, once it stops changing
	ModeRecording = "recording" // rotating chunks of one logical recording
)

// Watch is one folder to watch.
type Watch struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	// Marker is the file that says a run is finished, such as
	// CopyComplete.txt (run mode; required).
	Marker string `json:"marker,omitempty"`
	// IncompleteAfterHours reports a run with no marker as incomplete, what
	// it has sent kept, after this long (0 waits for ever).
	IncompleteAfterHours float64 `json:"incomplete_after_hours,omitempty"`
	// SettleSeconds is how long a file must not change to count as written.
	SettleSeconds int `json:"settle_seconds,omitempty"`
	// RecordingPattern groups chunks: its first group names the recording,
	// as in ^(.+)_\d+\.mcap$ (recording mode; required).
	RecordingPattern string `json:"recording_pattern,omitempty"`
	// ChunkSeconds is how long a chunk covers, ending at its modified time.
	ChunkSeconds int `json:"chunk_seconds,omitempty"`
	// CloseAfterIdleSeconds ends a recording that gets no chunk for this
	// long (default 600).
	CloseAfterIdleSeconds int `json:"close_after_idle_seconds,omitempty"`
	// DeleteAfterConfirm removes each local copy once the platform has
	// confirmed its SHA-256.
	DeleteAfterConfirm bool `json:"delete_after_confirm,omitempty"`
}

// Rule raises a file's priority. The highest matching rule wins; among equals
// the oldest file goes first.
type Rule struct {
	// Match is a glob against the file's name or relative path.
	Match string `json:"match,omitempty"`
	// FlagSuffix matches when a file with this suffix sits beside it, as an
	// event flag: scene_0041.mcap.keep raises scene_0041.mcap.
	FlagSuffix string `json:"flag_suffix,omitempty"`
	// OlderThanHours matches a file waiting at least this long.
	OlderThanHours float64 `json:"older_than_hours,omitempty"`
	Priority       int     `json:"priority"`
}

// UploadWindow is when uploading may happen, in local time; a window may run
// past midnight.
type UploadWindow struct {
	From string `json:"from"` // "22:00"
	To   string `json:"to"`   // "06:00"
}

// Config is the agent's configuration file.
type Config struct {
	API       string `json:"api"`
	Token     string `json:"token,omitempty"`
	TokenFile string `json:"token_file,omitempty"`
	StateDir  string `json:"state_dir"`
	// BandwidthBytesPerSecond caps uploads; 0 means no cap.
	BandwidthBytesPerSecond int64          `json:"bandwidth_bytes_per_second,omitempty"`
	Windows                 []UploadWindow `json:"windows,omitempty"`
	// WindowBypassPriority sends files at or above it outside the windows.
	WindowBypassPriority int     `json:"window_bypass_priority,omitempty"`
	Rules                []Rule  `json:"rules,omitempty"`
	Watches              []Watch `json:"watches"`
	Concurrency          int     `json:"concurrency,omitempty"`
	ScanSeconds          int     `json:"scan_seconds,omitempty"`
	HeartbeatSeconds     int     `json:"heartbeat_seconds,omitempty"`

	// MQTT, when set, also keeps messages from a broker (see MQTT).
	MQTT *MQTT `json:"mqtt,omitempty"`
}

// LoadConfig reads a configuration file.
func LoadConfig(file string) (Config, error) {
	var c Config
	raw, err := os.ReadFile(file)
	if err != nil {
		return c, err
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("%s: %w", file, err)
	}
	if c.Token == "" && c.TokenFile != "" {
		token, err := os.ReadFile(c.TokenFile)
		if err != nil {
			return c, err
		}
		c.Token = strings.TrimSpace(string(token))
	}
	return c, c.Validate()
}

// Validate refuses a configuration that could not work.
func (c Config) Validate() error {
	switch {
	case c.API == "" || c.Token == "":
		return errors.New("api and token (or token_file) are required")
	case c.StateDir == "":
		return errors.New("state_dir is required: it holds the journal")
	case len(c.Watches) == 0 && c.MQTT == nil:
		return errors.New("at least one watch, or mqtt, is required")
	}
	if c.MQTT != nil {
		if err := c.MQTT.validate(); err != nil {
			return err
		}
	}
	for _, w := range c.Windows {
		if _, err := parseClock(w.From); err != nil {
			return fmt.Errorf("window from: %w", err)
		}
		if _, err := parseClock(w.To); err != nil {
			return fmt.Errorf("window to: %w", err)
		}
	}
	for _, w := range c.Watches {
		switch w.Mode {
		case ModeRun:
			if w.Marker == "" {
				return fmt.Errorf("watch %s: a run needs its marker file", w.Path)
			}
		case ModeRecording:
			if _, err := recordingRegexp(w.RecordingPattern); err != nil {
				return fmt.Errorf("watch %s: %w", w.Path, err)
			}
		case ModeFiles:
		default:
			return fmt.Errorf("watch %s: mode is run, files or recording", w.Path)
		}
	}
	return nil
}

func recordingRegexp(pattern string) (*regexp.Regexp, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("recording_pattern: %w", err)
	}
	if re.NumSubexp() < 1 {
		return nil, errors.New("recording_pattern needs a group naming the recording")
	}
	return re, nil
}

func parseClock(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, errors.New(`a time like "22:00"`)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// inWindow reports whether uploads may run at now.
func inWindow(windows []UploadWindow, now time.Time) bool {
	if len(windows) == 0 {
		return true
	}
	minute := now.Hour()*60 + now.Minute()
	for _, w := range windows {
		from, _ := parseClock(w.From)
		to, _ := parseClock(w.To)
		if from <= to && minute >= from && minute < to {
			return true
		}
		if from > to && (minute >= from || minute < to) {
			return true
		}
	}
	return false
}

// Agent does the work; Tick is one pass, and Run repeats it.
type Agent struct {
	cfg     Config
	client  *osp.Client
	journal *Journal
	limiter *osp.Limiter
	log     *slog.Logger
	now     func() time.Time
	device  device
	seen    map[string]seenFile // size and time last seen, for settling
	lastErr string
}

type device struct {
	Namespace  string `json:"namespace"`
	PathPrefix string `json:"path_prefix"`
}

type seenFile struct {
	size    int64
	modTime time.Time
	since   time.Time
}

// New builds an agent and reads its journal.
func New(cfg Config, log *slog.Logger) (*Agent, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	j, err := OpenJournal(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 2
	}
	if cfg.MQTT != nil {
		m := *cfg.MQTT
		m.withDefaults()
		cfg.MQTT = &m
		if err := os.MkdirAll(m.Dir, 0o755); err != nil {
			return nil, err
		}
		// The finished files go out as chunks of one recording.
		cfg.Watches = append(cfg.Watches, m.watch())
	}
	return &Agent{cfg: cfg, client: osp.New(cfg.API, cfg.Token), journal: j, limiter: osp.NewLimiter(cfg.BandwidthBytesPerSecond),
		log: log, now: time.Now, seen: map[string]seenFile{}}, nil
}

// Close releases the journal.
func (a *Agent) Close() error { return a.journal.Close() }

// Run works until ctx ends. A failed pass is logged and tried again: a device
// is offline often, and that is normal.
func (a *Agent) Run(ctx context.Context) error {
	scan := time.Duration(max(a.cfg.ScanSeconds, 1)) * time.Second
	if a.cfg.ScanSeconds == 0 {
		scan = 10 * time.Second
	}
	beat := time.Duration(max(a.cfg.HeartbeatSeconds, 5)) * time.Second
	if a.cfg.HeartbeatSeconds == 0 {
		beat = time.Minute
	}
	if a.cfg.MQTT != nil {
		go a.runMQTT(ctx)
	}
	go func() {
		ticker := time.NewTicker(beat)
		defer ticker.Stop()
		for {
			if err := a.Heartbeat(ctx); err != nil && ctx.Err() == nil {
				a.log.Warn("heartbeat failed", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	for {
		if err := a.Tick(ctx); err != nil && ctx.Err() == nil {
			a.log.Warn("pass failed; trying again", "error", err)
			a.lastErr = err.Error()
		} else {
			a.lastErr = ""
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(scan):
		}
	}
}

// Tick is one pass: find what is ready, send what the rules allow, confirm
// what was sent, and close what is finished. It stops at the first error it
// cannot pass over; what was done is in the journal.
func (a *Agent) Tick(ctx context.Context) error {
	if a.device.PathPrefix == "" {
		if err := a.client.Do(ctx, http.MethodGet, "/api/v1/edge/me", nil, &a.device); err != nil {
			return fmt.Errorf("who am I: %w", err)
		}
	}
	if err := a.scan(); err != nil {
		return err
	}
	if err := a.send(ctx); err != nil {
		return err
	}
	if err := a.report(ctx); err != nil {
		return err
	}
	if err := a.confirm(ctx); err != nil {
		return err
	}
	return a.closeFinished(ctx)
}

func (a *Agent) objectPath(rel string) string {
	return a.device.PathPrefix + path.Clean(filepath.ToSlash(rel))
}

func (a *Agent) priorityOf(file, rel string, firstSeen time.Time) int {
	best := 0
	for _, r := range a.cfg.Rules {
		matched := false
		if r.Match != "" {
			if ok, _ := path.Match(r.Match, filepath.Base(file)); ok {
				matched = true
			} else if ok, _ := path.Match(r.Match, filepath.ToSlash(rel)); ok {
				matched = true
			}
		}
		if r.FlagSuffix != "" {
			if _, err := os.Stat(file + r.FlagSuffix); err == nil {
				matched = true
			}
		}
		if r.OlderThanHours > 0 && a.now().Sub(firstSeen).Hours() >= r.OlderThanHours {
			matched = true
		}
		if matched && r.Priority > best {
			best = r.Priority
		}
	}
	return best
}

// settled reports whether a file has stopped changing.
func (a *Agent) settled(file string, info os.FileInfo, seconds int) bool {
	if seconds <= 0 {
		seconds = 30
	}
	last, ok := a.seen[file]
	now := a.now()
	if !ok || last.size != info.Size() || !last.modTime.Equal(info.ModTime()) {
		a.seen[file] = seenFile{size: info.Size(), modTime: info.ModTime(), since: now}
		return false
	}
	return now.Sub(last.since) >= time.Duration(seconds)*time.Second
}

// add journals a file that is ready. A file seen before and unchanged is
// left alone; one rewritten after it was sent starts again.
func (a *Agent) add(file, rel string, info os.FileInfo, e Entry) error {
	if old := a.journal.Get(file); old != nil && old.Size == info.Size() && old.ModTime.Equal(info.ModTime()) {
		return nil
	}
	sum, size, err := osp.SHA256File(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	e.File, e.Path, e.Size, e.ModTime, e.SHA256, e.State, e.FirstSeen = file, a.objectPath(rel), size, info.ModTime(), sum, StateQueued, a.now()
	e.Priority = a.priorityOf(file, rel, e.FirstSeen)
	return a.journal.Put(&e)
}

func (a *Agent) scan() error {
	for _, w := range a.cfg.Watches {
		var err error
		switch w.Mode {
		case ModeRun:
			err = a.scanRuns(w)
		case ModeFiles:
			err = a.scanFiles(w)
		case ModeRecording:
			err = a.scanRecording(w)
		}
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("scan %s: %w", w.Path, err)
		}
	}
	return nil
}

func listFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	return files, err
}

func (a *Agent) scanFiles(w Watch) error {
	files, err := listFiles(w.Path)
	if err != nil {
		return err
	}
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil || info.Size() == 0 {
			continue
		}
		rel, _ := filepath.Rel(w.Path, file)
		if a.journal.Get(file) == nil && !a.settled(file, info, w.SettleSeconds) {
			continue
		}
		if err := a.add(file, rel, info, Entry{Delete: w.DeleteAfterConfirm}); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) scanRuns(w Watch) error {
	folders, err := os.ReadDir(w.Path)
	if err != nil {
		return err
	}
	for _, folder := range folders {
		if !folder.IsDir() {
			continue
		}
		name := folder.Name()
		root := filepath.Join(w.Path, name)
		run := a.journal.Run(name)
		if run != nil && run.Sealed {
			continue
		}
		_, markerErr := os.Stat(filepath.Join(root, w.Marker))
		finished := markerErr == nil
		if run == nil {
			run = &RunState{Name: name, Kind: "RUN", LastFile: a.now()}
			if err := a.journal.PutRun(run); err != nil {
				return err
			}
		}
		timedOut := !finished && w.IncompleteAfterHours > 0 && a.now().Sub(run.LastFile).Hours() >= w.IncompleteAfterHours
		if !finished && !timedOut {
			continue // the instrument is still writing
		}
		files, err := listFiles(root)
		if err != nil {
			return err
		}
		expected := int64(0)
		for _, file := range files {
			info, err := os.Stat(file)
			if err != nil || info.Size() == 0 {
				continue // markers are usually empty, and empty files are not stored
			}
			expected++
			rel, _ := filepath.Rel(w.Path, file)
			if err := a.add(file, rel, info, Entry{Recording: name, Kind: "RUN", Delete: w.DeleteAfterConfirm}); err != nil {
				return err
			}
		}
		run.Sealed, run.Expected = true, expected
		if timedOut {
			run.Missing = []string{fmt.Sprintf("no %s after %.0f hours", w.Marker, w.IncompleteAfterHours)}
		}
		if err := a.journal.PutRun(run); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) scanRecording(w Watch) error {
	re, err := recordingRegexp(w.RecordingPattern)
	if err != nil {
		return err
	}
	files, err := listFiles(w.Path)
	if err != nil {
		return err
	}
	chunk := time.Duration(w.ChunkSeconds) * time.Second
	if chunk <= 0 {
		chunk = time.Minute
	}
	idle := time.Duration(w.CloseAfterIdleSeconds) * time.Second
	if idle <= 0 {
		idle = 10 * time.Minute
	}
	for _, file := range files {
		g := re.FindStringSubmatch(filepath.Base(file))
		if g == nil {
			continue
		}
		info, err := os.Stat(file)
		if err != nil || info.Size() == 0 || a.journal.Get(file) != nil {
			continue
		}
		if !a.settled(file, info, w.SettleSeconds) {
			continue
		}
		rel, _ := filepath.Rel(w.Path, file)
		end := info.ModTime().UTC()
		start := end.Add(-chunk)
		if err := a.add(file, rel, info, Entry{Recording: g[1], Kind: "RECORDING", Delete: w.DeleteAfterConfirm, StartsAt: &start, EndsAt: &end}); err != nil {
			return err
		}
		run := a.journal.Run(g[1])
		if run == nil {
			run = &RunState{Name: g[1], Kind: "RECORDING"}
		}
		run.LastFile = a.now()
		run.Sealed = false
		if err := a.journal.PutRun(run); err != nil {
			return err
		}
	}
	// A recording that has gone quiet is finished.
	for _, run := range a.journal.Runs() {
		if run.Kind == "RECORDING" && !run.Sealed && a.now().Sub(run.LastFile) >= idle {
			run.Sealed = true
			if err := a.journal.PutRun(run); err != nil {
				return err
			}
		}
	}
	return nil
}

// queue is what to send now, most important first, oldest among equals.
func (a *Agent) queue() []*Entry {
	var out []*Entry
	open := inWindow(a.cfg.Windows, a.now())
	for _, e := range a.journal.Entries() {
		if e.State != StateQueued {
			continue
		}
		// Age rules can raise a file that has waited.
		rel := strings.TrimPrefix(e.Path, a.device.PathPrefix)
		if p := a.priorityOf(e.File, rel, e.FirstSeen); p > e.Priority {
			e.Priority = p
		}
		bypass := a.cfg.WindowBypassPriority > 0 && e.Priority >= a.cfg.WindowBypassPriority
		if open || bypass {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].FirstSeen.Before(out[j].FirstSeen)
	})
	return out
}

func (a *Agent) send(ctx context.Context) error {
	queue := a.queue()
	if len(queue) == 0 {
		return nil
	}
	work := make(chan *Entry)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	for range a.cfg.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for e := range work {
				err := a.sendOne(ctx, e)
				if err != nil {
					mu.Lock()
					if first == nil {
						first = err
					}
					mu.Unlock()
				}
			}
		}()
	}
	for _, e := range queue {
		mu.Lock()
		stop := first != nil
		mu.Unlock()
		if stop || ctx.Err() != nil {
			break
		}
		work <- e
	}
	close(work)
	wg.Wait()
	return first
}

func (a *Agent) sendOne(ctx context.Context, e *Entry) error {
	info, err := os.Stat(e.File)
	if os.IsNotExist(err) {
		e.State, e.Reason = StateVanished, "the local file went away before it was sent"
		return a.journal.Put(e)
	}
	if err != nil {
		return err
	}
	if info.Size() != e.Size || !info.ModTime().Equal(e.ModTime) {
		// Rewritten since it was hashed: the next scan picks it up afresh.
		return a.journalForget(e)
	}
	opts := osp.UploadOptions{Dataset: a.device.Namespace, Path: e.Path, SHA256: e.SHA256, Store: entryStore{j: a.journal, file: e.File}, Limiter: a.limiter}
	if e.UploadID != "" {
		opts.Resume = &osp.Progress{UploadID: e.UploadID, PartSize: e.PartSize, Parts: e.Parts}
	}
	_, err = a.client.UploadFile(ctx, e.File, opts)
	if err != nil && e.UploadID != "" && (osp.IsStatus(err, http.StatusNotFound) || osp.IsStatus(err, http.StatusConflict) || osp.IsStatus(err, http.StatusGone)) {
		// The upload it would continue has expired: begin again.
		fresh := a.journal.Get(e.File)
		fresh.UploadID, fresh.PartSize, fresh.Parts = "", 0, nil
		if perr := a.journal.Put(fresh); perr != nil {
			return perr
		}
		opts.Resume = nil
		_, err = a.client.UploadFile(ctx, e.File, opts)
	}
	if err != nil {
		var refused *osp.APIError
		if errors.As(err, &refused) {
			switch {
			case refused.Status == http.StatusForbidden || refused.Status == http.StatusUnauthorized:
				return fmt.Errorf("the credential was refused: %w", err)
			case refused.Status >= 400 && refused.Status < 500 && refused.Status != http.StatusTooManyRequests:
				// This file will never go as it is; the others still can.
				current := a.journal.Get(e.File)
				current.State, current.Reason = StateRejected, refused.Message
				a.log.Warn("the platform refused a file; the local copy stays", "path", e.Path, "reason", refused.Message)
				return a.journal.Put(current)
			}
		}
		return fmt.Errorf("upload %s: %w", e.Path, err)
	}
	current := a.journal.Get(e.File)
	now := a.now()
	current.State, current.UploadedAt = StateUploaded, &now
	return a.journal.Put(current)
}

func (a *Agent) journalForget(e *Entry) error {
	e.Size, e.ModTime = -1, time.Time{}
	return a.journal.Put(e)
}

// report tells each recording about its sent chunks.
func (a *Agent) report(ctx context.Context) error {
	for _, e := range a.journal.Entries() {
		if e.Recording == "" || e.Reported || (e.State != StateUploaded && e.State != StateConfirmed) {
			continue
		}
		id, err := a.recordingID(ctx, e.Recording, e.Kind)
		if err != nil {
			return err
		}
		if err := a.client.Do(ctx, http.MethodPost, "/api/v1/edge/recordings/"+id+"/chunks", map[string]any{
			"path": e.Path, "sha256": e.SHA256, "size_bytes": e.Size, "starts_at": e.StartsAt, "ends_at": e.EndsAt}, nil); err != nil {
			return fmt.Errorf("report %s: %w", e.Path, err)
		}
		e.Reported = true
		if err := a.journal.Put(e); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) recordingID(ctx context.Context, name, kind string) (string, error) {
	run := a.journal.Run(name)
	if run != nil && run.ID != "" {
		return run.ID, nil
	}
	var opened struct {
		ID string `json:"id"`
	}
	if err := a.client.Do(ctx, http.MethodPost, "/api/v1/edge/recordings", map[string]any{"name": name, "kind": kind}, &opened); err != nil {
		return "", fmt.Errorf("open %s: %w", name, err)
	}
	if run == nil {
		run = &RunState{Name: name, Kind: kind, LastFile: a.now()}
	}
	run.ID = opened.ID
	return opened.ID, a.journal.PutRun(run)
}

// confirm asks about what was sent. A local copy goes only on AVAILABLE: the
// platform computed this SHA-256 from the stored bytes and its checks passed.
func (a *Agent) confirm(ctx context.Context) error {
	for _, e := range a.journal.Entries() {
		switch {
		case e.State == StateUploaded:
			var state struct {
				State  string `json:"state"`
				Reason string `json:"reason"`
			}
			q := osp.Query("/api/v1/edge/confirm", map[string]string{"path": e.Path, "sha256": e.SHA256})
			if err := a.client.Do(ctx, http.MethodGet, q, nil, &state); err != nil {
				return fmt.Errorf("confirm %s: %w", e.Path, err)
			}
			switch state.State {
			case "AVAILABLE":
				e.State, e.Reason = StateConfirmed, ""
			case "REJECTED":
				e.State, e.Reason = StateRejected, state.Reason
				a.log.Warn("the platform rejected a file; the local copy stays", "path", e.Path, "reason", state.Reason)
			case "MISSING":
				// Sent but never seen: after a while, send it again.
				if e.UploadedAt != nil && a.now().Sub(*e.UploadedAt) > 10*time.Minute {
					e.State, e.UploadID, e.PartSize, e.Parts = StateQueued, "", 0, nil
				}
			}
			if err := a.journal.Put(e); err != nil {
				return err
			}
			if e.State != StateConfirmed {
				continue
			}
			fallthrough
		case e.State == StateConfirmed && e.Delete && !e.Removed:
			if !e.Delete || e.Removed {
				continue
			}
			if err := os.Remove(e.File); err != nil && !os.IsNotExist(err) {
				return err
			}
			e.Removed = true
			if err := a.journal.Put(e); err != nil {
				return err
			}
		}
	}
	return nil
}

// closeFinished ends each sealed run or recording whose files are settled.
func (a *Agent) closeFinished(ctx context.Context) error {
	entries := a.journal.Entries()
	for _, run := range a.journal.Runs() {
		if !run.Sealed || run.Closed == "COMPLETE" {
			continue
		}
		var count int64
		missing := append([]string{}, run.Missing...)
		pending := false
		for _, e := range entries {
			if e.Recording != run.Name {
				continue
			}
			count++
			switch e.State {
			case StateConfirmed:
			case StateRejected:
				missing = append(missing, e.Path+": rejected "+e.Reason)
			case StateVanished:
				missing = append(missing, e.Path+": the local file went away")
			default:
				pending = true
			}
		}
		if pending || (run.Closed != "" && len(missing) == run.Told) {
			continue // nothing new to say yet
		}
		if count == 0 {
			continue
		}
		id, err := a.recordingID(ctx, run.Name, run.Kind)
		if err != nil {
			return err
		}
		var closed struct {
			State string `json:"state"`
		}
		expected := run.Expected
		if err := a.client.Do(ctx, http.MethodPost, "/api/v1/edge/recordings/"+id+"/close", map[string]any{"expected": expected, "missing": missing}, &closed); err != nil {
			return fmt.Errorf("close %s: %w", run.Name, err)
		}
		fresh := a.journal.Run(run.Name)
		fresh.Closed, fresh.Told = closed.State, len(missing)
		if err := a.journal.PutRun(fresh); err != nil {
			return err
		}
		a.log.Info("run closed", "name", run.Name, "state", closed.State, "files", count)
	}
	return nil
}

// Pending is what the device has not finished sending.
type Pending struct {
	Files, Bytes, Failed int64
	Oldest               *time.Time
}

// Status counts what is waiting.
func (a *Agent) Status() Pending {
	var p Pending
	for _, e := range a.journal.Entries() {
		switch e.State {
		case StateQueued, StateUploaded:
			p.Files++
			p.Bytes += max(e.Size, 0)
			if p.Oldest == nil || e.FirstSeen.Before(*p.Oldest) {
				seen := e.FirstSeen
				p.Oldest = &seen
			}
		case StateRejected, StateVanished:
			p.Failed++
		}
	}
	return p
}

// Heartbeat reports what is waiting on this device.
func (a *Agent) Heartbeat(ctx context.Context) error {
	p := a.Status()
	return a.client.Do(ctx, http.MethodPost, "/api/v1/edge/heartbeat", map[string]any{"pending_files": p.Files, "pending_bytes": p.Bytes,
		"failed_files": p.Failed, "oldest_pending": p.Oldest, "agent_version": Version, "note": a.lastErr}, nil)
}
