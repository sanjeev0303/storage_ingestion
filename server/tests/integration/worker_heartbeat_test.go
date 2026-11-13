package integration

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
)

// Worker heartbeats (migration 000050): a beat is upserted per process,
// the status read returns the last day's, and pruning removes old ones.
func TestWorkerHeartbeats(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := postgres.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	defer db.Close()

	kind := "test-" + time.Now().Format("150405.000000")
	now := time.Now().UTC().Truncate(time.Microsecond)
	started := now.Add(-time.Minute)
	if err := db.BeatWorker(ctx, kind, "host-1", started, now.Add(-10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := db.BeatWorker(ctx, kind, "host-1", now, now); err != nil {
		t.Fatal(err)
	}
	if err := db.BeatWorker(ctx, kind, "host-2", started, now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM worker_heartbeats WHERE kind = $1`, kind)
	})

	status, err := db.OperationsStatus(ctx, "no-such-tenant", now)
	if err != nil {
		t.Fatal(err)
	}
	var mine []string
	for _, w := range status.Workers {
		if w.Kind != kind {
			continue
		}
		mine = append(mine, w.Instance)
		// A second beat moves last_seen_at and keeps the first start.
		if !w.LastSeenAt.Equal(now) || !w.StartedAt.Equal(started) {
			t.Fatalf("heartbeat: %+v", w)
		}
	}
	if len(mine) != 1 || mine[0] != "host-1" {
		t.Fatalf("a day-old process is not reported: %v", mine)
	}

	if err := db.PruneWorkerHeartbeats(ctx, now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM worker_heartbeats WHERE kind = $1`, kind).Scan(&left); err != nil || left != 1 {
		t.Fatalf("after pruning: %d %v", left, err)
	}
}
