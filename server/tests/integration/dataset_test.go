package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	datasetservice "github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
)

// Phase 10 against PostgreSQL: managed datasets, membership and
// snapshots, through the dataset service as the API calls it.

// publishObjects publishes n objects under prefix, each with its own
// content, and returns their versions.
func (s *securityStack) publishObjects(t *testing.T, p model.Principal, prefix string, n int) []model.Asset {
	t.Helper()
	out := make([]model.Asset, n)
	for i := range out {
		out[i] = s.publishAt(t, p, fmt.Sprintf("%s/item-%02d.txt", prefix, i), textData(fmt.Sprintf("%s-%d", prefix, i), 40+i))
	}
	return out
}

func (s *securityStack) newDataset(t *testing.T, p model.Principal, name string) model.Dataset {
	t.Helper()
	d, _, err := s.datasets.Create(context.Background(), p, datasetservice.CreateInput{Name: name})
	if err != nil {
		t.Fatalf("create dataset %s: %v", name, err)
	}
	return d
}

// addMembers adds objects by ID and fails the test on any refusal.
func (s *securityStack) addMembers(t *testing.T, p model.Principal, datasetID string, inputs ...datasetservice.MemberInput) datasetservice.AddResult {
	t.Helper()
	res, err := s.datasets.AddMembers(context.Background(), p, datasetID, inputs, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Rejected > 0 {
		t.Fatalf("members refused: %+v", res.Outcomes)
	}
	return res
}

func memberOf(a model.Asset, label, split string) datasetservice.MemberInput {
	return datasetservice.MemberInput{ObjectID: a.ObjectID, Label: label, Split: split}
}

// versionMembers reads a whole version in pages.
func (s *securityStack) versionMembers(t *testing.T, p model.Principal, v model.DatasetVersion) []model.VersionMember {
	t.Helper()
	var all []model.VersionMember
	var after int64
	for {
		page, err := s.datasets.VersionMembers(context.Background(), p, v.DatasetID, v.ID, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page...)
		if len(page) < 2 {
			return all
		}
		after = page[len(page)-1].Ordinal
	}
}

// manifestHash recomputes a version's canonical manifest hash from its
// members, as docs/datasets.md describes it.
func manifestHash(members []model.VersionMember) string {
	h := sha256.New()
	for _, m := range members {
		h.Write(m.CanonicalLine())
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestDatasetOwnershipUpdatesAndTransitions(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "ds-own-a")
	globex := s.team(t, "ds-own-b")
	dev := acme.developer

	d, created, err := s.datasets.Create(ctx, dev, datasetservice.CreateInput{
		Name: "cats", Description: "cat pictures", Tags: []string{"vision"}, Metadata: map[string]string{"owner": "ml", "stage": "raw"},
	})
	if err != nil || !created || d.Lifecycle != model.LifecycleDraft || d.Revision != 1 || d.CreatedBy != dev.UserID || d.TenantID != acme.org.ID {
		t.Fatalf("created %v: %+v %v", created, d, err)
	}
	// The same request again is the same dataset; a different one with the
	// same name is a conflict.
	if again, created, err := s.datasets.Create(ctx, dev, datasetservice.CreateInput{
		Name: "cats", Description: "cat pictures", Tags: []string{"vision"}, Metadata: map[string]string{"owner": "ml", "stage": "raw"},
	}); err != nil || created || again.ID != d.ID {
		t.Fatalf("replay: %v %+v %v", created, again, err)
	}
	if _, _, err := s.datasets.Create(ctx, dev, datasetservice.CreateInput{Name: "cats", Description: "dogs"}); !errors.Is(err, model.ErrDatasetConflict) {
		t.Fatalf("conflicting create: %v", err)
	}
	for _, bad := range []datasetservice.CreateInput{
		{Name: "Bad Name"}, {Name: "x", Tags: []string{"has space"}}, {Name: "y", Metadata: map[string]string{"bad key": "v"}},
		{Name: "z", ProfileID: "builtin:nonexistent"},
	} {
		if _, _, err := s.datasets.Create(ctx, dev, bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}

	// Another tenant sees nothing of it, and can use the same name.
	if _, err := s.datasets.Get(ctx, globex.developer, d.ID); !errors.Is(err, model.ErrDatasetNotFound) {
		t.Fatalf("cross-tenant get: %v", err)
	}
	if page, err := s.datasets.List(ctx, globex.developer, "", "", 0); err != nil || len(page.Items) != 0 {
		t.Fatalf("cross-tenant list: %+v %v", page, err)
	}
	if theirs, created, err := s.datasets.Create(ctx, globex.developer, datasetservice.CreateInput{Name: "cats"}); err != nil || !created || theirs.ID == d.ID {
		t.Fatalf("same name in another tenant: %+v %v", theirs, err)
	}
	// Auditors read, but do not change.
	if _, err := s.datasets.Get(ctx, acme.auditor, d.ID); err != nil {
		t.Fatalf("auditor read: %v", err)
	}
	if _, _, err := s.datasets.Create(ctx, acme.auditor, datasetservice.CreateInput{Name: "audit-set"}); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("auditor create: %v", err)
	}

	// Updates are conditional on the revision.
	stage := "clean"
	if _, err := s.datasets.Update(ctx, dev, d.ID, 0, datasetservice.UpdateInput{Description: &stage}); !errors.Is(err, datasetservice.ErrRevisionRequired) {
		t.Fatalf("no revision: %v", err)
	}
	if _, err := s.datasets.Update(ctx, dev, d.ID, d.Revision+5, datasetservice.UpdateInput{Description: &stage}); !errors.Is(err, model.ErrRevisionConflict) {
		t.Fatalf("stale revision: %v", err)
	}
	updated, err := s.datasets.Update(ctx, dev, d.ID, d.Revision, datasetservice.UpdateInput{
		Metadata: map[string]*string{"stage": &stage, "owner": nil},
	})
	if err != nil || updated.Revision != d.Revision+1 || updated.Metadata["stage"] != "clean" || updated.Metadata["owner"] != "" ||
		updated.Description != "cat pictures" || updated.UpdatedBy != dev.UserID {
		t.Fatalf("updated: %+v %v", updated, err)
	}

	// VALIDATING cannot be archived; cancelling the validation returns it
	// to DRAFT, which can.
	obj := s.publishAt(t, dev, "own/a.txt", textData("own-a", 30))
	s.addMembers(t, dev, d.ID, memberOf(obj, "", ""))
	if _, _, err := s.datasets.Snapshot(ctx, dev, d.ID, ""); err != nil {
		t.Fatal(err)
	}
	job, _, err := s.datasets.StartValidation(ctx, dev, d.ID, datasetservice.ValidationInput{})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.datasets.Get(ctx, dev, d.ID); got.Lifecycle != model.LifecycleValidating {
		t.Fatalf("validating: %s", got.Lifecycle)
	}
	if _, err := s.datasets.Archive(ctx, acme.admin, d.ID); !errors.Is(err, model.ErrDatasetTransition) {
		t.Fatalf("archive while validating: %v", err)
	}
	if cancelled, err := s.datasets.CancelValidation(ctx, dev, job.ID); err != nil || cancelled.State != model.ValidationCancelled {
		t.Fatalf("cancel: %+v %v", cancelled, err)
	}
	if got, _ := s.datasets.Get(ctx, dev, d.ID); got.Lifecycle != model.LifecycleDraft {
		t.Fatalf("after cancel: %s", got.Lifecycle)
	}

	// Archiving needs datasets.manage, and is final.
	if _, err := s.datasets.Archive(ctx, dev, d.ID); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("developer archive: %v", err)
	}
	archived, err := s.datasets.Archive(ctx, acme.admin, d.ID)
	if err != nil || archived.Lifecycle != model.LifecycleArchived || archived.ArchivedAt == nil {
		t.Fatalf("archive: %+v %v", archived, err)
	}
	if _, err := s.datasets.Archive(ctx, acme.admin, d.ID); !errors.Is(err, model.ErrDatasetArchived) {
		t.Fatalf("archive twice: %v", err)
	}
	if _, err := s.datasets.AddMembers(ctx, dev, d.ID, []datasetservice.MemberInput{memberOf(obj, "", "")}, ""); !errors.Is(err, model.ErrDatasetArchived) {
		t.Fatalf("members of an archived dataset: %v", err)
	}
	if _, _, err := s.datasets.Snapshot(ctx, dev, d.ID, ""); !errors.Is(err, model.ErrDatasetArchived) {
		t.Fatalf("snapshot of an archived dataset: %v", err)
	}
	if _, err := s.datasets.Update(ctx, dev, d.ID, archived.Revision, datasetservice.UpdateInput{Description: &stage}); !errors.Is(err, model.ErrDatasetArchived) {
		t.Fatalf("update of an archived dataset: %v", err)
	}
	// Its versions stay readable.
	if versions, err := s.datasets.Versions(ctx, acme.auditor, d.ID, 0, 0); err != nil || len(versions) != 1 {
		t.Fatalf("versions of an archived dataset: %v %v", versions, err)
	}

	// Listing pages by name, and filters by status.
	for _, name := range []string{"set-b", "set-a", "set-c"} {
		s.newDataset(t, dev, name)
	}
	first, err := s.datasets.List(ctx, dev, "draft", "", 2)
	if err != nil || len(first.Items) != 2 || first.Items[0].Name != "set-a" || first.NextCursor == "" {
		t.Fatalf("first page: %+v %v", first, err)
	}
	second, err := s.datasets.List(ctx, dev, "DRAFT", first.NextCursor, 2)
	if err != nil || len(second.Items) != 1 || second.Items[0].Name != "set-c" || second.NextCursor != "" {
		t.Fatalf("second page: %+v %v", second, err)
	}
	if page, _ := s.datasets.List(ctx, dev, "ARCHIVED", "", 0); len(page.Items) != 1 || page.Items[0].ID != d.ID {
		t.Fatalf("archived: %+v", page.Items)
	}
	if _, err := s.datasets.List(ctx, dev, "SHINY", "", 0); !errors.Is(err, datasetservice.ErrInvalidRequest) {
		t.Fatalf("bad status: %v", err)
	}
}

func TestDatasetMembershipAndDuplicates(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "ds-mem-a")
	globex := s.team(t, "ds-mem-b")
	dev := acme.developer
	objs := s.publishObjects(t, dev, "mem", 3)
	foreign := s.publishAt(t, globex.developer, "mem/item-00.txt", textData("theirs", 40))
	d := s.newDataset(t, dev, "members")

	res, err := s.datasets.AddMembers(ctx, dev, d.ID, []datasetservice.MemberInput{
		{ObjectID: objs[0].ObjectID, Label: "cat", Split: "train"},
		{Path: "MEM/Item-01.TXT"},                      // by path, folded like the catalog
		{ObjectID: foreign.ObjectID},                   // another tenant's object
		{ObjectID: objs[0].ObjectID, Label: "other"},   // again in the same request
		{ObjectID: objs[2].ObjectID, Version: 1},       // pinned by number
		{ObjectID: "no-such-object"},                   // unknown
		{Path: "../mem/item-02.txt"},                   // unsafe path
		{ObjectID: objs[1].ObjectID, Version: 9},       // no such version
		{ObjectID: objs[2].ObjectID, Split: "holdout"}, // bad split
	}, model.DuplicateReject)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ outcome, code string }{
		{"ADDED", ""}, {"ADDED", ""}, {"REJECTED", model.LineObjectNotFound}, {"REJECTED", model.LineDuplicateInFile},
		{"ADDED", ""}, {"REJECTED", model.LineObjectNotFound}, {"REJECTED", model.LineInvalidPath},
		{"REJECTED", model.LineVersionNotFound}, {"REJECTED", model.LineInvalidSplit},
	}
	for i, w := range want {
		if o := res.Outcomes[i]; o.Outcome != w.outcome || o.Code != w.code {
			t.Errorf("input %d: %+v, want %s %s", i, o, w.outcome, w.code)
		}
	}
	if res.Added != 3 || res.Rejected != 6 || res.Dataset.MemberCount != 3 || res.Outcomes[4].AssetID != objs[2].ID {
		t.Fatalf("result: %+v", res)
	}
	// A refused reference names nothing of the other tenant's.
	if res.Outcomes[2].ObjectID != "" {
		t.Fatalf("a foreign object was resolved: %+v", res.Outcomes[2])
	}

	again := []datasetservice.MemberInput{{ObjectID: objs[0].ObjectID, Label: "dog", Split: "test"}}
	for _, c := range []struct {
		policy  model.DuplicatePolicy
		outcome string
	}{{model.DuplicateReject, "REJECTED"}, {model.DuplicateSkip, "SKIPPED"}, {model.DuplicateReplace, "UPDATED"}, {model.DuplicateReplace, "SKIPPED"}} {
		res, err := s.datasets.AddMembers(ctx, dev, d.ID, again, c.policy)
		if err != nil || res.Outcomes[0].Outcome != c.outcome || res.Dataset.MemberCount != 3 {
			t.Fatalf("%s: %+v %v", c.policy, res, err)
		}
	}

	// Pages of the draft, with each object's place in the catalog.
	page, err := s.datasets.Members(ctx, acme.auditor, d.ID, "", 2)
	if err != nil || len(page.Items) != 2 || page.NextCursor == "" {
		t.Fatalf("first page: %+v %v", page, err)
	}
	rest, err := s.datasets.Members(ctx, dev, d.ID, page.NextCursor, 2)
	if err != nil || len(rest.Items) != 1 || rest.NextCursor != "" {
		t.Fatalf("second page: %+v %v", rest, err)
	}
	all := append(page.Items, rest.Items...)
	var replaced model.DatasetMember
	for _, m := range all {
		if m.ObjectID == objs[0].ObjectID {
			replaced = m
		}
		if m.Path == "" || m.Source != "api" || m.AddedBy != dev.UserID {
			t.Fatalf("member: %+v", m)
		}
	}
	if replaced.Label != "dog" || replaced.Split != "test" {
		t.Fatalf("replaced member: %+v", replaced)
	}

	// Removal, and its limits.
	after, err := s.datasets.RemoveMember(ctx, dev, d.ID, replaced.ID)
	if err != nil || after.MemberCount != 2 {
		t.Fatalf("remove: %+v %v", after, err)
	}
	if _, err := s.datasets.RemoveMember(ctx, dev, d.ID, replaced.ID); !errors.Is(err, model.ErrMemberNotFound) {
		t.Fatalf("remove twice: %v", err)
	}
	if _, err := s.datasets.RemoveMember(ctx, globex.developer, d.ID, all[0].ID); !errors.Is(err, model.ErrDatasetNotFound) {
		t.Fatalf("cross-tenant remove: %v", err)
	}
	if _, err := s.datasets.AddMembers(ctx, globex.developer, d.ID, []datasetservice.MemberInput{{ObjectID: foreign.ObjectID}}, ""); !errors.Is(err, model.ErrDatasetNotFound) {
		t.Fatalf("cross-tenant add: %v", err)
	}
	if _, err := s.datasets.AddMembers(ctx, acme.auditor, d.ID, again, ""); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("auditor add: %v", err)
	}
	if _, err := s.datasets.AddMembers(ctx, dev, d.ID, nil, ""); !errors.Is(err, datasetservice.ErrInvalidRequest) {
		t.Fatalf("empty add: %v", err)
	}
}

