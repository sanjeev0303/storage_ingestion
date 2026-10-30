package copyengine

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// The bundle's files: what rebuilding the catalog from a second bucket
// needs. Each query takes the tenant as $1 and names its columns; no
// credential is ever selected.
var bundleFiles = []struct{ name, query string }{
	{"connections.jsonl", `SELECT id, name, provider, auth_method, bucket, region, endpoint, account_id, staging_prefix, ready_prefix,
		status, encryption_key, expected_owner FROM storage_connections WHERE tenant_id = $1 ORDER BY id`},
	{"datasets.jsonl", `SELECT id, name, description, tags, metadata, lifecycle_status, storage_connection_id, replica_connection_id,
		data_class, residency, escrow, vault_retention_days, current_version_id, parent_dataset_id, branch_name, created_by, created_at
		FROM datasets WHERE tenant_id = $1 ORDER BY id`},
	{"versions.jsonl", `SELECT id, dataset_id, version, member_count, total_bytes, manifest_sha256, manifest_format,
		parent_version_id, created_by, created_at FROM dataset_versions WHERE tenant_id = $1 ORDER BY dataset_id, version`},
	{"version-members.jsonl", `SELECT vm.version_id, vm.ordinal, vm.path, vm.object_id, vm.asset_id, vm.object_version, vm.size_bytes,
		vm.checksum_sha256, vm.label, vm.split, a.storage_connection_id, a.object_key, a.storage_version_id
		FROM dataset_version_members AS vm LEFT JOIN assets AS a ON a.id = vm.asset_id AND a.tenant_id = vm.tenant_id
		WHERE vm.tenant_id = $1 ORDER BY vm.version_id, vm.ordinal`},
	{"catalog.jsonl", `SELECT o.id AS object_id, o.dataset, o.path, a.id AS asset_id, a.version, a.status, a.file_name,
		a.content_type, a.size_bytes, a.checksum_sha256, a.storage_connection_id, a.object_key, a.storage_version_id,
		a.storage_etag, a.classification, a.published_at
		FROM assets AS a LEFT JOIN catalog_objects AS o ON o.id = a.object_id AND o.tenant_id = a.tenant_id
		WHERE a.tenant_id = $1 AND a.status IN ('AVAILABLE', 'SOFT_DELETED') ORDER BY o.dataset, o.path, a.version`},
	{"replicas.jsonl", `SELECT asset_id, connection_id, object_key, version_id, size_bytes, checksum_sha256, lock_mode, locked_until,
		verified_at FROM asset_replicas WHERE tenant_id = $1 ORDER BY asset_id`},
	{"retention.jsonl", `SELECT * FROM (
		SELECT 'policy' AS kind, id, scope, scope_key, version, min_retention_seconds, lock_mode, template, reason,
		       NULL::jsonb AS query, NULL::text AS matter, created_at AS at
		FROM retention_policies WHERE tenant_id = $1
		UNION ALL
		SELECT 'hold', id, scope, scope_key, NULL, NULL, NULL, NULL, reason, query, matter, placed_at
		FROM legal_holds WHERE tenant_id = $1 AND released_at IS NULL) AS r
		ORDER BY kind, id`},
	{"audit.jsonl", `SELECT seq, id, actor_type, actor_id, action, resource_type, resource_id, outcome, reason, correlation_id,
		source_ip, details, created_at, encode(prev_hash, 'hex') AS prev_hash, encode(hash, 'hex') AS hash
		FROM audit_events WHERE chain = $1 ORDER BY seq`},
}

const bundleReadme = `# Export bundle

Everything needed to rebuild this organization's catalog from its storage,
without the platform's database. The objects stay where they are (and at
their replicas); this bundle says what they are and proves it.

- connections.jsonl     the storage connections (no credentials)
- datasets.jsonl        datasets and their settings
- versions.jsonl        snapshots, with their manifest SHA-256
- version-members.jsonl every snapshot member: path, SHA-256, and where its bytes are
- catalog.jsonl         every object version: path, SHA-256, size, connection, key and provider version
- replicas.jsonl        verified copies at replica connections
- retention.jsonl       retention policies and active legal holds
- audit.jsonl           the audit log, hash-chained: each line's prev_hash is the hash before it
- SHA256SUMS            the SHA-256 of every file here

Rebuilding: see "Rebuilding from an export" in docs/copy-engine.md.
`

// exportBundle writes the bundle under the target connection's
// .exports folder. Each file is spooled to disk first, so its size is
// known before it is stored and memory stays flat.
func (s *Service) exportBundle(ctx context.Context, job model.CopyJob) error {
	target, err := s.deps.Router.Locate(ctx, job.TenantID, job.TargetConnectionID)
	if err != nil {
		return err
	}
	base := target.ReadyPrefix + "/" + job.TenantID + "/.exports/" + s.now().Format("20060102T150405Z") + "-" + job.ID[:8] + "/"
	var sums strings.Builder
	put := func(name string, write func(io.Writer) error) error {
		f, err := os.CreateTemp("", "osp-export-*")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		defer f.Close()
		h := sha256.New()
		buf := bufio.NewWriter(io.MultiWriter(f, h))
		if err := write(buf); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := buf.Flush(); err != nil {
			return err
		}
		size, err := f.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		contentType := "application/x-ndjson"
		if !strings.HasSuffix(name, ".jsonl") {
			contentType = "text/plain"
		}
		if _, err := target.Storage.WriteObject(ctx, target.ReadyBucket, base+name, f, size, contentType); err != nil {
			return fmt.Errorf("store %s: %w", name, err)
		}
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(h.Sum(nil)), name)
		return nil
	}
	for _, file := range bundleFiles {
		err := put(file.name, func(w io.Writer) error {
			enc := json.NewEncoder(w)
			return s.deps.Store.Rows(ctx, file.query, []any{job.TenantID}, func(row map[string]any) error { return enc.Encode(row) })
		})
		if err != nil {
			return err
		}
	}
	if err := put("README.md", func(w io.Writer) error { _, err := io.WriteString(w, bundleReadme); return err }); err != nil {
		return err
	}
	body := sums.String()
	_, err = target.Storage.WriteObject(ctx, target.ReadyBucket, base+"SHA256SUMS", strings.NewReader(body), int64(len(body)), "text/plain")
	return err
}
