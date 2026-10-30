package storageconn

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// healthStore keeps posture reports in memory; the credential methods
// are the database's and are tested against it.
type healthStore struct {
	HealthStore
	reports map[string]model.PostureReport
	class   string
}

func (h *healthStore) Posture(_ context.Context, _, id string) (model.PostureReport, bool, error) {
	r, ok := h.reports[id]
	return r, ok, nil
}
func (h *healthStore) PutPosture(_ context.Context, _ string, r model.PostureReport) error {
	h.reports[r.ConnectionID] = r
	return nil
}
func (h *healthStore) DatasetRules(context.Context, string, string) (model.PlacementRules, error) {
	return model.PlacementRules{DataClass: h.class}, nil
}

type alerts struct{ got []model.SecurityAlert }

func (a *alerts) AddAlert(_ context.Context, al model.SecurityAlert) (bool, error) {
	for _, x := range a.got {
		if x.DedupKey == al.DedupKey {
			return false, nil
		}
	}
	a.got = append(a.got, al)
	return true, nil
}

// inspected is a provider whose bucket settings the test chooses.
type inspected struct {
	*memory.ObjectStorage
	facts    model.PostureFacts
	accepted bool
}

func (i *inspected) Posture(context.Context, string, model.PostureOptions) (model.PostureFacts, error) {
	return i.facts, nil
}
func (i *inspected) ProbeSSEC(context.Context, string, string) (bool, error) { return i.accepted, nil }

func safeFacts(lifecycle string) model.PostureFacts {
	f := model.PostureFacts{LifecycleHash: lifecycle, Encrypted: true}
	for _, name := range []string{model.CheckPublicAccessBlock, model.CheckPublicPolicy, model.CheckPublicACL,
		model.CheckEncryption, model.CheckVersioning} {
		f.Checks = append(f.Checks, model.PostureCheck{Name: name, Status: model.PosturePass})
	}
	f.Checks = append(f.Checks, model.PostureCheck{Name: model.CheckSSECDenied, Status: model.PostureNotChecked})
	return f
}

