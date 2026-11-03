// Package domain serves what particular fields need on top of snapshots
// (docs/domain.md): STAC export, recordings' metadata, segments inside files,
// embedding indexes people bring, the upstream copies imports came from, and
// publishing a snapshot with a landing page and a DOI.
package domain

import (
	"bytes"
	"container/heap"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
	"github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
)

// DataCite is the repository account DOIs are registered with.
type DataCite struct {
	API        string // https://api.datacite.org
	Repository string
	Password   string
	Prefix     string // 10.xxxx
}

// Lineage records edges of the graph.
type Lineage interface {
	AddLineage(ctx context.Context, tenantID string, e model.LineageEdge, now time.Time) error
}

type Deps struct {
	Store    *postgres.DomainRepository
	Datasets *dataset.Service
	Lineage  Lineage
	Audit    interfaces.AuditRecorder
	// Client must be the network guard's: upstream URLs are the tenant's.
	Client    *http.Client
	DataCite  DataCite
	PublicURL string // the console's address, where landing pages are
	Log       *slog.Logger
}

type Service struct {
	deps Deps
	now  func() time.Time
}

// CheckPublicURL says whether an address can go in a citation. An address with
// no scheme or host would pass as text and resolve to nothing.
func CheckPublicURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%q is not an http or https address", raw)
	}
	return nil
}

func New(deps Deps) *Service {
	deps.PublicURL = strings.TrimRight(deps.PublicURL, "/")
	deps.DataCite.API = strings.TrimRight(deps.DataCite.API, "/")
	return &Service{deps: deps, now: func() time.Time { return time.Now().UTC() }}
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{model.ErrInvalidDomain}, args...)...)
}

func (s *Service) record(ctx context.Context, p model.Principal, action, kind, id string, details map[string]any) {
	if s.deps.Audit == nil {
		return
	}
	e := audit.From(ctx, p, action, kind, id)
	e.Details = details
	s.deps.Audit.Record(ctx, e)
}

// ─── STAC export ──────────────────────────────────────────────────────

const stacVersion = "1.0.0"

var stacLicense = regexp.MustCompile(`^[\w\-.+]+$`)

// stacValue gives a metadata string its JSON type back, only when that loses
// nothing: a processing baseline of "04.00" stays a string.
func stacValue(v string) any {
	if v == "true" || v == "false" {
		return v == "true"
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil && strconv.FormatFloat(f, 'f', -1, 64) == v {
		return f
	}
	return v
}

func parseBBox(v string) []float64 {
	parts := strings.Split(v, ",")
	if len(parts) != 4 && len(parts) != 6 {
		return nil
	}
	out := make([]float64, 0, len(parts))
	for _, part := range parts {
		f, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return nil
		}
		out = append(out, f)
	}
	return out
}

// stacExtent grows a collection's extent as members pass by.
type stacExtent struct {
	west, south, east, north float64
	hasBox                   bool
	first, last              string
}

func (e *stacExtent) add(meta map[string]string) {
	if box := parseBBox(meta["stac.bbox"]); box != nil {
		half := len(box) / 2
		w, so, ea, no := box[0], box[1], box[half], box[half+1]
		if !e.hasBox {
			e.west, e.south, e.east, e.north, e.hasBox = w, so, ea, no, true
		} else {
			e.west, e.south, e.east, e.north = min(e.west, w), min(e.south, so), max(e.east, ea), max(e.north, no)
		}
	}
	if at := meta["stac.datetime"]; at != "" {
		if e.first == "" || at < e.first {
			e.first = at
		}
		if at > e.last {
			e.last = at
		}
	}
}

// STACCollection answers a snapshot as a STAC Collection, with its items at
// the items.json beside it.
func (s *Service) STACCollection(ctx context.Context, p model.Principal, datasetID, versionID string) (map[string]any, error) {
	ds, err := s.deps.Datasets.Get(ctx, p, datasetID)
	if err != nil {
		return nil, err
	}
	v, err := s.deps.Datasets.Version(ctx, p, datasetID, versionID)
	if err != nil {
		return nil, err
	}
	var extent stacExtent
	if err := s.deps.Store.MembersForSTAC(ctx, p.TenantID, v.ID, func(m postgres.STACMember) error {
		extent.add(m.Metadata)
		return nil
	}); err != nil {
		return nil, err
	}
	license, err := s.deps.Store.DatasetLicense(ctx, p.TenantID, ds.ID)
	if err != nil {
		return nil, err
	}
	if !stacLicense.MatchString(license) {
		license = "proprietary"
	}
	box := []float64{-180, -90, 180, 90}
	if extent.hasBox {
		box = []float64{extent.west, extent.south, extent.east, extent.north}
	}
	first, last := any(v.CreatedAt.UTC().Format(time.RFC3339)), any(nil)
	if extent.first != "" {
		first, last = extent.first, extent.last
	}
	description := ds.Description
	if description == "" {
		description = ds.Name
	}
	return map[string]any{
		"type": "Collection", "stac_version": stacVersion, "stac_extensions": []string{},
		"id": fmt.Sprintf("%s-v%d", ds.Name, v.Version), "title": ds.Name, "description": description, "license": license,
		"extent": map[string]any{
			"spatial":  map[string]any{"bbox": [][]float64{box}},
			"temporal": map[string]any{"interval": [][]any{{first, last}}},
		},
		"links": []map[string]string{
			{"rel": "items", "href": "./items.json", "type": "application/geo+json"},
			{"rel": "self", "href": "./collection.json", "type": "application/json"},
		},
		"osp:manifest_sha256": v.ManifestSHA256,
	}, nil
}