func TestSnapshotsAreImmutableDeterministicAndIdempotent(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "ds-snap")
	dev := acme.developer
	objs := s.publishObjects(t, dev, "snap", 3)
	d := s.newDataset(t, dev, "snapshots")

	if _, _, err := s.datasets.Snapshot(ctx, dev, d.ID, ""); !errors.Is(err, model.ErrNoMembers) {
		t.Fatalf("empty snapshot: %v", err)
	}
	s.addMembers(t, dev, d.ID, memberOf(objs[0], "a", "train"),
		datasetservice.MemberInput{ObjectID: objs[1].ObjectID, Version: 1, Label: "b", Split: "test"})

	v1, created, err := s.datasets.Snapshot(ctx, dev, d.ID, "snap-1")
	if err != nil || !created || v1.Version != 1 || v1.MemberCount != 2 || v1.Status != "FINALIZED" ||
		v1.ManifestFormat != model.ManifestFormatV1 || v1.ValidationStatus != model.VersionNotValidated {
		t.Fatalf("v1: %+v %v", v1, err)
	}
	// The same key returns the same version.
	if again, created, err := s.datasets.Snapshot(ctx, dev, d.ID, "snap-1"); err != nil || created || again.ID != v1.ID {
		t.Fatalf("replay: %+v %v %v", again, created, err)
	}
	members := s.versionMembers(t, dev, v1)
	if len(members) != 2 || manifestHash(members) != v1.ManifestSHA256 || v1.TotalBytes != members[0].SizeBytes+members[1].SizeBytes {
		t.Fatalf("v1 members: %+v; hash %s", members, v1.ManifestSHA256)
	}
	if members[0].ObjectID > members[1].ObjectID || members[0].Ordinal != 1 {
		t.Fatal("members are in canonical order (object ID)")
	}

	// A new version of an unpinned object changes the draft's next
	// snapshot, never the old one.
	newer := s.publishAt(t, dev, "snap/item-00.txt", textData("snap-0-v2", 77))
	if newer.ObjectID != objs[0].ObjectID || newer.Version != 2 {
		t.Fatalf("second version: %+v", newer)
	}
	s.addMembers(t, dev, d.ID, memberOf(objs[2], "c", "validation"))
	v2, created, err := s.datasets.Snapshot(ctx, dev, d.ID, "")
	if err != nil || !created || v2.Version != 2 || v2.MemberCount != 3 || v2.ParentVersionID != v1.ID {
		t.Fatalf("v2: %+v %v", v2, err)
	}
	if again := s.versionMembers(t, dev, v1); manifestHash(again) != v1.ManifestSHA256 || again[0].AssetID != members[0].AssetID {
		t.Fatal("v1 changed after later changes")
	}
	byObject := map[string]model.VersionMember{}
	for _, m := range s.versionMembers(t, dev, v2) {
		byObject[m.ObjectID] = m
	}
	if byObject[objs[0].ObjectID].AssetID != newer.ID || byObject[objs[0].ObjectID].ObjectVersion != 2 ||
		byObject[objs[1].ObjectID].AssetID != objs[1].ID || byObject[objs[1].ObjectID].ObjectVersion != 1 {
		t.Fatalf("v2 pins: %+v", byObject)
	}
	current, _ := s.datasets.Get(ctx, dev, d.ID)
	if current.CurrentVersionID != v2.ID || current.NextVersion != 3 {
		t.Fatalf("dataset after v2: %+v", current)
	}

	// The same members give the same hash in another dataset.
	twin := s.newDataset(t, dev, "snapshots-twin")
	s.addMembers(t, dev, twin.ID, memberOf(objs[0], "a", "train"),
		datasetservice.MemberInput{ObjectID: objs[1].ObjectID, Version: 1, Label: "b", Split: "test"}, memberOf(objs[2], "c", "validation"))
	tv, _, err := s.datasets.Snapshot(ctx, dev, twin.ID, "")
	if err != nil || tv.ManifestSHA256 != v2.ManifestSHA256 {
		t.Fatalf("twin hash %s, v2 %s (%v)", tv.ManifestSHA256, v2.ManifestSHA256, err)
	}

	// A member whose object has no published version cannot be fixed.
	pending := s.startUpload(t, dev, catalogUpload("snap/pending.txt", textData("pending", 20)), textData("pending", 20))
	s.addMembers(t, dev, d.ID, datasetservice.MemberInput{ObjectID: pending.ObjectID})
	if _, _, err := s.datasets.Snapshot(ctx, dev, d.ID, ""); !errors.Is(err, model.ErrUnresolvedMembers) {
		t.Fatalf("unresolved: %v", err)
	}
	// Versions page newest first.
	versions, err := s.datasets.Versions(ctx, dev, d.ID, 0, 1)
	if err != nil || len(versions) != 1 || versions[0].ID != v2.ID {
		t.Fatalf("newest: %v %v", versions, err)
	}
	if older, _ := s.datasets.Versions(ctx, dev, d.ID, versions[0].Version, 5); len(older) != 1 || older[0].ID != v1.ID {
		t.Fatalf("older: %v", older)
	}
	if _, err := s.datasets.Version(ctx, dev, twin.ID, v1.ID); !errors.Is(err, model.ErrDatasetVersionNotFound) {
		t.Fatalf("a version of another dataset: %v", err)
	}
}

