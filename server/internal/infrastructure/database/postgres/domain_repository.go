package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// DomainRepository keeps recordings' metadata, segments, embedding indexes,
// upstream sources and publications (docs/domain.md).
type DomainRepository struct {
	db *DB
}

func NewDomainRepository(db *DB) *DomainRepository {
	return &DomainRepository{db: db}
}

func (r *DomainRepository) PutRecording(ctx context.Context, m model.RecordingMeta) error {
	topics, err := json.Marshal(m.Topics)
	if err != nil {
		return err
	}
	_, err = r.db.Pool.Exec(ctx, `INSERT INTO recording_metadata (version_id, tenant_id, asset_id, path, kind, start_ns, end_ns, message_count, topics)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (version_id, asset_id) DO UPDATE SET kind = $5, start_ns = $6, end_ns = $7, message_count = $8, topics = $9`,
		m.VersionID, m.TenantID, m.AssetID, m.Path, m.Kind, m.StartNS, m.EndNS, m.MessageCount, topics)
	if err != nil {
		return fmt.Errorf("put recording: %w", err)
	}
	return nil
}

// Recordings lists a version's recordings, optionally those holding a topic
// or overlapping a time range.
func (r *DomainRepository) Recordings(ctx context.Context, tenantID, versionID, topic string, fromNS, toNS *int64, limit int) ([]model.RecordingMeta, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT version_id, tenant_id, asset_id, path, kind, start_ns, end_ns, message_count, topics
		FROM recording_metadata WHERE tenant_id = $1 AND version_id = $2
		  AND ($3 = '' OR topics @> jsonb_build_array(jsonb_build_object('topic', $3::text)))
		  AND ($4::bigint IS NULL OR end_ns >= $4) AND ($5::bigint IS NULL OR start_ns <= $5)
		ORDER BY start_ns, path LIMIT $6`, tenantID, versionID, topic, fromNS, toNS, limit)
	if err != nil {
		return nil, fmt.Errorf("list recordings: %w", err)
	}
	defer rows.Close()
	out := []model.RecordingMeta{}
	for rows.Next() {
		var m model.RecordingMeta
		var topics []byte
		if err := rows.Scan(&m.VersionID, &m.TenantID, &m.AssetID, &m.Path, &m.Kind, &m.StartNS, &m.EndNS, &m.MessageCount, &topics); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(topics, &m.Topics); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MemberAsset finds a path's asset in a version.
func (r *DomainRepository) MemberAsset(ctx context.Context, tenantID, versionID, path string) (string, error) {
	var asset string
	err := r.db.Pool.QueryRow(ctx, `SELECT asset_id FROM dataset_version_members WHERE tenant_id = $1 AND version_id = $2 AND path = $3`,
		tenantID, versionID, path).Scan(&asset)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: %s is not in this snapshot", model.ErrInvalidDomain, path)
	}
	return asset, err
}

// RecordingRange answers a file's recorded time range, if it has one.
func (r *DomainRepository) RecordingRange(ctx context.Context, tenantID, versionID, assetID string) (int64, int64, bool, error) {
	var start, end int64
	err := r.db.Pool.QueryRow(ctx, `SELECT start_ns, end_ns FROM recording_metadata WHERE tenant_id = $1 AND version_id = $2 AND asset_id = $3`,
		tenantID, versionID, assetID).Scan(&start, &end)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, false, nil
	}
	return start, end, err == nil, err
}

// STACMember is a member as a STAC export needs it.
type STACMember struct {
	AssetID     string
	Path        string
	SizeBytes   int64
	ContentType string
	SHA256      string
	Metadata    map[string]string
}

// MembersForSTAC walks a version's members with those of one STAC item side
// by side, so an export can stream items without holding the version.
func (r *DomainRepository) MembersForSTAC(ctx context.Context, tenantID, versionID string, each func(STACMember) error) error {
	rows, err := r.db.Pool.Query(ctx, `SELECT asset_id, path, size_bytes, content_type, COALESCE(checksum_sha256, ''), metadata
		FROM dataset_version_members WHERE tenant_id = $1 AND version_id = $2
		ORDER BY metadata ->> 'stac.item' NULLS LAST, path`, tenantID, versionID)
	if err != nil {
		return fmt.Errorf("stac members: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m STACMember
		var meta []byte
		if err := rows.Scan(&m.AssetID, &m.Path, &m.SizeBytes, &m.ContentType, &m.SHA256, &meta); err != nil {
			return err
		}
		if err := json.Unmarshal(meta, &m.Metadata); err != nil {
			return err
		}
		if err := each(m); err != nil {
			return err
		}
	}
	return rows.Err()
}

// AddSegments writes segments whole or not at all.
func (r *DomainRepository) AddSegments(ctx context.Context, segments []model.Segment) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	for _, s := range segments {
		attrs, err := json.Marshal(s.Attributes)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO segments (id, tenant_id, dataset_id, version_id, asset_id, path, start_ns, end_ns, label, note, attributes, created_by, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			s.ID, s.TenantID, s.DatasetID, s.VersionID, s.AssetID, s.Path, s.StartNS, s.EndNS, s.Label, s.Note, attrs, s.CreatedBy, s.CreatedAt); err != nil {
			return fmt.Errorf("add segment: %w", err)
		}
	}
	return tx.Commit(ctx)
}

const segmentColumns = `id, tenant_id, dataset_id, version_id, asset_id, path, start_ns, end_ns, label, note, attributes, created_by, created_at`

func scanSegment(row pgx.Row) (model.Segment, error) {
	var s model.Segment
	var attrs []byte
	err := row.Scan(&s.ID, &s.TenantID, &s.DatasetID, &s.VersionID, &s.AssetID, &s.Path, &s.StartNS, &s.EndNS, &s.Label, &s.Note, &attrs, &s.CreatedBy, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, model.ErrDomainNotFound
	}
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(attrs, &s.Attributes)
}

func (r *DomainRepository) Segments(ctx context.Context, tenantID, versionID string, f model.SegmentFilter) ([]model.Segment, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT `+segmentColumns+` FROM segments
		WHERE tenant_id = $1 AND version_id = $2 AND ($3 = '' OR path = $3) AND ($4 = '' OR label = $4)
		  AND ($5::bigint IS NULL OR end_ns >= $5) AND ($6::bigint IS NULL OR start_ns <= $6) AND id > $7
		ORDER BY id LIMIT $8`, tenantID, versionID, f.Path, f.Label, f.FromNS, f.ToNS, f.AfterID, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("list segments: %w", err)
	}
	defer rows.Close()
	out := []model.Segment{}
	for rows.Next() {
		s, err := scanSegment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Segment finds one of a version's segments.
func (r *DomainRepository) Segment(ctx context.Context, tenantID, versionID, id string) (model.Segment, error) {
	return scanSegment(r.db.Pool.QueryRow(ctx, `SELECT `+segmentColumns+` FROM segments WHERE tenant_id = $1 AND version_id = $2 AND id = $3`, tenantID, versionID, id))
}

// DeleteSegment removes a segment and the vectors computed over it.
func (r *DomainRepository) DeleteSegment(ctx context.Context, tenantID, versionID, id string) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	tag, err := tx.Exec(ctx, `DELETE FROM segments WHERE tenant_id = $1 AND version_id = $2 AND id = $3`, tenantID, versionID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return model.ErrDomainNotFound
	}
	if _, err := tx.Exec(ctx, `DELETE FROM embeddings WHERE tenant_id = $1 AND segment_id = $2`, tenantID, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *DomainRepository) AddIndex(ctx context.Context, x model.EmbeddingIndex) error {
	_, err := r.db.Pool.Exec(ctx, `INSERT INTO embedding_indexes (id, tenant_id, dataset_id, version_id, name, model, dimensions, metric, created_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`, x.ID, x.TenantID, x.DatasetID, x.VersionID, x.Name, x.Model, x.Dimensions, x.Metric, x.CreatedBy, x.CreatedAt)
	if isUniqueViolation(err) {
		return fmt.Errorf("%w: this snapshot already has an index with that name", model.ErrInvalidDomain)
	}
	if err != nil {
		return fmt.Errorf("add index: %w", err)
	}
	return nil
}

const indexColumns = `x.id, x.tenant_id, x.dataset_id, x.version_id, x.name, x.model, x.dimensions, x.metric, x.created_by, x.created_at,
	(SELECT count(*) FROM embeddings e WHERE e.index_id = x.id)`

func scanIndex(row pgx.Row) (model.EmbeddingIndex, error) {
	var x model.EmbeddingIndex
	err := row.Scan(&x.ID, &x.TenantID, &x.DatasetID, &x.VersionID, &x.Name, &x.Model, &x.Dimensions, &x.Metric, &x.CreatedBy, &x.CreatedAt, &x.Vectors)
	if errors.Is(err, pgx.ErrNoRows) {
		return x, model.ErrDomainNotFound
	}
	return x, err
}

func (r *DomainRepository) Index(ctx context.Context, tenantID, id string) (model.EmbeddingIndex, error) {
	return scanIndex(r.db.Pool.QueryRow(ctx, `SELECT `+indexColumns+` FROM embedding_indexes x WHERE x.tenant_id = $1 AND x.id = $2`, tenantID, id))
}

func (r *DomainRepository) Indexes(ctx context.Context, tenantID, versionID string) ([]model.EmbeddingIndex, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT `+indexColumns+` FROM embedding_indexes x WHERE x.tenant_id = $1 AND x.version_id = $2 ORDER BY x.name`, tenantID, versionID)
	if err != nil {
		return nil, fmt.Errorf("list indexes: %w", err)
	}
	defer rows.Close()
	out := []model.EmbeddingIndex{}
	for rows.Next() {
		x, err := scanIndex(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (r *DomainRepository) DeleteIndex(ctx context.Context, tenantID, id string) error {
	_, err := r.db.Pool.Exec(ctx, `DELETE FROM embedding_indexes WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	return err
}

// PutVectors adds or replaces vectors whole or not at all. Each is already
// encoded, in the same order as rows.
func (r *DomainRepository) PutVectors(ctx context.Context, tenantID, indexID string, rows []model.Embedding, encoded [][]byte) error {
	batch := &pgx.Batch{}
	for i, e := range rows {
		batch.Queue(`INSERT INTO embeddings (index_id, tenant_id, path, segment_id, vector) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (index_id, path, segment_id) DO UPDATE SET vector = $5`, indexID, tenantID, e.Path, e.SegmentID, encoded[i])
	}
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	results := tx.SendBatch(ctx, batch)
	for range rows {
		if _, err := results.Exec(); err != nil {
			results.Close()
			return fmt.Errorf("put vector: %w", err)
		}
	}
	if err := results.Close(); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ScanVectors walks an index's vectors, optionally only segments or files of
// a path. The bytes passed are only valid during the call.
func (r *DomainRepository) ScanVectors(ctx context.Context, tenantID, indexID, path string, each func(path, segmentID string, vector []byte) error) error {
	rows, err := r.db.Pool.Query(ctx, `SELECT path, segment_id, vector FROM embeddings WHERE tenant_id = $1 AND index_id = $2 AND ($3 = '' OR path = $3)`,
		tenantID, indexID, path)
	if err != nil {
		return fmt.Errorf("scan vectors: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p, seg string
		var vec []byte
		if err := rows.Scan(&p, &seg, &vec); err != nil {
			return err
		}
		if err := each(p, seg, vec); err != nil {
			return err
		}
	}
	return rows.Err()
}

// WatchNewUpstream starts watching what imports brought in by URL since the
// last pass. Answers how many it added.
func (r *DomainRepository) WatchNewUpstream(ctx context.Context, now time.Time, limit int) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx, `INSERT INTO upstream_sources (tenant_id, url, asset_id, path, sha256, size_bytes, state, created_at, next_check_at)
		SELECT DISTINCT ON (e.tenant_id, e.url) e.tenant_id, e.url, e.asset_id, e.path, e.sha256, e.size_bytes, 'WATCHING', $1::timestamptz, $1::timestamptz
		FROM adoption_entries e
		WHERE e.url <> '' AND e.state = 'IMPORTED' AND e.asset_id <> ''
		  AND NOT EXISTS (SELECT 1 FROM upstream_sources u WHERE u.tenant_id = e.tenant_id AND u.url = e.url)
		ORDER BY e.tenant_id, e.url, e.updated_at DESC
		LIMIT $2
		ON CONFLICT (tenant_id, url) DO NOTHING`, now, limit)
	if err != nil {
		return 0, fmt.Errorf("watch upstream: %w", err)
	}
	return tag.RowsAffected(), nil
}

// UpstreamLink is a snapshot holding bytes that came from an upstream URL.
type UpstreamLink struct {
	TenantID  string
	VersionID string
	URL       string
	SHA256    string
}

// UnlinkedUpstream finds snapshots not yet linked in the lineage to the
// upstream copies of their members.
func (r *DomainRepository) UnlinkedUpstream(ctx context.Context, limit int) ([]UpstreamLink, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT DISTINCT u.tenant_id, m.version_id, u.url, u.sha256
		FROM upstream_sources u
		JOIN dataset_version_members m ON m.asset_id = u.asset_id AND m.tenant_id = u.tenant_id
		WHERE NOT EXISTS (
		    SELECT 1 FROM lineage_nodes vn
		    JOIN lineage_edges le ON le.from_node = vn.id
		    JOIN lineage_nodes un ON un.id = le.to_node
		    WHERE vn.tenant_id = u.tenant_id AND vn.kind = 'DATASET_VERSION' AND vn.ref_id = m.version_id
		      AND un.tenant_id = u.tenant_id AND un.kind = 'UPSTREAM_SOURCE' AND un.ref_id = u.url)
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("unlinked upstream: %w", err)
	}
	defer rows.Close()
	out := []UpstreamLink{}
	for rows.Next() {
		var l UpstreamLink
		if err := rows.Scan(&l.TenantID, &l.VersionID, &l.URL, &l.SHA256); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

const upstreamColumns = `tenant_id, url, asset_id, path, sha256, size_bytes, state, etag, last_modified, note, created_at, checked_at, changed_at, next_check_at`

func scanUpstream(row pgx.Row) (model.UpstreamSource, error) {
	var u model.UpstreamSource
	err := row.Scan(&u.TenantID, &u.URL, &u.AssetID, &u.Path, &u.SHA256, &u.SizeBytes, &u.State, &u.ETag, &u.LastModified, &u.Note, &u.CreatedAt, &u.CheckedAt, &u.ChangedAt, &u.NextCheckAt)
	return u, err
}

func (r *DomainRepository) collectUpstream(rows pgx.Rows, err error) ([]model.UpstreamSource, error) {
	if err != nil {
		return nil, fmt.Errorf("list upstream: %w", err)
	}
	defer rows.Close()
	out := []model.UpstreamSource{}
	for rows.Next() {
		u, err := scanUpstream(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// DueUpstream answers sources whose time to be checked has come. A source
// already gone is not checked again.
func (r *DomainRepository) DueUpstream(ctx context.Context, now time.Time, limit int) ([]model.UpstreamSource, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT `+upstreamColumns+` FROM upstream_sources
		WHERE next_check_at <= $1 AND state <> 'GONE' ORDER BY next_check_at LIMIT $2`, now, limit)
	return r.collectUpstream(rows, err)
}

// PutUpstream records a check.
//
// It is not guarded against a GONE source: the checker never turns GONE
// back into WATCHING (DueUpstream skips GONE sources and checkSource only
// moves to CHANGED or GONE), and there is no re-watch operation that
// would need to. Two checkers racing on one due source can still let the
// later write win; guard on state <> 'GONE' if a re-watch is ever added.
func (r *DomainRepository) PutUpstream(ctx context.Context, u model.UpstreamSource) error {
	_, err := r.db.Pool.Exec(ctx, `UPDATE upstream_sources SET state = $3, etag = $4, last_modified = $5, note = $6, checked_at = $7, changed_at = $8, next_check_at = $9
		WHERE tenant_id = $1 AND url = $2`, u.TenantID, u.URL, u.State, u.ETag, u.LastModified, u.Note, u.CheckedAt, u.ChangedAt, u.NextCheckAt)
	return err
}

// Upstream lists a tenant's sources, optionally those of a dataset's
// snapshots or only those needing attention.
func (r *DomainRepository) Upstream(ctx context.Context, tenantID, datasetID, state string, limit int) ([]model.UpstreamSource, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT `+upstreamColumns+` FROM upstream_sources u
		WHERE u.tenant_id = $1 AND ($3 = '' OR u.state = $3)
		  AND ($2 = '' OR EXISTS (SELECT 1 FROM dataset_version_members m JOIN dataset_versions v ON v.id = m.version_id
		       WHERE m.tenant_id = u.tenant_id AND m.asset_id = u.asset_id AND v.dataset_id = $2))
		ORDER BY u.state <> 'GONE', u.state <> 'CHANGED', u.path LIMIT $4`, tenantID, datasetID, state, limit)
	return r.collectUpstream(rows, err)
}

func (r *DomainRepository) AddPublication(ctx context.Context, p model.Publication) error {
	creators, err := json.Marshal(p.Creators)
	if err != nil {
		return err
	}
	_, err = r.db.Pool.Exec(ctx, `INSERT INTO publications (id, tenant_id, dataset_id, version_id, slug, title, description, creators, publisher, publication_year,
		license, version_number, manifest_sha256, member_count, total_bytes, created_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
		p.ID, p.TenantID, p.DatasetID, p.VersionID, p.Slug, p.Title, p.Description, creators, p.Publisher, p.PublicationYear,
		p.License, p.VersionNumber, p.ManifestSHA256, p.MemberCount, p.TotalBytes, p.CreatedBy, p.CreatedAt)
	if isUniqueViolation(err) {
		return model.ErrAlreadyPublished
	}
	if err != nil {
		return fmt.Errorf("add publication: %w", err)
	}
	return nil
}

// SetDOI records how registering a DOI went, while the publication is not
// withdrawn.
func (r *DomainRepository) SetDOI(ctx context.Context, tenantID, id, doi, failure string) error {
	tag, err := r.db.Pool.Exec(ctx, `UPDATE publications SET doi = $3, doi_error = $4 WHERE tenant_id = $1 AND id = $2 AND withdrawn_at IS NULL`,
		tenantID, id, doi, failure)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return model.ErrWithdrawn
	}
	return nil
}

func (r *DomainRepository) Withdraw(ctx context.Context, tenantID, id, reason string, at time.Time) error {
	_, err := r.db.Pool.Exec(ctx, `UPDATE publications SET withdrawn_at = $3, withdrawn_reason = $4 WHERE tenant_id = $1 AND id = $2 AND withdrawn_at IS NULL`,
		tenantID, id, at, reason)
	return err
}

const publicationColumns = `id, tenant_id, dataset_id, version_id, slug, title, description, creators, publisher, publication_year,
	license, version_number, manifest_sha256, member_count, total_bytes, doi, doi_error, withdrawn_at, withdrawn_reason, created_by, created_at`

func scanPublication(row pgx.Row) (model.Publication, error) {
	var p model.Publication
	var creators []byte
	err := row.Scan(&p.ID, &p.TenantID, &p.DatasetID, &p.VersionID, &p.Slug, &p.Title, &p.Description, &creators, &p.Publisher, &p.PublicationYear,
		&p.License, &p.VersionNumber, &p.ManifestSHA256, &p.MemberCount, &p.TotalBytes, &p.DOI, &p.DOIError, &p.WithdrawnAt, &p.WithdrawnReason, &p.CreatedBy, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, model.ErrDomainNotFound
	}
	if err != nil {
		return p, err
	}
	return p, json.Unmarshal(creators, &p.Creators)
}

// PublicationBySlug needs no tenant: it is what the public landing page reads.
func (r *DomainRepository) PublicationBySlug(ctx context.Context, slug string) (model.Publication, error) {
	return scanPublication(r.db.Pool.QueryRow(ctx, `SELECT `+publicationColumns+` FROM publications WHERE slug = $1`, slug))
}

func (r *DomainRepository) PublicationOf(ctx context.Context, tenantID, versionID string) (model.Publication, error) {
	return scanPublication(r.db.Pool.QueryRow(ctx, `SELECT `+publicationColumns+` FROM publications WHERE tenant_id = $1 AND version_id = $2`, tenantID, versionID))
}

func (r *DomainRepository) Publications(ctx context.Context, tenantID, datasetID string) ([]model.Publication, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT `+publicationColumns+` FROM publications WHERE tenant_id = $1 AND dataset_id = $2 ORDER BY created_at DESC`, tenantID, datasetID)
	if err != nil {
		return nil, fmt.Errorf("list publications: %w", err)
	}
	defer rows.Close()
	out := []model.Publication{}
	for rows.Next() {
		p, err := scanPublication(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DatasetLicense is the licence and source recorded on a dataset.
func (r *DomainRepository) DatasetLicense(ctx context.Context, tenantID, datasetID string) (string, error) {
	var license string
	err := r.db.Pool.QueryRow(ctx, `SELECT COALESCE(license, '') FROM datasets WHERE tenant_id = $1 AND id = $2`, tenantID, datasetID).Scan(&license)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", model.ErrDatasetNotFound
	}
	return license, err
}
