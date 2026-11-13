package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// After a recording closes COMPLETE the platform takes no more chunks into
// it: a chunk that comes later starts the next recording, rather than failing
// every pass from then on (MQTT does this after every quiet spell).
func TestAChunkAfterACompleteRecordingStartsTheNext(t *testing.T) {
	f := newFake(t)
	root := t.TempDir()
	clock := time.Now()
	a := newAgent(t, f, t.TempDir(), []Watch{{Path: root, Mode: ModeRecording, RecordingPattern: `^(rec)_\d+\.bin$`, SettleSeconds: 1, CloseAfterIdleSeconds: 60}}, nil)
	a.now = func() time.Time { return clock }
	write(t, filepath.Join(root, "rec_1.bin"), "one")
	tick(t, a)
	clock = clock.Add(2 * time.Second)
	tick(t, a)
	clock = clock.Add(2 * time.Minute)
	tick(t, a)
	if !f.complete["rec-rec"] {
		t.Fatalf("the first recording did not close COMPLETE: %v", f.closed)
	}

	write(t, filepath.Join(root, "rec_2.bin"), "two")
	tick(t, a)
	clock = clock.Add(2 * time.Second)
	if err := a.Tick(context.Background()); err != nil {
		t.Fatalf("a pass after the recording completed: %v", err)
	}
	if got := f.chunks["rec-rec~2"]; len(got) != 1 || !strings.HasSuffix(got[0]["path"].(string), "rec_2.bin") {
		t.Fatalf("chunks %v, want rec_2.bin in the next recording", f.chunks)
	}
	clock = clock.Add(2 * time.Minute)
	tick(t, a)
	if !f.complete["rec-rec~2"] {
		t.Fatalf("the next recording did not close: %v", f.closed)
	}
}