func TestConcurrentMembershipChangesDuringSnapshots(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "ds-race")
	dev := acme.developer
	objs := s.publishObjects(t, dev, "race", 12)
	d := s.newDataset(t, dev, "race")
	s.addMembers(t, dev, d.ID, memberOf(objs[0], "", ""), memberOf(objs[1], "", ""))

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for _, o := range objs[2:] {
			if _, err := s.datasets.AddMembers(ctx, dev, d.ID, []datasetservice.MemberInput{memberOf(o, "", "")}, ""); err != nil {
				errs <- err
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range 5 {
			if _, _, err := s.datasets.Snapshot(ctx, dev, d.ID, ""); err != nil {
				errs <- err
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	// Every version is whole: its rows match its count and hash, and each
	// holds every member of the one before (only additions happened).
	versions, err := s.datasets.Versions(ctx, dev, d.ID, 0, 10)
	if err != nil || len(versions) != 5 {
		t.Fatalf("versions: %v %v", versions, err)
	}
	slices.Reverse(versions)
	var previous []string
	for _, v := range versions {
		members := s.versionMembers(t, dev, v)
		if int64(len(members)) != v.MemberCount || manifestHash(members) != v.ManifestSHA256 {
			t.Fatalf("version %d: %d rows, count %d", v.Version, len(members), v.MemberCount)
		}
		var ids []string
		for _, m := range members {
			ids = append(ids, m.ObjectID)
		}
		for _, id := range previous {
			if !slices.Contains(ids, id) {
				t.Fatalf("version %d lost %s", v.Version, id)
			}
		}
		previous = ids
	}
	if final, _ := s.datasets.Get(ctx, dev, d.ID); final.MemberCount != 12 {
		t.Fatalf("members: %d", final.MemberCount)
	}
}
