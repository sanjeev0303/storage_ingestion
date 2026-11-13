package processing

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Streams that read to the end of the file and fail to decode still cost
// their reads, and the check stops when its job is cancelled.
func TestInspectPDFStopsWhenCancelled(t *testing.T) {
	body := "%PDF-1.4\n" + strings.Repeat("<</Filter/A85>>stream\n", 200) + strings.Repeat("x", 2<<20)
	path := filepath.Join(t.TempDir(), "streams.pdf")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = inspectPDF(ctx, f, int64(len(body)), &inspection{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("took %s after a 50ms deadline", took)
	}
}