// stacItemWriter gathers one item's members, which arrive side by side.
type stacItemWriter struct {
	w          io.Writer
	collection string
	fallback   string
	id         string
	meta       map[string]string
	assets     map[string]any
	written    int
}

func (b *stacItemWriter) flush() error {
	if b.id == "" {
		return nil
	}
	properties := map[string]any{"datetime": b.fallback}
	var geometry any
	var box []float64
	for k, v := range b.meta {
		key, ok := strings.CutPrefix(k, "stac.")
		if !ok {
			continue
		}
		switch key {
		case "item", "collection":
		case "bbox":
			box = parseBBox(v)
		case "geometry":
			_ = json.Unmarshal([]byte(v), &geometry)
		case "datetime":
			properties["datetime"] = v
		default:
			// Ingest wrote eo:cloud_cover as eo.cloud_cover.
			properties[strings.Replace(key, ".", ":", 1)] = stacValue(v)
		}
	}
	item := map[string]any{
		"type": "Feature", "stac_version": stacVersion,
		"stac_extensions": []string{"https://stac-extensions.github.io/file/v2.1.0/schema.json"},
		"id":              b.id, "collection": b.collection, "geometry": geometry, "properties": properties, "assets": b.assets,
		"links": []map[string]string{{"rel": "collection", "href": "./collection.json", "type": "application/json"}},
	}
	// The STAC specification wants a bbox exactly when there is a geometry.
	if geometry != nil && box != nil {
		item["bbox"] = box
	} else if geometry == nil && box != nil {
		west, south := box[0], box[1]
		east, north := box[len(box)/2], box[len(box)/2+1]
		item["bbox"] = box
		item["geometry"] = map[string]any{"type": "Polygon", "coordinates": [][][]float64{{
			{west, south}, {east, south}, {east, north}, {west, north}, {west, south}}}}
	}
	raw, err := json.Marshal(item)
	if err != nil {
		return err
	}
	if b.written > 0 {
		if _, err := io.WriteString(b.w, ","); err != nil {
			return err
		}
	}
	b.written++
	_, err = b.w.Write(raw)
	b.id = ""
	return err
}

// WriteSTACItems streams a snapshot's items as a FeatureCollection. Members
// imported from STAC group back into their items; any other file is an item
// of its own. Hrefs are this API's, relative, and need a credential to read.
func (s *Service) WriteSTACItems(ctx context.Context, p model.Principal, datasetID, versionID string, w io.Writer) error {
	ds, err := s.deps.Datasets.Get(ctx, p, datasetID)
	if err != nil {
		return err
	}
	v, err := s.deps.Datasets.Version(ctx, p, datasetID, versionID)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, `{"type":"FeatureCollection","features":[`); err != nil {
		return err
	}
	b := &stacItemWriter{w: w, collection: fmt.Sprintf("%s-v%d", ds.Name, v.Version), fallback: v.CreatedAt.UTC().Format(time.RFC3339)}
	err = s.deps.Store.MembersForSTAC(ctx, p.TenantID, v.ID, func(m postgres.STACMember) error {
		id := m.Metadata["stac.item"]
		key := strings.TrimSuffix(path.Base(m.Path), path.Ext(m.Path))
		if id == "" {
			id, key = strings.ReplaceAll(m.Path, "/", "_"), "data"
		}
		if id != b.id {
			if err := b.flush(); err != nil {
				return err
			}
			b.id, b.meta, b.assets = id, m.Metadata, map[string]any{}
		}
		asset := map[string]any{"href": "/api/v1/assets/" + m.AssetID + "/content", "title": m.Path, "file:size": m.SizeBytes}
		if m.ContentType != "" {
			asset["type"] = m.ContentType
		}
		if m.SHA256 != "" {
			asset["file:checksum"] = "1220" + m.SHA256 // a multihash of SHA-256
		}
		b.assets[key] = asset
		return nil
	})
	if err != nil {
		return err
	}
	if err := b.flush(); err != nil {
		return err
	}
	_, err = io.WriteString(w, "]}")
	return err
}

// ─── Recordings and segments ──────────────────────────────────────────

func (s *Service) Recordings(ctx context.Context, p model.Principal, datasetID, versionID, topic string, fromNS, toNS *int64) ([]model.RecordingMeta, error) {
	v, err := s.deps.Datasets.Version(ctx, p, datasetID, versionID)
	if err != nil {
		return nil, err
	}
	return s.deps.Store.Recordings(ctx, p.TenantID, v.ID, topic, fromNS, toNS, 1000)
}

const maxSegmentsPerCall = 1000

