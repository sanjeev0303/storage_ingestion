package integration

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/platform/migrate"
	"github.com/sanjeev0303/object-storage-platform/migrations"
)

// schemaOf describes a database's tables, columns, indexes and
// constraints, to compare two databases migrated different ways.
func schemaOf(t *testing.T, ctx context.Context, conn *pgx.Conn) []string {
	t.Helper()
	rows, err := conn.Query(ctx, `
		SELECT 'column ' || table_name || '.' || column_name || ' ' || data_type || ' ' || is_nullable || ' ' || COALESCE(column_default, '')
		FROM information_schema.columns WHERE table_schema = 'public'
		UNION ALL
		SELECT 'index ' || indexdef FROM pg_indexes WHERE schemaname = 'public'
		UNION ALL
		SELECT 'constraint ' || conrelid::regclass || ' ' || conname || ' ' || pg_get_constraintdef(oid)
		FROM pg_constraint WHERE connamespace = 'public'::regnamespace
		UNION ALL
		SELECT 'trigger ' || event_object_table || ' ' || trigger_name || ' ' || event_manipulation
		FROM information_schema.triggers WHERE trigger_schema = 'public'
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		out = append(out, line)
	}
	return out
}

func diff(a, b []string) (onlyA, onlyB []string) {
	inA, inB := map[string]bool{}, map[string]bool{}
	for _, s := range a {
		inA[s] = true
	}
	for _, s := range b {
		inB[s] = true
	}
	for _, s := range a {
		if !inB[s] {
			onlyA = append(onlyA, s)
		}
	}
	for _, s := range b {
		if !inA[s] {
			onlyB = append(onlyB, s)
		}
	}
	return onlyA, onlyB
}

// A rehearsal of a production migration: from an empty database to the
// newest schema, identical to the test database migrated file by file over
// time; run again, nothing happens; the newest release rolls back and
// forward again; an older one refuses to roll back.
func TestMigrationsFromScratchAndRollback(t *testing.T) {
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	name := fmt.Sprintf("osp_migrate_rehearsal_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	cfg, err := pgx.ParseConfig(base)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = name
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	m := &migrate.Migrator{Conn: conn, Up: migrations.Up, Down: migrations.Down}

	applied, err := m.ApplyUp(ctx)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := m.Status(ctx)
	if len(applied) != len(all) || len(applied) < 38 {
		t.Fatalf("from scratch: %d of %d applied", len(applied), len(all))
	}
	if again, err := m.ApplyUp(ctx); err != nil || len(again) != 0 {
		t.Fatalf("a second run applied %v (%v)", again, err)
	}

	// Identical to the test database, migrated by scripts/migrate.ps1.
	fresh := schemaOf(t, ctx, conn)
	if len(fresh) < 300 {
		t.Fatalf("only %d schema facts: the comparison would prove nothing", len(fresh))
	}
	reference := schemaOf(t, ctx, admin)
	if onlyFresh, onlyOld := diff(fresh, reference); len(onlyFresh)+len(onlyOld) > 0 {
		t.Fatalf("schemas differ.\nonly from scratch:\n  %s\nonly in the test database:\n  %s",
			strings.Join(onlyFresh, "\n  "), strings.Join(onlyOld, "\n  "))
	}

	// The newest release rolls back...
	want := []string{"000040_erasure.sql", "000039_quality.sql", "000038_evidence.sql", "000037_tiering_costs.sql", "000036_pack_streaming.sql", "000035_copy_engine.sql",
		"000034_residency_keys.sql", "000033_storage_posture.sql", "000032_access_governance.sql", "000031_adoption.sql",
		"000030_governance.sql", "000029_content_checks.sql", "000028_lock_sync.sql", "000027_training_runs.sql"}
	undone, err := m.Rollback(ctx, len(want))
	if err != nil || !slices.Equal(undone, want) {
		t.Fatalf("rollback: %v %v", undone, err)
	}
	var tables int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
		WHERE table_name IN ('erasure_requests', 'erasure_items', 'dataset_quality', 'version_profiles', 'leakage_checks', 'evidence_settings', 'audit_anchors', 'audit_reviews', 'tier_rules', 'tier_moves', 'archive_restores', 'cost_budgets', 'repack_requests', 'run_checkpoints', 'run_verifications', 'tenant_packing', 'copy_jobs', 'asset_replicas', 'connection_posture', 'storage_drift_findings', 'api_tokens', 'security_alerts', 'adoption_jobs', 'governance_approvals', 'disposition_certificates', 'content_findings', 'lock_sync_requests', 'training_runs', 'models')`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("after rollback, %d of the release's tables remain (%v)", tables, err)
	}
	// ...but not past it: 000026 has no rollback file, and nothing changes.
	if _, err := m.Rollback(ctx, 1); err == nil || !strings.Contains(err.Error(), "restore a backup") {
		t.Fatalf("rolling back a migration without a rollback file: %v", err)
	}
	// And forward again, to the same schema.
	if again, err := m.ApplyUp(ctx); err != nil || len(again) != len(want) {
		t.Fatalf("forward again: %v %v", again, err)
	}
	if onlyA, onlyB := diff(schemaOf(t, ctx, conn), fresh); len(onlyA)+len(onlyB) > 0 {
		t.Fatalf("after a rollback and forward again the schema changed: %v %v", onlyA, onlyB)
	}
}
