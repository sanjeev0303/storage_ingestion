package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
)

// A full pack ends between samples (files sharing a path stem), so each
// sample is packed whole; an organization's own target pack size wins
// over the platform's.
func TestPackingKeepsSamplesTogetherAndUsesTheTenantTarget(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "pack-samples")
	dev := acme.developer
	sizes := []struct {
		path string
		n    int
	}{{"ds/s1.cls.txt", 40}, {"ds/s1.txt", 40}, {"ds/s2.cls.txt", 40}, {"ds/s2.txt", 90}, {"ds/s3.cls.txt", 40}, {"ds/s3.txt", 40}}
	data := map[string][]byte{}
	for _, f := range sizes {
		b := textData(f.path, f.n)
		a := s.publishAt(t, dev, f.path, b)
		data[a.ID] = b
	}
	rig := s.packingRig(t)
	w := rig.worker(t, "w-samples", nil, []string{acme.org.ID}, func(c *config.Packing) { c.TargetPackBytes, c.MaxObjectBytes = 150, 100 })
	for i := 0; i < 3; i++ {
		if stats := rig.run(t, w); stats.Published != 1 {
			t.Fatalf("cycle %d publishes one pack: %+v", i, stats)
		}
	}
	packs := rig.packs(t, acme.org.ID)
	if len(packs) != 3 {
		t.Fatalf("one pack per sample: %+v", packs)
	}
	assets := postgres.NewAssetRepository(s.db)
	for _, p := range packs {
		stems := map[string]bool{}
		for _, e := range rig.entries(t, p.ID) {
			a, err := assets.GetByID(ctx, acme.org.ID, e.AssetID)
			if err != nil {
				t.Fatal(err)
			}
			stems[strings.SplitN(a.FileName, ".", 2)[0]] = true
		}
		if len(stems) != 1 || p.EntryCount != 2 {
			t.Fatalf("pack %s holds one whole sample: %v", p.ID, stems)
		}
	}
	for id, b := range data {
		s.mustDownload(t, dev, id, b)
	}

	// With its own 1 MiB target, the organization's next files go in one
	// pack, though the platform's target is 150 bytes.
	if err := rig.repo.SetTargetPackBytes(ctx, acme.org.ID, 1<<20, "it", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		s.publishAt(t, dev, fmt.Sprintf("big/f%d.txt", i), textData(fmt.Sprintf("big-%d", i), 60))
	}
	if stats := rig.run(t, w); stats.Published != 1 {
		t.Fatalf("one pack: %+v", stats)
	}
	packs = rig.packs(t, acme.org.ID)
	if last := packs[len(packs)-1]; last.EntryCount != 6 {
		t.Fatalf("the tenant's target holds all six: %+v", last)
	}
}

// A repack gathers a dataset's entries from several packs into one, in
// path order; the old packs retire, every file still reads back exactly,
// and repacking again changes nothing.
func TestRepackGathersADatasetsEntriesInPathOrder(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "repack")
	dev := acme.developer
	files := s.publishObjects(t, dev, "rp", 6) // rp/item-00 .. item-05, 40 to 45 bytes
	rig := s.packingRig(t)
	tenants := []string{acme.org.ID}
	small := rig.worker(t, "w-small", nil, tenants, func(c *config.Packing) { c.TargetPackBytes, c.MaxObjectBytes = 90, 50 })
	for i := 0; i < 3; i++ {
		rig.run(t, small)
	}
	old := rig.packs(t, acme.org.ID)
	if len(old) != 3 {
		t.Fatalf("three packs of two: %+v", old)
	}

	d := s.newDataset(t, dev, "repack-set")
	for _, a := range files {
		s.addMembers(t, dev, d.ID, memberOf(a, "", ""))
	}
	if err := rig.repo.CreateRepack(ctx, model.RepackRequest{ID: "rq-" + d.ID, TenantID: acme.org.ID, DatasetID: d.ID,
		RequestedBy: "it", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := rig.repo.CreateRepack(ctx, model.RepackRequest{ID: "rq2-" + d.ID, TenantID: acme.org.ID, DatasetID: d.ID,
		RequestedBy: "it", CreatedAt: time.Now().UTC()}); err != model.ErrRepackActive {
		t.Fatalf("one repack at a time: %v", err)
	}

	w := rig.worker(t, "w-repack", nil, tenants, nil)
	if stats := rig.run(t, w); stats.Repacked != 1 || stats.Published != 1 {
		t.Fatalf("one repack pack built: %+v", stats)
	}
	packs := rig.packs(t, acme.org.ID)
	repacked := packs[len(packs)-1]
	entries := rig.entries(t, repacked.ID)
	if len(entries) != 6 {
		t.Fatalf("all six in one pack: %+v", entries)
	}
	for i, e := range entries {
		if e.AssetID != files[i].ID || e.State != model.PackEntryPublished || e.SourcePackID == "" {
			t.Fatalf("entry %d in path order, copied from a pack: %+v", i, e)
		}
	}
	for _, p := range old {
		now, _ := rig.repo.GetPack(ctx, acme.org.ID, p.ID)
		if now.State != model.PackRetiring && now.State != model.PackDeleted {
			t.Fatalf("old pack %s retires: %s", p.ID, now.State)
		}
	}
	for i, a := range files {
		s.mustDownload(t, dev, a.ID, textData(fmt.Sprintf("rp-%d", i), 40+i))
	}
	reqs, err := rig.repo.Repacks(ctx, acme.org.ID, d.ID, 5)
	if err != nil || len(reqs) != 1 || reqs[0].State != "DONE" || reqs[0].PacksPlanned != 1 || reqs[0].EntriesPlanned != 6 {
		t.Fatalf("the repack is done: %+v %v", reqs, err)
	}

	if err := rig.repo.CreateRepack(ctx, model.RepackRequest{ID: "rq3-" + d.ID, TenantID: acme.org.ID, DatasetID: d.ID,
		RequestedBy: "it", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if stats := rig.run(t, w); stats.Repacked != 0 || stats.Planned != 0 {
		t.Fatalf("a pack already in order is left alone: %+v", stats)
	}
	if reqs, _ := rig.repo.Repacks(ctx, acme.org.ID, d.ID, 5); reqs[0].State != "DONE" || reqs[0].PacksPlanned != 0 {
		t.Fatalf("the second repack finds nothing to do: %+v", reqs[0])
	}
}