// AddSegments records time ranges inside a snapshot's files, all or none.
// A range outside what the recording says it holds is refused.
func (s *Service) AddSegments(ctx context.Context, p model.Principal, datasetID, versionID string, in []model.Segment) ([]model.Segment, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsWrite, "dataset.segments_add", "dataset_version", versionID); err != nil {
		return nil, err
	}
	v, err := s.deps.Datasets.Version(ctx, p, datasetID, versionID)
	if err != nil {
		return nil, err
	}
	if len(in) == 0 || len(in) > maxSegmentsPerCall {
		return nil, invalid("send 1 to %d segments", maxSegmentsPerCall)
	}
	assets := map[string]string{}
	out := make([]model.Segment, 0, len(in))
	for _, seg := range in {
		if seg.StartNS < 0 || seg.EndNS < seg.StartNS {
			return nil, invalid("%s: a segment ends at or after it starts, in nanoseconds", seg.Path)
		}
		if len(seg.Label) > 200 || len(seg.Note) > 2000 {
			return nil, invalid("a label is at most 200 characters, a note at most 2000")
		}
		asset, known := assets[seg.Path]
		if !known {
			if asset, err = s.deps.Store.MemberAsset(ctx, p.TenantID, v.ID, seg.Path); err != nil {
				return nil, err
			}
			assets[seg.Path] = asset
		}
		start, end, found, err := s.deps.Store.RecordingRange(ctx, p.TenantID, v.ID, asset)
		if err != nil {
			return nil, err
		}
		if found && (seg.StartNS > end || seg.EndNS < start) {
			return nil, invalid("%s records %d to %d, and this segment is outside that", seg.Path, start, end)
		}
		if seg.Attributes == nil {
			seg.Attributes = map[string]any{}
		}
		raw, err := json.Marshal(seg.Attributes)
		if err != nil || len(raw) > 4096 {
			return nil, invalid("a segment's attributes are at most 4 KiB of JSON")
		}
		seg.ID, seg.TenantID, seg.DatasetID, seg.VersionID, seg.AssetID = event.NewID(), p.TenantID, v.DatasetID, v.ID, asset
		seg.CreatedBy, seg.CreatedAt = p.UserID, s.now()
		out = append(out, seg)
	}
	if err := s.deps.Store.AddSegments(ctx, out); err != nil {
		return nil, err
	}
	s.record(ctx, p, "dataset.segments_add", "dataset_version", v.ID, map[string]any{"segments": len(out)})
	return out, nil
}

func (s *Service) Segments(ctx context.Context, p model.Principal, datasetID, versionID string, f model.SegmentFilter) ([]model.Segment, error) {
	v, err := s.deps.Datasets.Version(ctx, p, datasetID, versionID)
	if err != nil {
		return nil, err
	}
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 200
	}
	return s.deps.Store.Segments(ctx, p.TenantID, v.ID, f)
}

func (s *Service) DeleteSegment(ctx context.Context, p model.Principal, datasetID, versionID, id string) error {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsWrite, "dataset.segment_delete", "segment", id); err != nil {
		return err
	}
	v, err := s.deps.Datasets.Version(ctx, p, datasetID, versionID)
	if err != nil {
		return err
	}
	if err := s.deps.Store.DeleteSegment(ctx, p.TenantID, v.ID, id); err != nil {
		return err
	}
	s.record(ctx, p, "dataset.segment_delete", "segment", id, nil)
	return nil
}

// ─── Embedding indexes ────────────────────────────────────────────────

const (
	maxVectorsPerCall = 1000
	maxIndexVectors   = 1_000_000
	maxQueryResults   = 1000
)

func (s *Service) CreateIndex(ctx context.Context, p model.Principal, datasetID, versionID string, in model.EmbeddingIndex) (model.EmbeddingIndex, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsWrite, "dataset.embedding_index_create", "dataset_version", versionID); err != nil {
		return in, err
	}
	v, err := s.deps.Datasets.Version(ctx, p, datasetID, versionID)
	if err != nil {
		return in, err
	}
	in.Name, in.Metric = strings.TrimSpace(in.Name), strings.ToUpper(strings.TrimSpace(in.Metric))
	if in.Metric == "" {
		in.Metric = model.MetricCosine
	}
	switch {
	case in.Name == "" || len(in.Name) > 100:
		return in, invalid("a name of 1 to 100 characters")
	case len(in.Model) > 200:
		return in, invalid("a model name of at most 200 characters")
	case in.Dimensions < 1 || in.Dimensions > 4096:
		return in, invalid("dimensions from 1 to 4096")
	case in.Metric != model.MetricCosine && in.Metric != model.MetricDot && in.Metric != model.MetricEuclidean:
		return in, invalid("a metric of COSINE, DOT or EUCLIDEAN")
	}
	in.ID, in.TenantID, in.DatasetID, in.VersionID, in.CreatedBy, in.CreatedAt = event.NewID(), p.TenantID, v.DatasetID, v.ID, p.UserID, s.now()
	if err := s.deps.Store.AddIndex(ctx, in); err != nil {
		return in, err
	}
	s.record(ctx, p, "dataset.embedding_index_create", "embedding_index", in.ID, map[string]any{"name": in.Name, "model": in.Model, "dimensions": in.Dimensions})
	return in, nil
}

func (s *Service) Indexes(ctx context.Context, p model.Principal, datasetID, versionID string) ([]model.EmbeddingIndex, error) {
	v, err := s.deps.Datasets.Version(ctx, p, datasetID, versionID)
	if err != nil {
		return nil, err
	}
	return s.deps.Store.Indexes(ctx, p.TenantID, v.ID)
}

// index finds an index the caller may read: that of a snapshot they can read.
func (s *Service) index(ctx context.Context, p model.Principal, id string) (model.EmbeddingIndex, error) {
	x, err := s.deps.Store.Index(ctx, p.TenantID, id)
	if err != nil {
		return x, err
	}
	if _, err := s.deps.Datasets.Version(ctx, p, x.DatasetID, x.VersionID); err != nil {
		return x, err
	}
	return x, nil
}

func (s *Service) DeleteIndex(ctx context.Context, p model.Principal, id string) error {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsWrite, "dataset.embedding_index_delete", "embedding_index", id); err != nil {
		return err
	}
	x, err := s.index(ctx, p, id)
	if err != nil {
		return err
	}
	if err := s.deps.Store.DeleteIndex(ctx, p.TenantID, x.ID); err != nil {
		return err
	}
	s.record(ctx, p, "dataset.embedding_index_delete", "embedding_index", x.ID, nil)
	return nil
}

