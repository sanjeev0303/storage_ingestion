package adoption

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/csvlimit"
)

// stateStore records state changes; read needs nothing else before it
// opens the source.
type stateStore struct{ Store }

func (stateStore) SetState(context.Context, string, string, string, string, time.Time) error {
	return nil
}

// The platform's bucket holds every organization's objects: a URL list on
// the platform connection must be sent inline, and the worker refuses to
// read a source key from it whatever the job says.
func TestPlatformSourcesAreNeverReadFromTheBucket(t *testing.T) {
	s := &Service{deps: Dependencies{Store: stateStore{}}, now: time.Now}
	member := model.Principal{TenantID: "t1", UserID: "u1", Permissions: model.PermIngestionWrite}

	_, err := s.CreateFor(context.Background(), member, CreateInput{Source: model.AdoptURLList, SourceRef: "assets/t2/a1"})
	if !errors.Is(err, model.ErrInvalidAdoption) {
		t.Fatalf("create: err = %v, want ErrInvalidAdoption", err)
	}

	job := model.AdoptionJob{ID: "j1", TenantID: "t1", ConnectionID: model.PlatformConnectionID,
		Source: model.AdoptURLList, SourceRef: "assets/t2/a1"}
	if err := s.read(context.Background(), job); err == nil || !strings.Contains(err.Error(), "platform") {
		t.Fatalf("read: err = %v, want a refusal", err)
	}
}

// An inventory record with no end is refused instead of being held in memory.
func TestInventoryCSVRefusesAnOverlongRecord(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(bytes.Repeat([]byte("a"), maxSourceLine+1))
	zw.Close()
	err := inventoryCSV(&gz, "Bucket, Key, Size", func(inventoryRow) error { return nil })
	if !errors.Is(err, csvlimit.ErrRecordTooLong) {
		t.Fatalf("err = %v, want ErrRecordTooLong", err)
	}
}
