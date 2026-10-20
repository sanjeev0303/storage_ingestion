package jsonl

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReaderLinesOffsetsAndLimits(t *testing.T) {
	input := "{\"a\":1}\n\n" + strings.Repeat("x", 100) + "\n{\"b\":2}"
	r := NewReader(strings.NewReader(input), 20, 0, 0)

	want := []struct {
		text    string
		tooLong bool
	}{{"{\"a\":1}\n", false}, {"\n", false}, {"", true}, {"{\"b\":2}", false}}
	for i, w := range want {
		line, tooLong, err := r.Next()
		if err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		if string(line) != w.text || tooLong != w.tooLong || r.Line() != int64(i+1) {
			t.Fatalf("line %d: %q tooLong=%v number=%d", i+1, line, tooLong, r.Line())
		}
	}
	if _, _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("after the last line: %v", err)
	}
	if r.Offset() != int64(len(input)) {
		t.Fatalf("offset %d, want %d", r.Offset(), len(input))
	}

	// Resuming from a checkpoint numbers lines and offsets from it.
	resumed := NewReader(strings.NewReader(input[9:]), 200, 2, 9)
	if _, _, err := resumed.Next(); err != nil || resumed.Line() != 3 || resumed.Offset() != 110 {
		t.Fatalf("resumed: line %d offset %d err %v", resumed.Line(), resumed.Offset(), err)
	}
}