func encodeVector(v []float32) []byte {
	out := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(out[4*i:], math.Float32bits(f))
	}
	return out
}

// PutVectors adds or replaces vectors, all or none. Each names a file of the
// snapshot, or one of its segments.
func (s *Service) PutVectors(ctx context.Context, p model.Principal, id string, rows []model.Embedding) (int, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsWrite, "dataset.embeddings_put", "embedding_index", id); err != nil {
		return 0, err
	}
	x, err := s.index(ctx, p, id)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 || len(rows) > maxVectorsPerCall {
		return 0, invalid("send 1 to %d vectors", maxVectorsPerCall)
	}
	if x.Vectors+int64(len(rows)) > maxIndexVectors {
		return 0, invalid("an index holds at most %d vectors", maxIndexVectors)
	}
	paths := map[string]bool{}
	encoded := make([][]byte, len(rows))
	for i, e := range rows {
		if len(e.Vector) != x.Dimensions {
			return 0, invalid("%s: this index's vectors have %d dimensions, not %d", e.Path, x.Dimensions, len(e.Vector))
		}
		if x.Metric == model.MetricCosine && norm(e.Vector) == 0 {
			return 0, invalid("%s: a zero vector has no direction to compare by cosine", e.Path)
		}
		if !paths[e.Path] {
			if _, err := s.deps.Store.MemberAsset(ctx, p.TenantID, x.VersionID, e.Path); err != nil {
				return 0, err
			}
			paths[e.Path] = true
		}
		if e.SegmentID != "" {
			seg, err := s.deps.Store.Segment(ctx, p.TenantID, x.VersionID, e.SegmentID)
			if err != nil || seg.Path != e.Path {
				return 0, invalid("%s has no segment %s", e.Path, e.SegmentID)
			}
		}
		encoded[i] = encodeVector(e.Vector)
	}
	if err := s.deps.Store.PutVectors(ctx, p.TenantID, x.ID, rows, encoded); err != nil {
		return 0, err
	}
	s.record(ctx, p, "dataset.embeddings_put", "embedding_index", x.ID, map[string]any{"vectors": len(rows)})
	return len(rows), nil
}

func norm(v []float32) float64 {
	sum := 0.0
	for _, f := range v {
		sum += float64(f) * float64(f)
	}
	return math.Sqrt(sum)
}

// Match is one query result; a higher score is closer.
type Match struct {
	Path      string  `json:"path"`
	SegmentID string  `json:"segment_id,omitempty"`
	Score     float64 `json:"score"`
}

type matchHeap []Match

