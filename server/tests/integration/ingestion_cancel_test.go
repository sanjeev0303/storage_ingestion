package integration

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
)

// Cancelling a large job runs at a steady rate: the sweep pages past the
// entries it already cancelled instead of reading them again. A published
// entry stays published, and an upload stored before the cancel still
// publishes and then lets the job end.
//
// INGEST_CANCEL_ENTRIES sets the job size (default 20,000); the R5
// measurement in docs/load-testing.md used 1,000,000.
func TestIngestionCancellingALargeJobKeepsItsRate(t *testing.T) {
	if testing.Short() {
		t.Skip("stores a large job")
	}
	lines := 20_000
	if v := os.Getenv("INGEST_CANCEL_ENTRIES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1000 {
			t.Fatalf("INGEST_CANCEL_ENTRIES=%q: want a number of at least 1000", v)
		}
		lines = n
	}
	// One batch per pass, so the rate is sampled batch by batch.
	e := openIngestEnv(t, func(c *config.Ingestion) {
		c.InsertBatch = 1000
		c.MaxInlineManifestBytes = 1 << 30
		c.MaxManifestBytes = 1 << 30
		c.PollInterval = time.Nanosecond
	})
	ctx := context.Background()
	d := e.dataset(t, "cancel-large", model.CompleteAll)
	job := e.newJob(t, d.ID)
	started := time.Now()
	report, err := e.svc.SubmitInlineManifest(ctx, e.p, job.ID, &generatedManifest{n: lines}, "")
	if err != nil || report.Accepted != lines {
		t.Fatalf("submission: %v %+v", err, report)
	}
	if _, err := e.svc.Finalize(ctx, e.p, job.ID); err != nil {
		t.Fatal(err)
	}
	t.Logf("stored %d entries in %s", lines, time.Since(started).Round(time.Millisecond))

	// Entry 1 is published, entry 2 stored and waiting for processing,
	// entries 3 and 4 claimed with nothing sent.
	grants := e.claim(t, job.ID, 4).Grants
	if len(grants) != 4 {
		t.Fatalf("grants: %d", len(grants))
	}
	if note := e.process(t, e.upload(t, grants[0], generatedFile(1).data)); note != "published" {
		t.Fatalf("entry 1: %s", note)
	}
	stored := e.upload(t, grants[1], generatedFile(2).data)

	if _, err := e.svc.Cancel(ctx, e.p, job.ID); err != nil {
		t.Fatal(err)
	}
	waiting := int64(lines - 4)
	tenth := waiting / 10
	var cancelled, mark int64
	var rates []float64
	started = time.Now()
	last := started
	for cancelled < waiting+2 {
		n, err := e.svc.AdvanceCancellations(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Fatalf("a pass cancelled nothing with %d of %d cancelled", cancelled, waiting+2)
		}
		cancelled += int64(n)
		if cancelled-mark >= tenth {
			rates = append(rates, float64(cancelled-mark)/time.Since(last).Seconds())
			mark, last = cancelled, time.Now()
		}
	}
	elapsed := time.Since(started)
	t.Logf("cancelled %d entries in %s (%.0f/s); per tenth: %.0f", cancelled, elapsed.Round(time.Millisecond),
		float64(cancelled)/elapsed.Seconds(), rates)
	// A sweep that re-reads what it cancelled slows down as it goes; R5
	// fell to a quarter of its starting rate.
	if len(rates) >= 4 && rates[len(rates)-1] < rates[0]/3 {
		t.Errorf("the rate fell from %.0f/s to %.0f/s", rates[0], rates[len(rates)-1])
	}

	if j := e.job(t, job.ID); j.State != model.JobCancelling {
		t.Fatalf("job with a stored upload left: %s", j.State)
	}
	if note := e.process(t, stored); note != "published" {
		t.Fatalf("stored upload: %s", note)
	}
	j := e.job(t, job.ID)
	if j.State != model.JobCancelled || j.Counters.Published != 2 || j.Counters.Cancelled != int64(lines-2) || j.Counters.Open() != 0 {
		t.Fatalf("job: %s %+v", j.State, j.Counters)
	}
	if entry := e.entry(t, job.ID, 1); entry.State != model.EntryPublished {
		t.Fatalf("published entry: %s", entry.State)
	}
	if stored, actual := e.recount(t, job.ID); stored != actual {
		t.Fatalf("counters %+v, entries %+v", stored, actual)
	}
}

// generatedFile is line i of a generatedManifest.
func generatedFile(i int) file {
	return textFile(fmt.Sprintf("gen/%03d/%07d.txt", i/1000, i), 64+i%512)
}
