package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	datasetservice "github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/domain"
)

func mcapRecord(op byte, body []byte) []byte {
	out := binary.LittleEndian.AppendUint64([]byte{op}, uint64(len(body)))
	return append(out, body...)
}

func mcapString(b []byte, s string) []byte {
	return append(binary.LittleEndian.AppendUint32(b, uint32(len(s))), s...)
}

// mcapFile is a small MCAP file with one topic and a summary section.
func mcapFile(startNS, endNS, messages uint64) []byte {
	const magic = "\x89MCAP0\r\n"
	file := []byte(magic)
	file = append(file, mcapRecord(0x01, mcapString(mcapString(nil, ""), "test"))...)
	file = append(file, mcapRecord(0x0F, binary.LittleEndian.AppendUint64(make([]byte, 8), 0))...) // data end, roughly

	schema := binary.LittleEndian.AppendUint16(nil, 1)
	schema = mcapString(mcapString(mcapString(schema, "sensor_msgs/Imu"), "ros2msg"), "")
	channel := binary.LittleEndian.AppendUint16(nil, 7)
	channel = binary.LittleEndian.AppendUint16(channel, 1)
	channel = mcapString(mcapString(channel, "/imu"), "cdr")
	channel = binary.LittleEndian.AppendUint32(channel, 0)
	stats := binary.LittleEndian.AppendUint64(nil, messages)
	stats = binary.LittleEndian.AppendUint16(stats, 1)
	stats = binary.LittleEndian.AppendUint32(stats, 1)
	stats = binary.LittleEndian.AppendUint32(stats, 0)
	stats = binary.LittleEndian.AppendUint32(stats, 0)
	stats = binary.LittleEndian.AppendUint32(stats, 0)
	stats = binary.LittleEndian.AppendUint64(stats, startNS)
	stats = binary.LittleEndian.AppendUint64(stats, endNS)
	stats = binary.LittleEndian.AppendUint32(stats, 10)
	stats = binary.LittleEndian.AppendUint16(stats, 7)
	stats = binary.LittleEndian.AppendUint64(stats, messages)

	summary := uint64(len(file))
	file = append(file, mcapRecord(0x03, schema)...)
	file = append(file, mcapRecord(0x04, channel)...)
	file = append(file, mcapRecord(0x0B, stats)...)
	footer := binary.LittleEndian.AppendUint64(nil, summary)
	footer = binary.LittleEndian.AppendUint64(footer, 0)
	footer = binary.LittleEndian.AppendUint32(footer, 0)
	file = append(file, mcapRecord(0x02, footer)...)
	return append(file, magic...)
}

const rosbagYAML = `rosbag2_bagfile_information:
  version: 5
  duration:
    nanoseconds: 4000
  starting_time:
    nanoseconds_since_epoch: 2000
  message_count: 8
  topics_with_message_count:
    - topic_metadata:
        name: /camera
        type: sensor_msgs/msg/Image
        serialization_format: cdr
      message_count: 8
`