func (h matchHeap) Len() int           { return len(h) }
func (h matchHeap) Less(a, b int) bool { return h[a].Score < h[b].Score }
func (h matchHeap) Swap(a, b int)      { h[a], h[b] = h[b], h[a] }
func (h *matchHeap) Push(x any)        { *h = append(*h, x.(Match)) }
func (h *matchHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

// score compares a stored vector with the query. For cosine, stored vectors
// are never zero (PutVectors) and queryNorm was checked by the caller.
func score(metric string, query []float32, queryNorm float64, stored []byte) float64 {
	dot, sq, dist := 0.0, 0.0, 0.0
	for i, q := range query {
		f := float64(math.Float32frombits(binary.LittleEndian.Uint32(stored[4*i:])))
		dot += float64(q) * f
		sq += f * f
		dist += (float64(q) - f) * (float64(q) - f)
	}
	switch metric {
	case model.MetricDot:
		return dot
	case model.MetricEuclidean:
		return -math.Sqrt(dist)
	}
	return dot / (queryNorm * math.Sqrt(sq))
}

// Query finds the nearest vectors by reading all of them.
//
// ponytail: an exact scan, so results are never approximate, and a query over
// the 1M-vector ceiling takes a second or more. Add pgvector with HNSW when
// indexes get larger or queries must be faster.
func (s *Service) Query(ctx context.Context, p model.Principal, id string, query []float32, top int, onlyPath string) ([]Match, error) {
	x, err := s.index(ctx, p, id)
	if err != nil {
		return nil, err
	}
	if len(query) != x.Dimensions {
		return nil, invalid("this index's vectors have %d dimensions, not %d", x.Dimensions, len(query))
	}
	if top <= 0 {
		top = 10
	}
	if top > maxQueryResults {
		return nil, invalid("ask for at most %d results", maxQueryResults)
	}
	queryNorm := norm(query)
	if x.Metric == model.MetricCosine && queryNorm == 0 {
		return nil, invalid("a zero vector has no direction to compare by cosine")
	}
	nearest := &matchHeap{}
	err = s.deps.Store.ScanVectors(ctx, p.TenantID, x.ID, onlyPath, func(path, segmentID string, stored []byte) error {
		if len(stored) != 4*x.Dimensions {
			return fmt.Errorf("vector of %s has %d bytes", path, len(stored))
		}
		m := Match{Path: path, SegmentID: segmentID, Score: score(x.Metric, query, queryNorm, stored)}
		if nearest.Len() < top {
			heap.Push(nearest, m)
		} else if m.Score > (*nearest)[0].Score {
			(*nearest)[0] = m
			heap.Fix(nearest, 0)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]Match, nearest.Len())
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = heap.Pop(nearest).(Match)
	}
	return out, nil
}

// ─── Upstream sources ─────────────────────────────────────────────────

const (
	upstreamBatch = 100
	upstreamEvery = 24 * time.Hour
)

// CheckUpstream starts watching what imports brought in by URL, links it into
// the lineage, and asks each due source whether it still is what was imported.
// Run by the cleanup worker. Answers how many sources it asked.
func (s *Service) CheckUpstream(ctx context.Context) (int, error) {
	if _, err := s.deps.Store.WatchNewUpstream(ctx, s.now(), 1000); err != nil {
		return 0, err
	}
	links, err := s.deps.Store.UnlinkedUpstream(ctx, 500)
	if err != nil {
		return 0, err
	}
	for _, l := range links {
		err := s.deps.Lineage.AddLineage(ctx, l.TenantID, model.LineageEdge{
			From:     model.LineageRef{Kind: model.NodeDatasetVersion, RefID: l.VersionID},
			To:       model.LineageRef{Kind: model.NodeUpstreamSource, RefID: l.URL},
			Relation: model.RelImportedFrom, Actor: "system",
			Details: map[string]any{"sha256": l.SHA256}}, s.now())
		if err != nil {
			return 0, fmt.Errorf("link upstream: %w", err)
		}
	}
	due, err := s.deps.Store.DueUpstream(ctx, s.now(), upstreamBatch)
	if err != nil {
		return 0, err
	}
	for _, u := range due {
		if err := s.deps.Store.PutUpstream(ctx, s.checkSource(ctx, u)); err != nil {
			return 0, err
		}
	}
	return len(due), nil
}

// checkSource asks with a HEAD. The first answer becomes the baseline, except
// that a size other than what was imported is a change at once. An answer
// that says nothing (refused, redirected, a server error) changes no state,
// only the note: absence of evidence is not a change.
func (s *Service) checkSource(ctx context.Context, u model.UpstreamSource) model.UpstreamSource {
	now := s.now()
	u.CheckedAt, u.NextCheckAt, u.Note = &now, now.Add(upstreamEvery), ""
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u.URL, nil)
	if err != nil {
		u.Note = "the URL cannot be requested"
		return u
	}
	res, err := s.deps.Client.Do(req)
	if err != nil {
		u.Note = "not reachable at the last check"
		return u
	}
	res.Body.Close()
	changed := func(note string) {
		if u.State != model.UpstreamChanged {
			u.ChangedAt = &now
		}
		u.State, u.Note = model.UpstreamChanged, note
	}
	switch {
	case res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusGone:
		u.State, u.ChangedAt, u.Note = model.UpstreamGone, &now, fmt.Sprintf("the server answered %d", res.StatusCode)
		return u
	case res.StatusCode >= 300:
		u.Note = fmt.Sprintf("the server answered %d, which does not say whether the file changed", res.StatusCode)
		return u
	}
	etag, modified := res.Header.Get("ETag"), res.Header.Get("Last-Modified")
	switch {
	case res.ContentLength >= 0 && u.SizeBytes > 0 && res.ContentLength != u.SizeBytes:
		changed(fmt.Sprintf("it is now %d bytes; %d were imported", res.ContentLength, u.SizeBytes))
	case u.ETag != "" && etag != "" && etag != u.ETag:
		changed("its ETag changed from " + u.ETag + " to " + etag)
	case u.ETag == "" && u.LastModified != "" && modified != "" && modified != u.LastModified:
		changed("it was modified on " + modified)
	}
	if u.State == model.UpstreamChanged {
		return u // the baseline stays what was first seen
	}
	if etag != "" {
		u.ETag = etag
	}
	if modified != "" {
		u.LastModified = modified
	}
	return u
}

// Upstream lists sources, optionally of one dataset or in one state.
func (s *Service) Upstream(ctx context.Context, p model.Principal, datasetID, state string) ([]model.UpstreamSource, error) {
	if datasetID != "" {
		if _, err := s.deps.Datasets.Get(ctx, p, datasetID); err != nil {
			return nil, err
		}
	} else if err := audit.Require(ctx, s.deps.Audit, p, model.PermOrgRead, "dataset.upstream_read", "upstream_source", ""); err != nil {
		return nil, err
	}
	return s.deps.Store.Upstream(ctx, p.TenantID, datasetID, state, 1000)
}

// ─── Publishing ───────────────────────────────────────────────────────

func newSlug() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func (s *Service) doiConfigured() bool {
	d := s.deps.DataCite
	return d.API != "" && d.Repository != "" && d.Password != "" && d.Prefix != "" && s.deps.PublicURL != ""
}

// Publish makes a snapshot citable: a public landing page with its metadata
// and manifest hash, and, when asked, a DOI. The data stays behind its access
// controls. A DOI that fails leaves the publication standing, with the error;
// MintDOI tries again.
func (s *Service) Publish(ctx context.Context, p model.Principal, datasetID, versionID string, meta model.PublicationMeta, mint bool) (model.Publication, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsManage, "dataset.publish", "dataset_version", versionID); err != nil {
		return model.Publication{}, err
	}
	v, err := s.deps.Datasets.Version(ctx, p, datasetID, versionID)
	if err != nil {
		return model.Publication{}, err
	}
	if v.ExpiredAt != nil || v.SupersededBy != "" {
		return model.Publication{}, invalid("this snapshot expired or was superseded, and cannot be published")
	}
	// Without its address, a citation would point nowhere.
	if s.deps.PublicURL == "" {
		return model.Publication{}, model.ErrPublishingOff
	}
	if mint && !s.doiConfigured() {
		return model.Publication{}, model.ErrDOIOff
	}
	if err := cleanMeta(&meta); err != nil {
		return model.Publication{}, err
	}
	if meta.License == "" {
		if meta.License, err = s.deps.Store.DatasetLicense(ctx, p.TenantID, v.DatasetID); err != nil {
			return model.Publication{}, err
		}
		if meta.License == "" {
			return model.Publication{}, invalid("name a licence, here or in the dataset's provenance")
		}
	}
	slug, err := newSlug()
	if err != nil {
		return model.Publication{}, err
	}
	now := s.now()
	pub := model.Publication{PublicationMeta: meta, ID: event.NewID(), TenantID: p.TenantID, DatasetID: v.DatasetID, VersionID: v.ID, Slug: slug,
		PublicationYear: now.Year(), VersionNumber: v.Version, ManifestSHA256: v.ManifestSHA256, MemberCount: v.MemberCount, TotalBytes: v.TotalBytes,
		CreatedBy: p.UserID, CreatedAt: now}
	if err := s.deps.Store.AddPublication(ctx, pub); err != nil {
		return pub, err
	}
	s.record(ctx, p, "dataset.publish", "dataset_version", v.ID, map[string]any{"slug": slug, "title": meta.Title, "license": meta.License})
	if mint {
		return s.mint(ctx, p, pub)
	}
	return pub, nil
}

