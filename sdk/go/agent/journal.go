package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/sdk/go/osp"
)

// Entry states.
const (
	StateQueued    = "QUEUED"    // found and hashed, waiting its turn
	StateUploaded  = "UPLOADED"  // bytes sent; the platform has not confirmed them
	StateConfirmed = "CONFIRMED" // the platform computed this SHA-256 and passed its checks
	StateRejected  = "REJECTED"  // the platform refused it; the local copy stays
	StateVanished  = "VANISHED"  // the local file went away before it was sent
)

// Entry is what the journal remembers of one local file.
type Entry struct {
	File       string         `json:"file"`
	Path       string         `json:"path"`
	Size       int64          `json:"size"`
	ModTime    time.Time      `json:"mod_time"`
	SHA256     string         `json:"sha256"`
	Priority   int            `json:"priority"`
	Recording  string         `json:"recording,omitempty"`
	Kind       string         `json:"kind,omitempty"`
	StartsAt   *time.Time     `json:"starts_at,omitempty"`
	EndsAt     *time.Time     `json:"ends_at,omitempty"`
	Delete     bool           `json:"delete,omitempty"`
	State      string         `json:"state"`
	FirstSeen  time.Time      `json:"first_seen"`
	UploadedAt *time.Time     `json:"uploaded_at,omitempty"`
	Reported   bool           `json:"reported,omitempty"` // the recording knows this chunk
	Removed    bool           `json:"removed,omitempty"`  // the local copy was deleted
	Reason     string         `json:"reason,omitempty"`
	Tries      int            `json:"tries,omitempty"`    // refusals that pass in time, in a row
	NextTry    *time.Time     `json:"next_try,omitempty"` // not sent again before this
	UploadID   string         `json:"upload_id,omitempty"`
	PartSize   int64          `json:"part_size,omitempty"`
	Parts      map[int]string `json:"parts,omitempty"`
}

// RunState is what the journal remembers of one run or recording.
type RunState struct {
	Name     string    `json:"name"`
	Kind     string    `json:"kind"`
	ID       string    `json:"id,omitempty"`
	Sealed   bool      `json:"sealed,omitempty"` // no more files will come
	Expected int64     `json:"expected,omitempty"`
	Missing  []string  `json:"missing,omitempty"`
	Closed   string    `json:"closed,omitempty"` // COMPLETE, or INCOMPLETE as last told
	Told     int       `json:"told,omitempty"`   // how many problems the platform was last told of
	LastFile time.Time `json:"last_file"`
}

// Journal is the agent's memory: an append-only file, synced to disk after
// each record, so a power cut loses nothing that was acknowledged. Reading it
// back replays the records and rewrites it compactly.
type Journal struct {
	mu      sync.Mutex
	path    string
	f       *os.File
	entries map[string]*Entry
	runs    map[string]*RunState
}

type record struct {
	Entry *Entry    `json:"e,omitempty"`
	Run   *RunState `json:"r,omitempty"`
	// A part sent: small, because a large file has thousands.
	File string `json:"f,omitempty"`
	Part int    `json:"p,omitempty"`
	ETag string `json:"t,omitempty"`
}

// OpenJournal reads the journal in dir, or starts one. A last line cut short
// by a power cut is ignored: it was never acknowledged.
func OpenJournal(dir string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	j := &Journal{path: filepath.Join(dir, "journal.log"), entries: map[string]*Entry{}, runs: map[string]*RunState{}}
	if f, err := os.Open(j.path); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<16), 64<<20)
		for sc.Scan() {
			var r record
			if json.Unmarshal(sc.Bytes(), &r) != nil {
				continue
			}
			j.apply(r)
		}
		err = sc.Err()
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("read journal: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := j.compact(); err != nil {
		return nil, err
	}
	return j, nil
}

func (j *Journal) apply(r record) {
	switch {
	case r.Entry != nil:
		j.entries[r.Entry.File] = r.Entry
	case r.Run != nil:
		j.runs[r.Run.Name] = r.Run
	case r.File != "":
		if e := j.entries[r.File]; e != nil {
			if e.Parts == nil {
				e.Parts = map[int]string{}
			}
			e.Parts[r.Part] = r.ETag
		}
	}
}