// Recordings are indexed from their summaries; segments and vectors must name
// what is in the snapshot; STAC round-trips; a publication shows nothing
// internal and survives a failed DOI; upstream copies are watched.
func TestDomain(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "domain")
	dev, owner := acme.developer, acme.owner
	other := s.team(t, "domain-other").owner

	// DataCite that fails until told otherwise, and an upstream server.
	var mu sync.Mutex
	datacite := http.StatusServiceUnavailable
	var registered []string
	var registeredAuth, registeredBody string
	dc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		raw, _ := io.ReadAll(r.Body)
		if datacite == http.StatusCreated {
			registered = append(registered, r.Method+" "+r.URL.Path)
			registeredAuth, registeredBody = r.Header.Get("Authorization"), string(raw)
		}
		w.WriteHeader(datacite)
	}))
	defer dc.Close()
	svc := domain.New(domain.Deps{Store: postgres.NewDomainRepository(s.db), Datasets: s.datasets, Lineage: s.datasetDB, Audit: s.audit,
		Client: http.DefaultClient, PublicURL: "https://console.test/", Log: s.log,
		DataCite: domain.DataCite{API: dc.URL, Repository: "ORG.REPO", Password: "secret", Prefix: "10.5072"}})
	noDOI := domain.New(domain.Deps{Store: postgres.NewDomainRepository(s.db), Datasets: s.datasets, Audit: s.audit, Client: http.DefaultClient, PublicURL: "https://console.test", Log: s.log})
	noAddress := domain.New(domain.Deps{Store: postgres.NewDomainRepository(s.db), Datasets: s.datasets, Audit: s.audit, Client: http.DefaultClient, Log: s.log})

	mcap := s.publishAt(t, dev, "logs/run1.mcap", mcapFile(1_000, 9_000, 42))
	bag := s.publishAt(t, dev, "bags/b1/metadata.yaml", []byte(rosbagYAML))
	notBag := s.publishAt(t, dev, "conf/metadata.yaml", []byte("name: something else\n"))
	// A bag whose recorder never wrote metadata.yaml: only its SQLite file.
	db3Bytes, err := os.ReadFile("../../internal/platform/recording/testdata/rosbag2.db3")
	if err != nil {
		t.Fatal(err)
	}
	db3 := s.publishAt(t, dev, "bags/b2/bag_0.db3", db3Bytes)
	band := withMetadata(map[string]string{"stac.item": "t1", "stac.collection": "s2", "stac.datetime": "2024-03-01T10:00:00Z",
		"stac.eo.cloud_cover": "12.5", "stac.s2.processing_baseline": "04.00", "stac.bbox": "10,20,11,21"})
	b4 := s.publishAt(t, dev, "s2/t1/B04.bin", textData("band4", 300), band)
	b8 := s.publishAt(t, dev, "s2/t1/B08.bin", textData("band8", 300), band)

	d := s.newDataset(t, dev, "domain-set")
	s.addMembers(t, dev, d.ID, memberOf(mcap, "", ""), memberOf(bag, "", ""), memberOf(notBag, "", ""), memberOf(db3, "", ""), memberOf(b4, "", ""), memberOf(b8, "", ""))
	v, _, err := s.datasets.Snapshot(ctx, dev, d.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	s.profileAll(t)

	// Recordings.
	recs, err := svc.Recordings(ctx, dev, d.ID, v.ID, "", nil, nil)
	if err != nil || len(recs) != 3 {
		t.Fatalf("recordings: %+v %v", recs, err)
	}
	var fromDB *model.RecordingMeta
	for i := range recs {
		if recs[i].Path == "bags/b2/bag_0.db3" {
			fromDB = &recs[i]
		}
	}
	if fromDB == nil || fromDB.Kind != "ROSBAG2" || fromDB.MessageCount != 160 || fromDB.StartNS != 1730000000000000000 || len(fromDB.Topics) != 3 {
		t.Fatalf("rosbag2 sqlite: %+v", fromDB)
	}
	if r := recs[0]; r.Path != "logs/run1.mcap" || r.Kind != "MCAP" || r.StartNS != 1_000 || r.EndNS != 9_000 || r.MessageCount != 42 ||
		r.Topics[0].Topic != "/imu" || r.Topics[0].Schema != "sensor_msgs/Imu" {
		t.Fatalf("mcap: %+v", r)
	}
	if r := recs[1]; r.Kind != "ROSBAG2" || r.StartNS != 2_000 || r.EndNS != 6_000 || r.Topics[0].Topic != "/camera" {
		t.Fatalf("rosbag2: %+v", r)
	}
	if byTopic, err := svc.Recordings(ctx, dev, d.ID, v.ID, "/imu", nil, nil); err != nil || len(byTopic) != 2 {
		t.Fatalf("by topic: %+v %v", byTopic, err)
	}
	late := int64(7_000)
	if byTime, err := svc.Recordings(ctx, dev, d.ID, v.ID, "", &late, nil); err != nil || len(byTime) != 2 || byTime[0].Kind != "MCAP" {
		t.Fatalf("by time: %+v %v", byTime, err)
	}
	if _, err := svc.Recordings(ctx, other, d.ID, v.ID, "", nil, nil); err == nil {
		t.Fatal("another organization read the recordings")
	}

	// Segments.
	if _, err := svc.AddSegments(ctx, dev, d.ID, v.ID, []model.Segment{{Path: "logs/run1.mcap", StartNS: 20_000, EndNS: 30_000}}); !errors.Is(err, model.ErrInvalidDomain) {
		t.Fatalf("a range outside the recording: %v", err)
	}
	if _, err := svc.AddSegments(ctx, dev, d.ID, v.ID, []model.Segment{{Path: "not/here.mcap", StartNS: 1, EndNS: 2}}); !errors.Is(err, model.ErrInvalidDomain) {
		t.Fatalf("a path outside the snapshot: %v", err)
	}
	segs, err := svc.AddSegments(ctx, dev, d.ID, v.ID, []model.Segment{
		{Path: "logs/run1.mcap", StartNS: 1_000, EndNS: 2_000, Label: "cut-in", Attributes: map[string]any{"speed": 12.5}},
		{Path: "logs/run1.mcap", StartNS: 5_000, EndNS: 6_000, Label: "braking"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := svc.Segments(ctx, dev, d.ID, v.ID, model.SegmentFilter{Label: "braking"}); err != nil || len(got) != 1 || got[0].ID != segs[1].ID {
		t.Fatalf("by label: %+v %v", got, err)
	}
	inRange := int64(5_500)
	if got, err := svc.Segments(ctx, dev, d.ID, v.ID, model.SegmentFilter{FromNS: &inRange, ToNS: &inRange}); err != nil || len(got) != 1 || got[0].Label != "braking" {
		t.Fatalf("by overlap: %+v %v", got, err)
	}
	unchanged, err := s.datasets.Version(ctx, dev, d.ID, v.ID)
	if err != nil || unchanged.ManifestSHA256 != v.ManifestSHA256 {
		t.Fatalf("segments changed the manifest: %v", err)
	}

	// Embedding indexes.
	x, err := svc.CreateIndex(ctx, dev, d.ID, v.ID, model.EmbeddingIndex{Name: "clip", Model: "clip-vit", Dimensions: 2})
	if err != nil || x.Metric != model.MetricCosine {
		t.Fatalf("index: %+v %v", x, err)
	}
	if _, err := svc.CreateIndex(ctx, dev, d.ID, v.ID, model.EmbeddingIndex{Name: "clip", Dimensions: 2}); !errors.Is(err, model.ErrInvalidDomain) {
		t.Fatalf("a second index with that name: %v", err)
	}
	for name, rows := range map[string][]model.Embedding{
		"wrong dimensions": {{Path: "logs/run1.mcap", Vector: []float32{1}}},
		"zero vector":      {{Path: "logs/run1.mcap", Vector: []float32{0, 0}}},
		"outside":          {{Path: "not/here", Vector: []float32{1, 0}}},
		"foreign segment":  {{Path: "s2/t1/B04.bin", SegmentID: segs[0].ID, Vector: []float32{1, 0}}},
	} {
		if _, err := svc.PutVectors(ctx, dev, x.ID, rows); !errors.Is(err, model.ErrInvalidDomain) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := svc.PutVectors(ctx, dev, x.ID, []model.Embedding{
		{Path: "logs/run1.mcap", SegmentID: segs[0].ID, Vector: []float32{1, 0}},
		{Path: "logs/run1.mcap", SegmentID: segs[1].ID, Vector: []float32{0, 1}},
		{Path: "s2/t1/B04.bin", Vector: []float32{0.7, 0.7}},
	}); err != nil {
		t.Fatal(err)
	}
	matches, err := svc.Query(ctx, dev, x.ID, []float32{1, 0.1}, 2, "")
	if err != nil || len(matches) != 2 || matches[0].SegmentID != segs[0].ID || matches[1].Path != "s2/t1/B04.bin" || matches[0].Score <= matches[1].Score {
		t.Fatalf("query: %+v %v", matches, err)
	}
	if _, err := svc.Query(ctx, other, x.ID, []float32{1, 0}, 1, ""); !errors.Is(err, model.ErrDomainNotFound) {
		t.Fatalf("another organization's query: %v", err)
	}
	if err := svc.DeleteSegment(ctx, dev, d.ID, v.ID, segs[0].ID); err != nil {
		t.Fatal(err)
	}
	if after, err := svc.Query(ctx, dev, x.ID, []float32{1, 0}, 10, ""); err != nil || len(after) != 2 {
		t.Fatalf("a segment's vector outlived it: %+v %v", after, err)
	}

	// STAC.
	collection, err := svc.STACCollection(ctx, dev, d.ID, v.ID)
	if err != nil || collection["type"] != "Collection" || collection["license"] != "proprietary" {
		t.Fatalf("collection: %+v %v", collection, err)
	}
	var buf bytes.Buffer
	if err := svc.WriteSTACItems(ctx, dev, d.ID, v.ID, &buf); err != nil {
		t.Fatal(err)
	}
	var fc struct {
		Features []struct {
			ID         string         `json:"id"`
			Geometry   map[string]any `json:"geometry"`
			BBox       []float64      `json:"bbox"`
			Properties map[string]any `json:"properties"`
			Assets     map[string]struct {
				Href     string `json:"href"`
				Size     int64  `json:"file:size"`
				Checksum string `json:"file:checksum"`
			} `json:"assets"`
		} `json:"features"`
	}
	if err := json.Unmarshal(buf.Bytes(), &fc); err != nil {
		t.Fatalf("%v: %s", err, buf.String())
	}
	found := false
	for _, f := range fc.Features {
		if f.ID != "t1" {
			continue
		}
		found = true
		if len(f.Assets) != 2 || f.Assets["B04"].Href != "/api/v1/assets/"+b4.ID+"/content" || f.Assets["B04"].Checksum != "1220"+sha256Of(textData("band4", 300)) {
			t.Fatalf("assets: %+v", f.Assets)
		}
		if f.Properties["eo:cloud_cover"] != 12.5 || f.Properties["s2:processing_baseline"] != "04.00" || f.Properties["datetime"] != "2024-03-01T10:00:00Z" {
			t.Fatalf("properties: %+v", f.Properties)
		}
		if f.Geometry == nil || len(f.BBox) != 4 {
			t.Fatalf("place: %+v %+v", f.Geometry, f.BBox)
		}
	}
	if !found || len(fc.Features) != 5 { // the STAC item and each other file, the bag's SQLite file included
		t.Fatalf("features: %s", buf.String())
	}

	// Publishing.
	meta := model.PublicationMeta{Title: "Run logs", Creators: []model.Creator{{Name: "Ada Lovelace", Affiliation: "Lab", ORCID: "0000-0002-1825-0097"}}, Publisher: "Acme Robotics"}
	if _, err := svc.Publish(ctx, dev, d.ID, v.ID, meta, false); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("publishing needs datasets.manage: %v", err)
	}
	if _, err := svc.Publish(ctx, owner, d.ID, v.ID, meta, false); !errors.Is(err, model.ErrInvalidDomain) {
		t.Fatalf("publishing with no licence: %v", err)
	}
	meta.License = "CC-BY-4.0"
	if _, err := noAddress.Publish(ctx, owner, d.ID, v.ID, meta, false); !errors.Is(err, model.ErrPublishingOff) {
		t.Fatalf("publishing with no public address: %v", err)
	}
	if _, err := noDOI.Publish(ctx, owner, d.ID, v.ID, meta, true); !errors.Is(err, model.ErrDOIOff) {
		t.Fatalf("a DOI with DataCite not configured: %v", err)
	}
	pub, err := svc.Publish(ctx, owner, d.ID, v.ID, meta, true)
	if err != nil || pub.DOI != "" || !strings.Contains(pub.DOIError, "503") {
		t.Fatalf("a failed DOI leaves the publication with the error: %+v %v", pub, err)
	}
	if _, err := svc.Publish(ctx, owner, d.ID, v.ID, meta, false); !errors.Is(err, model.ErrAlreadyPublished) {
		t.Fatalf("published twice: %v", err)
	}
	mu.Lock()
	datacite = http.StatusCreated
	mu.Unlock()
	pub, err = svc.MintDOI(ctx, owner, d.ID, v.ID)
	if err != nil || pub.DOI != "10.5072/osp."+pub.Slug || pub.DOIError != "" {
		t.Fatalf("mint: %+v %v", pub, err)
	}
	if again, err := svc.MintDOI(ctx, owner, d.ID, v.ID); err != nil || again.DOI != pub.DOI {
		t.Fatalf("mint again: %+v %v", again, err)
	}
	mu.Lock()
	if len(registered) != 1 || registered[0] != "PUT /dois/"+pub.DOI ||
		registeredAuth != "Basic "+base64.StdEncoding.EncodeToString([]byte("ORG.REPO:secret")) ||
		!strings.Contains(registeredBody, `"url":"https://console.test/published/`+pub.Slug+`"`) || !strings.Contains(registeredBody, `"event":"publish"`) {
		t.Fatalf("registered: %v %s %s", registered, registeredAuth, registeredBody)
	}
	mu.Unlock()

	record, err := svc.Public(ctx, pub.Slug)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(record)
	for _, internal := range []string{dev.TenantID, d.ID, v.ID, owner.UserID, "logs/run1.mcap", "doi_error"} {
		if strings.Contains(string(raw), internal) {
			t.Fatalf("the public record shows %q: %s", internal, raw)
		}
	}
	if record.ManifestSHA256 != v.ManifestSHA256 || !strings.Contains(record.BibTeX, pub.DOI) || !strings.Contains(record.RIS, "TY  - DATA") ||
		!strings.Contains(record.Citation, "https://doi.org/"+pub.DOI) || record.SchemaOrg["@type"] != "Dataset" {
		t.Fatalf("record: %s", raw)
	}
	if _, err := svc.Public(ctx, "nope"); !errors.Is(err, model.ErrDomainNotFound) {
		t.Fatalf("an unknown slug: %v", err)
	}
	if _, err := svc.Withdraw(ctx, owner, d.ID, v.ID, ""); !errors.Is(err, model.ErrInvalidDomain) {
		t.Fatalf("withdrawing without saying why: %v", err)
	}
	if _, err := svc.Withdraw(ctx, owner, d.ID, v.ID, "wrong calibration"); err != nil {
		t.Fatal(err)
	}
	if after, err := svc.Public(ctx, pub.Slug); err != nil || after.WithdrawnAt == nil || after.WithdrawnReason != "wrong calibration" || after.DOI != pub.DOI {
		t.Fatalf("a withdrawn page stays and says so: %+v %v", after, err)
	}
	uses, err := postgres.NewTieringRepository(s.db).VersionUses(ctx, dev.TenantID, d.ID)
	if err != nil || len(uses) != 1 || uses[0].Reason == "" {
		t.Fatalf("uses: %+v %v", uses, err)
	}
	cost, err := svc.RetentionCost(ctx, dev, d.ID, v.ID, 10, 0.01)
	if err != nil || cost.Years != 10 || len(cost.Tiers) != 1 || cost.TotalBytes != v.TotalBytes {
		t.Fatalf("cost: %+v %v", cost, err)
	}

	// Upstream sources.
	var state struct {
		sync.Mutex
		status int
		etag   string
		length int
	}
	state.status, state.etag, state.length = http.StatusOK, `"v1"`, 300
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.Lock()
		defer state.Unlock()
		w.Header().Set("ETag", state.etag)
		w.Header().Set("Content-Length", strconv.Itoa(state.length))
		w.WriteHeader(state.status)
	}))
	defer upstream.Close()
	now := time.Now().UTC()
	if _, err := s.db.Pool.Exec(ctx, `INSERT INTO adoption_jobs (id, tenant_id, connection_id, source, mode, state, created_by, created_at, updated_at)
		VALUES ('job-domain', $1, 'platform', 'URL_LIST', 'MANAGE', 'COMPLETED', 'x', $2, $2)`, dev.TenantID, now); err != nil {
		t.Fatal(err)
	}
	insert := func(line int, url, path, assetID string) {
		t.Helper()
		if _, err := s.db.Pool.Exec(ctx, `INSERT INTO adoption_entries (job_id, line, tenant_id, url, path, size_bytes, state, asset_id, sha256, updated_at)
			VALUES ('job-domain', $1, $2, $3, $4, 300, 'IMPORTED', $5, 'abc', $6)`, line, dev.TenantID, url, path, assetID, now); err != nil {
			t.Fatal(err)
		}
	}
	insert(1, upstream.URL+"/b04", "s2/t1/B04.bin", b4.ID)
	if _, err := s.db.Pool.Exec(ctx, `INSERT INTO webhook_endpoints (id, tenant_id, url, event_types, enabled, secret_key_version, secret_nonce, secret_ciphertext, created_by, created_at, updated_at)
		VALUES ('hook-domain', $1, 'https://hooks.test/x', ARRAY['upstream.changed', 'upstream.gone'], true, 1, 'a'::bytea, 'a'::bytea, 'x', $2, $2)`, dev.TenantID, now); err != nil {
		t.Fatal(err)
	}
	check := func(want string) model.UpstreamSource {
		t.Helper()
		// Everything is due again: a check moves it a day ahead.
		if _, err := s.db.Pool.Exec(ctx, `UPDATE upstream_sources SET next_check_at = now() - interval '1 minute' WHERE tenant_id = $1`, dev.TenantID); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.CheckUpstream(ctx); err != nil {
			t.Fatal(err)
		}
		list, err := svc.Upstream(ctx, dev, d.ID, "")
		if err != nil || len(list) != 1 || list[0].State != want {
			t.Fatalf("want %s: %+v %v", want, list, err)
		}
		return list[0]
	}
	first := check(model.UpstreamWatching)
	if first.ETag != `"v1"` || first.CheckedAt == nil {
		t.Fatalf("the first answer is the baseline: %+v", first)
	}
	state.Lock()
	state.status = http.StatusInternalServerError
	state.Unlock()
	if got := check(model.UpstreamWatching); !strings.Contains(got.Note, "500") {
		t.Fatalf("a server error changes nothing but the note: %+v", got)
	}
	state.Lock()
	state.status, state.etag = http.StatusOK, `"v2"`
	state.Unlock()
	if got := check(model.UpstreamChanged); got.ChangedAt == nil || !strings.Contains(got.Note, "ETag") {
		t.Fatalf("changed: %+v", got)
	}
	state.Lock()
	state.status = http.StatusNotFound
	state.Unlock()
	check(model.UpstreamGone)
	var changedHooks, goneHooks int
	if err := s.db.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE event_type = 'upstream.changed'), count(*) FILTER (WHERE event_type = 'upstream.gone')
		FROM webhook_deliveries WHERE tenant_id = $1`, dev.TenantID).Scan(&changedHooks, &goneHooks); err != nil || changedHooks != 1 || goneHooks != 1 {
		t.Fatalf("webhooks: %d %d %v", changedHooks, goneHooks, err)
	}
	lineage, err := s.datasets.VersionLineage(ctx, dev, v.ID, datasetservice.LineageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	linked := false
	for _, e := range lineage.Edges {
		if e.To.Kind == model.NodeUpstreamSource && e.To.RefID == upstream.URL+"/b04" && e.Relation == model.RelImportedFrom {
			linked = true
		}
	}
	if !linked {
		t.Fatalf("no upstream node in the lineage: %+v", lineage.Edges)
	}
}