func cleanMeta(m *model.PublicationMeta) error {
	m.Title, m.Publisher, m.License = strings.TrimSpace(m.Title), strings.TrimSpace(m.Publisher), strings.TrimSpace(m.License)
	switch {
	case m.Title == "" || len(m.Title) > 500:
		return invalid("a title of 1 to 500 characters")
	case m.Publisher == "" || len(m.Publisher) > 500:
		return invalid("a publisher of 1 to 500 characters")
	case len(m.Description) > 10000:
		return invalid("a description of at most 10000 characters")
	case len(m.License) > 500:
		return invalid("a licence of at most 500 characters")
	case len(m.Creators) == 0 || len(m.Creators) > 200:
		return invalid("name 1 to 200 creators")
	}
	for i := range m.Creators {
		m.Creators[i].Name = strings.TrimSpace(m.Creators[i].Name)
		if m.Creators[i].Name == "" {
			return invalid("every creator has a name")
		}
	}
	return nil
}

// MintDOI registers a publication's DOI, or tries again after a failure.
func (s *Service) MintDOI(ctx context.Context, p model.Principal, datasetID, versionID string) (model.Publication, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsManage, "dataset.doi_mint", "dataset_version", versionID); err != nil {
		return model.Publication{}, err
	}
	pub, err := s.Publication(ctx, p, datasetID, versionID)
	if err != nil {
		return pub, err
	}
	if pub.DOI != "" {
		return pub, nil
	}
	if !s.doiConfigured() {
		return pub, model.ErrDOIOff
	}
	return s.mint(ctx, p, pub)
}

// mint registers the DOI with a PUT on a name made from the slug, so that
// trying again after an answer that got lost cannot make a second DOI.
func (s *Service) mint(ctx context.Context, p model.Principal, pub model.Publication) (model.Publication, error) {
	doi := s.deps.DataCite.Prefix + "/osp." + pub.Slug
	if err := s.registerDOI(ctx, doi, pub); err != nil {
		s.deps.Log.Warn("DOI registration failed", "publication", pub.ID, "error", err)
		pub.DOIError = err.Error()
		if err := s.deps.Store.SetDOI(ctx, pub.TenantID, pub.ID, "", pub.DOIError); err != nil {
			return pub, err
		}
		return pub, nil
	}
	if err := s.deps.Store.SetDOI(ctx, pub.TenantID, pub.ID, doi, ""); err != nil {
		return pub, err
	}
	pub.DOI, pub.DOIError = doi, ""
	s.record(ctx, p, "dataset.doi_mint", "dataset_version", pub.VersionID, map[string]any{"doi": doi})
	return pub, nil
}

func (s *Service) registerDOI(ctx context.Context, doi string, pub model.Publication) error {
	attributes := DataCiteAttributes(pub, s.LandingURL(pub.Slug))
	attributes["doi"], attributes["event"] = doi, "publish"
	body, err := json.Marshal(map[string]any{"data": map[string]any{"type": "dois", "attributes": attributes}})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.deps.DataCite.API+"/dois/"+doi, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/vnd.api+json")
	req.SetBasicAuth(s.deps.DataCite.Repository, s.deps.DataCite.Password)
	res, err := s.deps.Client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", model.ErrDOIFailed, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
		detail, _ := io.ReadAll(io.LimitReader(res.Body, 500))
		return fmt.Errorf("%w: DataCite answered %d: %s", model.ErrDOIFailed, res.StatusCode, strings.TrimSpace(string(detail)))
	}
	return nil
}

// Withdraw marks a publication withdrawn. The landing page stays and says so:
// a DOI must keep resolving, and the bytes are not touched.
func (s *Service) Withdraw(ctx context.Context, p model.Principal, datasetID, versionID, reason string) (model.Publication, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsManage, "dataset.publication_withdraw", "dataset_version", versionID); err != nil {
		return model.Publication{}, err
	}
	pub, err := s.Publication(ctx, p, datasetID, versionID)
	if err != nil {
		return pub, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 2000 {
		return pub, invalid("say why, in at most 2000 characters: readers of the landing page will see it")
	}
	if err := s.deps.Store.Withdraw(ctx, p.TenantID, pub.ID, reason, s.now()); err != nil {
		return pub, err
	}
	s.record(ctx, p, "dataset.publication_withdraw", "dataset_version", pub.VersionID, map[string]any{"reason": reason})
	return s.Publication(ctx, p, datasetID, versionID)
}

// LandingURL is where a publication's public page is, or "" when the server
// does not know its own address: never half a link.
func (s *Service) LandingURL(slug string) string {
	if s.deps.PublicURL == "" {
		return ""
	}
	return s.deps.PublicURL + "/published/" + slug
}