// compact rewrites the journal without what finished work no longer needs,
// whole or not at all.
func (j *Journal) compact() error {
	tmp := j.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, r := range j.runs {
		if err := enc.Encode(record{Run: r}); err != nil {
			f.Close()
			return err
		}
	}
	for file, e := range j.entries {
		// A file confirmed and deleted needs nothing more, once any recording
		// it belongs to is COMPLETE: until then closing it counts its files.
		run := j.runs[e.Recording]
		if e.State == StateConfirmed && e.Removed && (e.Recording == "" || (run != nil && run.Closed == "COMPLETE")) {
			delete(j.entries, file)
			continue
		}
		if err := enc.Encode(record{Entry: e}); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	if j.f != nil {
		j.f.Close()
	}
	if err := os.Rename(tmp, j.path); err != nil {
		return err
	}
	j.f, err = os.OpenFile(j.path, os.O_APPEND|os.O_WRONLY, 0o600)
	return err
}

func (j *Journal) write(r record) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := j.f.Write(append(raw, '\n')); err != nil {
		return err
	}
	return j.f.Sync()
}

// Close closes the file.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.f.Close()
}

func copyEntry(e *Entry) *Entry {
	c := *e
	if e.Parts != nil {
		c.Parts = make(map[int]string, len(e.Parts))
		for k, v := range e.Parts {
			c.Parts[k] = v
		}
	}
	return &c
}

// Put records an entry.
func (j *Journal) Put(e *Entry) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	c := copyEntry(e)
	j.entries[e.File] = c
	return j.write(record{Entry: c})
}

// Get returns a copy of an entry, or nil.
func (j *Journal) Get(file string) *Entry {
	j.mu.Lock()
	defer j.mu.Unlock()
	if e := j.entries[file]; e != nil {
		return copyEntry(e)
	}
	return nil
}

// Entries returns copies of the entries, in file order.
func (j *Journal) Entries() []*Entry {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]*Entry, 0, len(j.entries))
	for _, e := range j.entries {
		out = append(out, copyEntry(e))
	}
	sort.Slice(out, func(a, b int) bool { return strings.Compare(out[a].File, out[b].File) < 0 })
	return out
}

// PutRun records a run or recording.
func (j *Journal) PutRun(r *RunState) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	c := *r
	j.runs[r.Name] = &c
	return j.write(record{Run: &c})
}

// Run returns a copy of a run, or nil.
func (j *Journal) Run(name string) *RunState {
	j.mu.Lock()
	defer j.mu.Unlock()
	if r := j.runs[name]; r != nil {
		c := *r
		return &c
	}
	return nil
}

// Runs returns copies of the runs.
func (j *Journal) Runs() []*RunState {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]*RunState, 0, len(j.runs))
	for _, r := range j.runs {
		c := *r
		out = append(out, &c)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

// entryStore records one upload's progress in the journal.
type entryStore struct {
	j    *Journal
	file string
}

// The entry already names its file (size, time and SHA-256), so only the
// upload is kept.
func (s entryStore) SaveUpload(p osp.Progress) error {
	s.j.mu.Lock()
	defer s.j.mu.Unlock()
	e := s.j.entries[s.file]
	if e == nil {
		return fmt.Errorf("no journal entry for %s", s.file)
	}
	e.UploadID, e.PartSize, e.Parts = p.UploadID, p.PartSize, map[int]string{}
	return s.j.write(record{Entry: e})
}

func (s entryStore) SavePart(part int, etag string) error {
	s.j.mu.Lock()
	defer s.j.mu.Unlock()
	if e := s.j.entries[s.file]; e != nil {
		if e.Parts == nil {
			e.Parts = map[int]string{}
		}
		e.Parts[part] = etag
	}
	return s.j.write(record{File: s.file, Part: part, ETag: etag})
}
