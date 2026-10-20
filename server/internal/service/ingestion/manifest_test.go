package ingestion

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

var testLimits = ManifestLimits{
	MaxLineBytes: 4096, MaxEntries: 2_000_000, MaxMetadataBytes: 512, MaxMetadataKeys: 4,
	MaxObjectBytes: 1 << 40, SmallFileThreshold: 16 << 20, Path: model.PathPolicy{MaxBytes: 1024, MaxSegmentBytes: 255, MaxDepth: 8},
}

const sum = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

func readAll(t *testing.T, manifest string, limits ManifestLimits) []Line {
	t.Helper()
	m := NewManifestReader(strings.NewReader(manifest), limits, 0, 0, 0)
	var lines []Line
	for {
		line, err := m.Next()
		if errors.Is(err, io.EOF) {
			return lines
		}
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
}

func TestManifestValidEntries(t *testing.T) {
	manifest := `{"path":"a/one.txt","size":12,"content_type":"text/plain","checksum":"sha256:` + sum + `"}
{"path":"b/big.bin","size":104857600,"content_type":"application/octet-stream","checksum":"sha256:` + sum + `","required":false,"metadata":{"source":"cam-1","b":"2"}}

{"path":"c/two.txt","size":7,"content_type":"text/plain; charset=utf-8","checksum":"SHA256:` + strings.ToUpper(sum) + `"}`
	lines := readAll(t, manifest, testLimits)
	if len(lines) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(lines))
	}
	first, big, third := lines[0].Entry, lines[1].Entry, lines[2].Entry
	if first.State != model.EntryPending || first.Position != 1 || first.Strategy != model.StrategySingle || !first.Required {
		t.Fatalf("first %+v", first)
	}
	if big.Strategy != model.StrategyMultipart || big.Required || big.ChecksumSHA256 != sum || string(big.Metadata) != `{"b":"2","source":"cam-1"}` {
		t.Fatalf("big %+v %s", big, big.Metadata)
	}
	// The blank line keeps its number: the third entry is line 4.
	if third.Position != 4 || third.ChecksumSHA256 != sum || third.State != model.EntryPending {
		t.Fatalf("third %+v", third)
	}
	if lines[2].End != int64(len(manifest)) {
		t.Fatalf("the last offset must be the manifest's length: %d != %d", lines[2].End, len(manifest))
	}
}

func TestManifestInvalidEntriesAreReportedNotFatal(t *testing.T) {
	cases := map[string]string{
		`not json`: model.EntryErrInvalidJSON,
		`{"path":"../x","size":1,"content_type":"text/plain"}`:                                                       model.EntryErrInvalidPath,
		`{"path":"x.txt","content_type":"text/plain"}`:                                                               model.EntryErrInvalidSize,
		`{"path":"x.txt","size":-1,"content_type":"text/plain"}`:                                                     model.EntryErrInvalidSize,
		`{"path":"x.txt","size":2199023255553,"content_type":"text/plain"}`:                                          model.EntryErrTooLarge,
		`{"path":"x.txt","size":1,"content_type":"text/plain","checksum":"md5:abc"}`:                                 model.EntryErrInvalidChecksum,
		`{"path":"x.txt","size":1}`:                                                                                  model.EntryErrInvalidContentType,
		`{"path":"x.txt","size":1,"content_type":"not a type"}`:                                                      model.EntryErrInvalidContentType,
		`{"path":"x.txt","size":1,"content_type":"text/plain","metadata":{"a":{"nested":"x"}}}`:                      model.EntryErrInvalidMetadata,
		`{"path":"x.txt","size":1,"content_type":"text/plain","metadata":{"bad key!":"x"}}`:                          model.EntryErrInvalidMetadata,
		`{"path":"x.bin","size":104857600,"content_type":"application/octet-stream"}`:                                model.EntryErrChecksumRequired,
		`{"path":"x.txt","size":1,"content_type":"text/plain","metadata":{"a":"1","b":"2","c":"3","d":"4","e":"5"}}`: model.EntryErrInvalidMetadata,
	}
	var manifest strings.Builder
	var want []string
	for line, code := range cases {
		manifest.WriteString(line + "\n")
		want = append(want, code)
	}
	manifest.WriteString(`{"path":"ok.txt","size":1,"content_type":"text/plain","checksum":"sha256:` + sum + `"}` + "\n")

	lines := readAll(t, manifest.String(), testLimits)
	if len(lines) != len(cases)+1 {
		t.Fatalf("every line yields a result: %d", len(lines))
	}
	for i, code := range want {
		e := lines[i].Entry
		if e.State != model.EntryInvalid || e.ErrorCode != code {
			t.Errorf("line %d: got %s %s (%s), want %s", i+1, e.State, e.ErrorCode, e.ErrorDetail, code)
		}
	}
	if last := lines[len(lines)-1].Entry; last.State != model.EntryPending {
		t.Fatalf("a valid line after invalid ones is still accepted: %+v", last)
	}
}