func (s *Service) Publication(ctx context.Context, p model.Principal, datasetID, versionID string) (model.Publication, error) {
	v, err := s.deps.Datasets.Version(ctx, p, datasetID, versionID)
	if err != nil {
		return model.Publication{}, err
	}
	return s.deps.Store.PublicationOf(ctx, p.TenantID, v.ID)
}

func (s *Service) Publications(ctx context.Context, p model.Principal, datasetID string) ([]model.Publication, error) {
	ds, err := s.deps.Datasets.Get(ctx, p, datasetID)
	if err != nil {
		return nil, err
	}
	return s.deps.Store.Publications(ctx, p.TenantID, ds.ID)
}

// PublicRecord is what anyone may read about a publication. It is built field
// by field so that nothing internal (tenant, IDs, who published it, a failed
// DOI's error) can reach the page by being added to Publication.
type PublicRecord struct {
	Slug            string          `json:"slug"`
	Title           string          `json:"title"`
	Description     string          `json:"description"`
	Creators        []model.Creator `json:"creators"`
	Publisher       string          `json:"publisher"`
	PublicationYear int             `json:"publication_year"`
	License         string          `json:"license"`
	Version         int             `json:"version"`
	ManifestSHA256  string          `json:"manifest_sha256"`
	MemberCount     int64           `json:"member_count"`
	TotalBytes      int64           `json:"total_bytes"`
	DOI             string          `json:"doi,omitempty"`
	PublishedAt     time.Time       `json:"published_at"`
	WithdrawnAt     *time.Time      `json:"withdrawn_at,omitempty"`
	WithdrawnReason string          `json:"withdrawn_reason,omitempty"`
	LandingURL      string          `json:"landing_url"`
	Citation        string          `json:"citation"`
	BibTeX          string          `json:"bibtex"`
	RIS             string          `json:"ris"`
	DataCite        map[string]any  `json:"datacite"`
	SchemaOrg       map[string]any  `json:"schema_org"`
}

// Public answers a publication's public record. It takes no principal.
func (s *Service) Public(ctx context.Context, slug string) (PublicRecord, error) {
	pub, err := s.deps.Store.PublicationBySlug(ctx, slug)
	if err != nil {
		return PublicRecord{}, err
	}
	landing := s.LandingURL(pub.Slug)
	return PublicRecord{Slug: pub.Slug, Title: pub.Title, Description: pub.Description, Creators: pub.Creators, Publisher: pub.Publisher,
		PublicationYear: pub.PublicationYear, License: pub.License, Version: pub.VersionNumber, ManifestSHA256: pub.ManifestSHA256,
		MemberCount: pub.MemberCount, TotalBytes: pub.TotalBytes, DOI: pub.DOI, PublishedAt: pub.CreatedAt, WithdrawnAt: pub.WithdrawnAt,
		WithdrawnReason: pub.WithdrawnReason, LandingURL: landing, Citation: citation(pub, landing), BibTeX: bibtex(pub, landing), RIS: ris(pub, landing),
		DataCite: DataCiteAttributes(pub, landing), SchemaOrg: schemaOrg(pub, landing)}, nil
}

// DataCiteAttributes is the DataCite metadata of a publication.
func DataCiteAttributes(pub model.Publication, landing string) map[string]any {
	creators := make([]map[string]any, 0, len(pub.Creators))
	for _, c := range pub.Creators {
		entry := map[string]any{"name": c.Name}
		if c.Affiliation != "" {
			entry["affiliation"] = []string{c.Affiliation}
		}
		if c.ORCID != "" {
			entry["nameIdentifiers"] = []map[string]string{{"nameIdentifier": "https://orcid.org/" + c.ORCID, "nameIdentifierScheme": "ORCID", "schemeUri": "https://orcid.org"}}
		}
		creators = append(creators, entry)
	}
	out := map[string]any{
		"creators": creators, "titles": []map[string]string{{"title": pub.Title}}, "publisher": pub.Publisher,
		"publicationYear": pub.PublicationYear, "types": map[string]string{"resourceTypeGeneral": "Dataset"}, "url": landing,
		"version": strconv.Itoa(pub.VersionNumber), "rightsList": []map[string]string{{"rights": pub.License}},
		"sizes": []string{fmt.Sprintf("%d files", pub.MemberCount), fmt.Sprintf("%d bytes", pub.TotalBytes)}, "schemaVersion": "http://datacite.org/schema/kernel-4",
	}
	if pub.Description != "" {
		out["descriptions"] = []map[string]string{{"description": pub.Description, "descriptionType": "Abstract"}}
	}
	return out
}

func identifierOf(pub model.Publication, landing string) string {
	if pub.DOI != "" {
		return "https://doi.org/" + pub.DOI
	}
	return landing
}

func creatorNames(pub model.Publication, sep string) string {
	names := make([]string, 0, len(pub.Creators))
	for _, c := range pub.Creators {
		names = append(names, c.Name)
	}
	return strings.Join(names, sep)
}

func citation(pub model.Publication, landing string) string {
	return strings.TrimSpace(fmt.Sprintf("%s (%d). %s (Version %d) [Data set]. %s. %s", creatorNames(pub, ", "), pub.PublicationYear, pub.Title, pub.VersionNumber, pub.Publisher, identifierOf(pub, landing)))
}