// The platform completed a recording but the journal never heard (the agent
// stopped first): the refused chunk moves to the next recording.
func TestARefusedChunkMovesToTheNextRecording(t *testing.T) {
	f := newFake(t)
	root, state := t.TempDir(), t.TempDir()
	j, err := OpenJournal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.PutRun(&RunState{Name: "rec", Kind: "RECORDING", ID: "rec-rec", LastFile: time.Now()}); err != nil {
		t.Fatal(err)
	}
	_ = j.Close()
	f.complete["rec-rec"] = true
	write(t, filepath.Join(root, "rec_2.bin"), "two")
	a := newAgent(t, f, state, []Watch{{Path: root, Mode: ModeRecording, RecordingPattern: `^(rec)_\d+\.bin$`, SettleSeconds: 1}}, nil)
	clock := time.Now()
	a.now = func() time.Time { return clock }
	tick(t, a)
	clock = clock.Add(2 * time.Second)
	if err := a.Tick(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(f.chunks["rec-rec~2"]) != 1 {
		t.Fatalf("chunks %v, want the refused one in the next recording", f.chunks)
	}
}

// A run file rewritten after it was hashed is hashed again and sent: only
// files mode scans a journaled file again.
func TestARunFileRewrittenAfterHashingIsSent(t *testing.T) {
	f := newFake(t)
	root := t.TempDir()
	file := filepath.Join(root, "run-1", "L001.fastq")
	write(t, file, "reads")
	write(t, filepath.Join(root, "run-1", "CopyComplete.txt"), "")
	a := newAgent(t, f, t.TempDir(), []Watch{{Path: root, Mode: ModeRun, Marker: "CopyComplete.txt"}}, func(c *Config) {
		c.Windows = []UploadWindow{{From: "00:00", To: "00:01"}}
	})
	a.now = func() time.Time { return time.Date(2026, 1, 1, 12, 0, 0, 0, time.Local) }
	tick(t, a) // hashed and queued; the window is closed
	write(t, file, "reads, rewritten")
	later := time.Now().Add(time.Minute)
	_ = os.Chtimes(file, later, later)
	a.cfg.Windows = nil
	for range 3 {
		tick(t, a)
	}
	if got := f.objects["devices/novaseq/run-1/L001.fastq"]; got != hexSHA("reads, rewritten") {
		t.Fatalf("objects %v, want the rewritten bytes", f.objects)
	}
	if !f.complete["rec-run-1"] {
		t.Fatalf("the run did not close: %v", f.closed)
	}
}

// A path another upload holds is busy, not refused: the file waits, and is
// sent once the path is free.
func TestABusyPathIsTriedAgainLater(t *testing.T) {
	f := newFake(t)
	f.busy = "a.bin"
	root := t.TempDir()
	write(t, filepath.Join(root, "a.bin"), "a")
	a := newAgent(t, f, t.TempDir(), []Watch{{Path: root, Mode: ModeFiles, SettleSeconds: 1}}, nil)
	clock := time.Now()
	a.now = func() time.Time { return clock }
	tick(t, a)
	clock = clock.Add(2 * time.Second)
	if err := a.Tick(context.Background()); err == nil {
		t.Fatal("a busy path should be reported")
	}
	if s := a.Status(); s.Failed != 0 || s.Files != 1 {
		t.Fatalf("status %+v: a busy path was rejected for good", s)
	}
	f.mu.Lock()
	f.busy = ""
	f.mu.Unlock()
	tick(t, a) // still waiting out its delay
	if len(f.objects) != 0 {
		t.Fatal("tried again at once")
	}
	clock = clock.Add(time.Hour)
	tick(t, a)
	if f.objects["devices/novaseq/a.bin"] != hexSHA("a") {
		t.Fatalf("objects %v", f.objects)
	}
}

// A file that cannot be read (a sharing violation on Windows) holds nothing
// else up: the others are hashed, sent and confirmed, and the pass says what
// failed.
func TestAnUnreadableFileHoldsNothingUp(t *testing.T) {
	f := newFake(t)
	root := t.TempDir()
	for _, name := range []string{"0.bin", "a.bin", "b.bin", "c.bin"} {
		write(t, filepath.Join(root, name), name)
	}
	lockFile(t, filepath.Join(root, "0.bin")) // cannot be hashed
	a := newAgent(t, f, t.TempDir(), []Watch{{Path: root, Mode: ModeFiles, SettleSeconds: 1}}, func(c *Config) {
		c.Windows = []UploadWindow{{From: "00:00", To: "00:01"}}
	})
	clock := time.Date(2026, 1, 1, 12, 0, 0, 0, time.Local)
	a.now = func() time.Time { return clock }
	tick(t, a)
	clock = clock.Add(2 * time.Second)
	if err := a.Tick(context.Background()); err == nil || !strings.Contains(err.Error(), "0.bin") {
		t.Fatalf("pass error %v, want the file that could not be hashed", err)
	}
	lockFile(t, filepath.Join(root, "a.bin")) // hashed, but cannot be sent
	a.cfg.Windows = nil
	err := a.Tick(context.Background())
	if err == nil || !strings.Contains(err.Error(), "a.bin") {
		t.Fatalf("pass error %v, want the file that could not be sent", err)
	}
	for _, name := range []string{"b.bin", "c.bin"} {
		if f.objects["devices/novaseq/"+name] != hexSHA(name) {
			t.Fatalf("objects %v: %s was held up", f.objects, name)
		}
	}
	if s := a.Status(); s.Files != 1 {
		t.Fatalf("status %+v, want only a.bin waiting", s)
	}
}

// A file rewritten after it was sent waits to settle again before it is
// hashed: it may still be being written.
func TestARewrittenFileSettlesAgain(t *testing.T) {
	f := newFake(t)
	root := t.TempDir()
	file := filepath.Join(root, "a.bin")
	write(t, file, "one")
	a := newAgent(t, f, t.TempDir(), []Watch{{Path: root, Mode: ModeFiles, SettleSeconds: 30}}, nil)
	clock := time.Now()
	a.now = func() time.Time { return clock }
	tick(t, a)
	clock = clock.Add(31 * time.Second)
	tick(t, a)
	write(t, file, "one two")
	_ = os.Chtimes(file, clock, clock)
	tick(t, a)
	if f.objects["devices/novaseq/a.bin"] != hexSHA("one") {
		t.Fatalf("objects %v: sent while it may still be changing", f.objects)
	}
	clock = clock.Add(31 * time.Second)
	tick(t, a)
	if f.objects["devices/novaseq/a.bin"] != hexSHA("one two") {
		t.Fatalf("objects %v, want the rewrite once settled", f.objects)
	}
}

// Reading the journal back drops files whose work is over: confirmed, the
// local copy deleted, and any recording they belong to closed COMPLETE.
func TestTheJournalForgetsFinishedFiles(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []*RunState{{Name: "done", Closed: "COMPLETE"}, {Name: "open"}} {
		if err := j.PutRun(r); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []*Entry{
		{File: "gone", State: StateConfirmed, Removed: true},
		{File: "gone-in-done", State: StateConfirmed, Removed: true, Recording: "done"},
		{File: "gone-in-open", State: StateConfirmed, Removed: true, Recording: "open"},
		{File: "kept-locally", State: StateConfirmed},
		{File: "queued", State: StateQueued},
	} {
		if err := j.Put(e); err != nil {
			t.Fatal(err)
		}
	}
	_ = j.Close()
	again, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	var files []string
	for _, e := range again.Entries() {
		files = append(files, e.File)
	}
	if strings.Join(files, ",") != "gone-in-open,kept-locally,queued" {
		t.Fatalf("entries %v", files)
	}
	if again.Run("done") == nil {
		t.Fatal("a closed run must stay: its name is not used again")
	}
}