func TestManifestLongLinesAndLineLimit(t *testing.T) {
	limits := testLimits
	limits.MaxLineBytes = 200
	manifest := `{"path":"a.txt","size":1,"content_type":"text/plain","metadata":{"x":"` + strings.Repeat("y", 500) + `"}}` + "\n" +
		`{"path":"b.txt","size":1,"content_type":"text/plain","checksum":"sha256:` + sum + `"}`
	lines := readAll(t, manifest, limits)
	if len(lines) != 2 || lines[0].Entry.ErrorCode != model.EntryErrLineTooLong || lines[1].Entry.State != model.EntryPending || lines[1].Entry.Position != 2 {
		t.Fatalf("a long line is rejected and reading goes on: %+v", lines)
	}

	limits.MaxEntries = 1
	m := NewManifestReader(strings.NewReader("{}\n{}\n"), limits, 0, 0, 0)
	_, _ = m.Next()
	if _, err := m.Next(); !errors.Is(err, ErrTooManyLines) {
		t.Fatalf("expected ErrTooManyLines, got %v", err)
	}
}

func TestManifestResumesFromACheckpoint(t *testing.T) {
	line := func(path string) string {
		return `{"path":"` + path + `","size":1,"content_type":"text/plain","checksum":"sha256:` + sum + `"}` + "\n"
	}
	manifest := line("a.txt") + line("b.txt") + line("c.txt")
	all := readAll(t, manifest, testLimits)

	// From a byte offset (a staged manifest read again with a range).
	from := all[0].End
	m := NewManifestReader(strings.NewReader(manifest[from:]), testLimits, 1, from, 0)
	next, err := m.Next()
	if err != nil || next.Entry.Path != "b.txt" || next.Entry.Position != 2 || next.End != all[1].End {
		t.Fatalf("resume from offset: %+v %v", next, err)
	}

	// From the start, replaying what is stored (a resubmitted manifest).
	m = NewManifestReader(strings.NewReader(manifest), testLimits, 0, 0, 2)
	next, err = m.Next()
	if err != nil || next.Entry.Path != "c.txt" || next.Entry.Position != 3 {
		t.Fatalf("skip replayed lines: %+v %v", next, err)
	}
}

// generated produces n manifest lines on demand, without ever holding the
// manifest in memory.
type generated struct {
	n, i int
	buf  []byte
}

func (g *generated) Read(p []byte) (int, error) {
	for len(g.buf) == 0 {
		if g.i >= g.n {
			return 0, io.EOF
		}
		g.i++
		g.buf = []byte(fmt.Sprintf(`{"path":"set/%07d/file-%d.bin","size":%d,"content_type":"application/octet-stream","checksum":"sha256:%s","metadata":{"n":"%d"}}`+"\n",
			g.i/1000, g.i, 1024+g.i%4096, sum, g.i))
	}
	n := copy(p, g.buf)
	g.buf = g.buf[n:]
	return n, nil
}

func TestLargeManifestIsReadWithBoundedMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("reads a million lines")
	}
	const lines = 1_000_000
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	m := NewManifestReader(&generated{n: lines}, testLimits, 0, 0, 0)
	var peak uint64
	count := 0
	for {
		line, err := m.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if line.Entry.State != model.EntryPending {
			t.Fatalf("line %d: %+v", count+1, line.Entry)
		}
		count++
		if count%100_000 == 0 {
			runtime.GC()
			var now runtime.MemStats
			runtime.ReadMemStats(&now)
			peak = max(peak, now.HeapInuse)
		}
	}
	if count != lines {
		t.Fatalf("read %d of %d lines", count, lines)
	}
	// The manifest is about 100 MB; reading it must not hold more than a
	// few MB at any point.
	growth := int64(peak) - int64(before.HeapInuse)
	t.Logf("read %d lines; heap in use grew by at most %d KiB", count, growth/1024)
	if growth > 8<<20 {
		t.Fatalf("heap grew by %d bytes while streaming", growth)
	}
}