// bibtexEscaper keeps a title from closing its braces or starting a command.
var bibtexEscaper = strings.NewReplacer(`\`, `\textbackslash{}`, `{`, `\{`, `}`, `\}`, `%`, `\%`, `&`, `\&`, `$`, `\$`, `#`, `\#`, `_`, `\_`, "\n", " ", "\r", " ")

func bibtex(pub model.Publication, landing string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "@misc{osp_%s,\n", pub.Slug)
	fmt.Fprintf(&b, "  author = {%s},\n", bibtexEscaper.Replace(creatorNames(pub, " and ")))
	fmt.Fprintf(&b, "  title = {%s},\n", bibtexEscaper.Replace(pub.Title))
	fmt.Fprintf(&b, "  year = {%d},\n", pub.PublicationYear)
	fmt.Fprintf(&b, "  publisher = {%s},\n", bibtexEscaper.Replace(pub.Publisher))
	fmt.Fprintf(&b, "  version = {%d},\n", pub.VersionNumber)
	if pub.DOI != "" {
		fmt.Fprintf(&b, "  doi = {%s},\n", pub.DOI)
	}
	if id := identifierOf(pub, landing); id != "" {
		fmt.Fprintf(&b, "  url = {%s}\n", id)
	}
	b.WriteString("}\n")
	return b.String()
}

// ris writes one tag per line, so a line break in a value would forge a tag.
func ris(pub model.Publication, landing string) string {
	line := strings.NewReplacer("\r", " ", "\n", " ")
	var b strings.Builder
	b.WriteString("TY  - DATA\r\n")
	for _, c := range pub.Creators {
		fmt.Fprintf(&b, "AU  - %s\r\n", line.Replace(c.Name))
	}
	fmt.Fprintf(&b, "TI  - %s\r\nPY  - %d\r\nPB  - %s\r\nET  - %d\r\n", line.Replace(pub.Title), pub.PublicationYear, line.Replace(pub.Publisher), pub.VersionNumber)
	if pub.DOI != "" {
		fmt.Fprintf(&b, "DO  - %s\r\n", pub.DOI)
	}
	if id := identifierOf(pub, landing); id != "" {
		fmt.Fprintf(&b, "UR  - %s\r\n", id)
	}
	b.WriteString("ER  - \r\n")
	return b.String()
}

func schemaOrg(pub model.Publication, landing string) map[string]any {
	creators := make([]map[string]any, 0, len(pub.Creators))
	for _, c := range pub.Creators {
		entry := map[string]any{"@type": "Person", "name": c.Name}
		if c.Affiliation != "" {
			entry["affiliation"] = map[string]string{"@type": "Organization", "name": c.Affiliation}
		}
		if c.ORCID != "" {
			entry["sameAs"] = "https://orcid.org/" + c.ORCID
		}
		creators = append(creators, entry)
	}
	return map[string]any{
		"@context": "https://schema.org", "@type": "Dataset", "name": pub.Title, "description": pub.Description, "creator": creators,
		"publisher": map[string]string{"@type": "Organization", "name": pub.Publisher}, "datePublished": pub.CreatedAt.Format("2006-01-02"),
		"license": pub.License, "version": strconv.Itoa(pub.VersionNumber), "url": landing, "identifier": identifierOf(pub, landing),
	}
}

// ─── Retention cost ───────────────────────────────────────────────────

// TierCost is what keeping a snapshot costs on one storage tier.
type TierCost struct {
	Tier            string  `json:"tier"`
	PricePerGBMonth float64 `json:"price_per_gb_month"`
	PerYear         float64 `json:"per_year"`
	Total           float64 `json:"total"`
}

// RetentionEstimate is bytes × years × price. It knows nothing of retrieval,
// request or egress charges, and says so.
type RetentionEstimate struct {
	TotalBytes int64      `json:"total_bytes"`
	Years      int        `json:"years"`
	Tiers      []TierCost `json:"tiers"`
	Note       string     `json:"note"`
}

// defaultTiers are AWS S3 list prices in US dollars, a starting point only;
// a caller with their own price passes it.
var defaultTiers = []TierCost{
	{Tier: "S3 Standard", PricePerGBMonth: 0.023},
	{Tier: "S3 Standard-IA", PricePerGBMonth: 0.0125},
	{Tier: "S3 Glacier Instant Retrieval", PricePerGBMonth: 0.004},
	{Tier: "S3 Glacier Deep Archive", PricePerGBMonth: 0.00099},
}

// RetentionCost estimates what keeping a snapshot for years costs. A funder's
// retention period usually outlasts the grant, so it is asked before publishing.
func (s *Service) RetentionCost(ctx context.Context, p model.Principal, datasetID, versionID string, years int, price float64) (RetentionEstimate, error) {
	v, err := s.deps.Datasets.Version(ctx, p, datasetID, versionID)
	if err != nil {
		return RetentionEstimate{}, err
	}
	if years < 1 || years > 100 {
		return RetentionEstimate{}, invalid("years from 1 to 100")
	}
	if price < 0 {
		return RetentionEstimate{}, invalid("a price is not negative")
	}
	tiers := defaultTiers
	if price > 0 {
		tiers = []TierCost{{Tier: "Your price", PricePerGBMonth: price}}
	}
	return estimate(v.TotalBytes, years, tiers), nil
}

func estimate(totalBytes int64, years int, tiers []TierCost) RetentionEstimate {
	gb := float64(totalBytes) / (1 << 30)
	out := RetentionEstimate{TotalBytes: totalBytes, Years: years, Tiers: make([]TierCost, 0, len(tiers)),
		Note: "Storage only, at the prices shown, unchanged over the period. Retrieval, requests, egress and extra copies are not included."}
	for _, t := range tiers {
		t.PerYear = gb * t.PricePerGBMonth * 12
		t.Total = t.PerYear * float64(years)
		out.Tiers = append(out.Tiers, t)
	}
	return out
}