func TestPostureSummaryAlertsAndPlacement(t *testing.T) {
	f := newFixture(t)
	store, raised := &healthStore{reports: map[string]model.PostureReport{}}, &alerts{}
	f.svc.deps.Health, f.svc.deps.Alerts = store, raised
	ctx := context.Background()
	created, err := f.svc.Create(ctx, admin, minioInput())
	if err != nil {
		t.Fatal(err)
	}
	other := minioInput()
	other.Name, other.Bucket = "never checked", "other-bucket"
	unchecked, err := f.svc.Create(ctx, admin, other)
	if err != nil {
		t.Fatal(err)
	}
	c := created
	c.Provider, c.Bucket, c.StagingPrefix, c.ReadyPrefix = model.ProviderAWSS3, "lab-7f3k29xq1b", "staging", "ready"
	provider := &inspected{ObjectStorage: memory.NewObjectStorage(), facts: safeFacts("h1")}

	// Without the SSE-C probe the connection is not yet safe for PHI.
	r, err := f.svc.posture(ctx, c, provider, false, nil)
	if err != nil || r.PHISafe || r.Worst != model.PostureWarn {
		t.Fatalf("no probe: %+v %v", r, err)
	}
	if err := f.svc.CheckPlacement(ctx, admin, model.PlacementRules{DataClass: model.DataPHI}, c.ID); !errors.Is(err, model.ErrPlacementRefused) ||
		!strings.Contains(err.Error(), "sse_c_denied") {
		t.Fatalf("PHI before the probe: %v", err)
	}
	// The probe is refused: safe for PHI; anything but PHI never asks.
	r, _ = f.svc.posture(ctx, c, provider, true, nil)
	if !r.PHISafe {
		t.Fatalf("after a refused probe: %+v", r.Checks)
	}
	if err := f.svc.CheckPlacement(ctx, admin, model.PlacementRules{DataClass: model.DataPHI}, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.CheckPlacement(ctx, admin, model.PlacementRules{DataClass: model.DataPHI}, unchecked.ID); !errors.Is(err, model.ErrPlacementRefused) {
		t.Fatalf("an unchecked connection: %v", err)
	}
	if err := f.svc.CheckPlacement(ctx, admin, model.PlacementRules{DataClass: model.DataPCI}, unchecked.ID); err != nil {
		t.Fatalf("PCI is not limited by posture: %v", err)
	}
	store.class = model.DataPHI
	if err := f.svc.CheckDatasetPlacement(ctx, admin, "scans", unchecked.ID); !errors.Is(err, model.ErrPlacementRefused) {
		t.Fatalf("dataset placement: %v", err)
	}
	// Every decision under rules is audited, refusals as denied.
	denied := 0
	for _, e := range f.audit.events {
		if e.Action == "placement.check" && e.Outcome == model.AuditDenied {
			denied++
		}
	}
	if denied < 3 {
		t.Fatalf("placement checks audited: %d denied", denied)
	}
	if len(raised.got) != 0 {
		t.Fatalf("alerts on a safe connection: %+v", raised.got)
	}

	// The bucket accepts SSE-C and its lifecycle rules change: two alerts.
	provider.accepted, provider.facts = true, safeFacts("h2")
	r, _ = f.svc.posture(ctx, c, provider, true, nil)
	if r.Worst != model.PostureFail || r.PHISafe {
		t.Fatalf("SSE-C accepted: %+v", r)
	}
	if d, _ := r.Check(model.CheckLifecycleDrift); d.Status != model.PostureWarn {
		t.Fatalf("lifecycle drift: %+v", d)
	}
	kinds := map[string]bool{}
	for _, a := range raised.got {
		kinds[a.Kind] = true
	}
	if !kinds[model.AlertPosture] || !kinds[model.AlertLifecycleChanged] || len(raised.got) != 2 {
		t.Fatalf("alerts: %+v", raised.got)
	}
	// The same state again raises nothing new.
	f.svc.posture(ctx, c, provider, true, nil)
	if len(raised.got) != 2 {
		t.Fatalf("repeated alerts: %+v", raised.got)
	}
}

func TestGuessableNames(t *testing.T) {
	for name, want := range map[string]string{
		"acme-backups":               model.PostureWarn,
		"osp-test-byos-758274585063": model.PosturePass,
		"data-7f3k29xq":              model.PosturePass,
		"logs-2024":                  model.PostureWarn,
	} {
		got := guessable(model.StorageConnection{Provider: model.ProviderAWSS3, Bucket: name})
		if got.Status != want {
			t.Errorf("%s: %s", name, got.Status)
		}
	}
	if guessable(model.StorageConnection{Provider: model.ProviderR2, Bucket: "backups"}).Status != model.PosturePass {
		t.Error("R2 names are per account")
	}
}

// drifting answers HEADs the way the test sets up.
type drifting struct {
	*memory.ObjectStorage
	pinned, latest map[string]interfaces.ObjectMetadata
	errs           map[string]error
}

func (d *drifting) HeadObjectVersion(_ context.Context, _, key, _ string) (interfaces.ObjectMetadata, error) {
	if err := d.errs[key]; err != nil {
		return interfaces.ObjectMetadata{}, err
	}
	m, ok := d.pinned[key]
	if !ok {
		return m, interfaces.ErrObjectNotFound
	}
	return m, nil
}

func (d *drifting) HeadObject(_ context.Context, _, key string) (interfaces.ObjectMetadata, error) {
	if err := d.errs[key]; err != nil {
		return interfaces.ObjectMetadata{}, err
	}
	m, ok := d.latest[key]
	if !ok {
		return m, interfaces.ErrObjectNotFound
	}
	return m, nil
}

func TestExamineFindsOutOfBandChanges(t *testing.T) {
	d := &drifting{ObjectStorage: memory.NewObjectStorage(),
		pinned: map[string]interfaces.ObjectMetadata{"v-ok": {VersionID: "1"}, "v-new": {VersionID: "1"}, "v-marker": {VersionID: "1"}},
		latest: map[string]interfaces.ObjectMetadata{
			"v-ok": {VersionID: "1"}, "v-new": {VersionID: "2"},
			"u-ok": {SizeBytes: 10, ETag: `"abc"`}, "u-size": {SizeBytes: 11, ETag: `"abc"`}, "u-etag": {SizeBytes: 10, ETag: `"zzz"`},
		},
		errs: map[string]error{"u-ssec": interfaces.ErrStorageBadRequest, "u-down": errors.New("connection reset")},
	}
	cases := map[string]struct {
		o    postgres.StoredObject
		kind string
	}{
		"version intact":       {postgres.StoredObject{Key: "v-ok", VersionID: "1"}, ""},
		"newer version on top": {postgres.StoredObject{Key: "v-new", VersionID: "1"}, model.DriftNewVersion},
		"delete marker":        {postgres.StoredObject{Key: "v-marker", VersionID: "1"}, model.DriftDeleted},
		"pinned version gone":  {postgres.StoredObject{Key: "v-gone", VersionID: "1"}, model.DriftDeleted},
		"unversioned intact":   {postgres.StoredObject{Key: "u-ok", SizeBytes: 10, ETag: "abc"}, ""},
		"size changed":         {postgres.StoredObject{Key: "u-size", SizeBytes: 10, ETag: "abc"}, model.DriftOverwritten},
		"etag changed":         {postgres.StoredObject{Key: "u-etag", SizeBytes: 10, ETag: "abc"}, model.DriftOverwritten},
		"deleted":              {postgres.StoredObject{Key: "u-gone", SizeBytes: 10}, model.DriftDeleted},
		"re-encrypted":         {postgres.StoredObject{Key: "u-ssec", SizeBytes: 10}, model.DriftUnreadable},
	}
	for name, tc := range cases {
		kind, _, err := examine(context.Background(), d, "b", tc.o)
		if err != nil || kind != tc.kind {
			t.Errorf("%s: %q %v", name, kind, err)
		}
	}
	// A provider that does not answer says nothing about the object.
	if kind, _, err := examine(context.Background(), d, "b", postgres.StoredObject{Key: "u-down"}); err == nil || kind != "" {
		t.Errorf("unreachable: %q %v", kind, err)
	}
}

func TestLastSelfTest(t *testing.T) {
	now := time.Now()
	recent, old := now.Add(-time.Hour), now.Add(-40*24*time.Hour)
	if lastSelfTest(model.StorageConnection{LastTestOKAt: &recent}, now).Status != model.PosturePass ||
		lastSelfTest(model.StorageConnection{LastTestOKAt: &old}, now).Status != model.PostureWarn ||
		lastSelfTest(model.StorageConnection{}, now).Status != model.PostureWarn {
		t.Fatal("self test freshness")
	}
}
