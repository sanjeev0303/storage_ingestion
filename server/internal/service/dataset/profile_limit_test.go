package dataset

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/platform/csvlimit"
)

// A CSV record with no end is refused instead of being held in memory.
func TestProfileRefusesAnOverlongCSVRecord(t *testing.T) {
	body := bytes.Repeat([]byte("a"), csvRecordMax+1)
	err := profileStream(context.Background(), bytes.NewReader(body), formatCSV, false, newTable("t", "csv", "v", 1000), t.TempDir())
	if !errors.Is(err, csvlimit.ErrRecordTooLong) {
		t.Fatalf("err = %v, want ErrRecordTooLong", err)
	}
}
