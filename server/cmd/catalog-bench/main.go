// Command catalog-bench measures the catalog alone. It grows a synthetic
// tenant (catalogbench: metadata only, no object bytes) in steps and, at
// each size, measures listing latency through the real repository, the
// plans PostgreSQL chose, write throughput, and table, index and database
// sizes. Its numbers are catalog numbers, never storage throughput.
//
// It writes to the database it is given: use a dedicated local one
// (-db is required and must be on localhost).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/catalogbench"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

type options struct {
	db       string
	tenant   string
	steps    []int64
	versions int
	datasets int
	batch    int64
	reps     int
	keep     bool
	out      string
}

// query is one measured operation.
type query struct {
	Name         string   `json:"name"`
	P50Ms        float64  `json:"p50_ms"`
	P95Ms        float64  `json:"p95_ms"`
	MaxMs        float64  `json:"max_ms"`
	Rows         int      `json:"rows"`
	Indexes      []string `json:"indexes,omitempty"`
	SeqScan      bool     `json:"seq_scan"`
	ExplainMs    float64  `json:"explain_execution_ms,omitempty"`
	SharedHit    int64    `json:"shared_hit_blocks,omitempty"`
	SharedRead   int64    `json:"shared_read_blocks,omitempty"`
	PlanTopNodes string   `json:"plan,omitempty"`
}

// step is everything measured at one catalog size.
type step struct {
	Objects           int64            `json:"objects"`
	Versions          int64            `json:"versions"`
	PopulateSeconds   float64          `json:"populate_seconds"`
	ObjectsPerSecond  float64          `json:"objects_per_second"`
	VersionsPerSecond float64          `json:"versions_per_second"`
	TableBytes        map[string]int64 `json:"table_bytes"`
	IndexBytes        map[string]int64 `json:"index_bytes"`
	DatabaseBytes     int64            `json:"database_bytes"`
	BlocksRead        int64            `json:"blocks_read_during_step"`
	BlocksHit         int64            `json:"blocks_hit_during_step"`
	Queries           []query          `json:"queries"`
}

func main() {
	var o options
	var steps string
	flag.StringVar(&o.db, "db", "", "PostgreSQL URL of a dedicated local database (required; never read from the environment)")
	flag.StringVar(&o.tenant, "tenant", "", "synthetic tenant (default: catalog-bench-<time>)")
	flag.StringVar(&steps, "steps", "10000,100000,1000000", "catalog sizes to measure at, in objects")
	flag.IntVar(&o.versions, "versions", 1, "published versions per object")
	flag.IntVar(&o.datasets, "datasets", 4, "dataset namespaces the objects are spread over")
	flag.Int64Var(&o.batch, "batch", 10000, "objects per populate transaction")
	flag.IntVar(&o.reps, "reps", 30, "repetitions of each query")
	flag.BoolVar(&o.keep, "keep", false, "keep the synthetic tenant afterwards")
	flag.StringVar(&o.out, "out", "", "also write the results as JSON to this file")
	flag.Parse()
	if o.db == "" || !(strings.Contains(o.db, "@127.0.0.1") || strings.Contains(o.db, "@localhost")) {
		fmt.Fprintln(os.Stderr, "catalog-bench: -db must name a local database")
		os.Exit(2)
	}
	for _, s := range strings.Split(steps, ",") {
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil || n <= 0 {
			fmt.Fprintln(os.Stderr, "catalog-bench: -steps must be positive numbers")
			os.Exit(2)
		}
		o.steps = append(o.steps, n)
	}
	if o.tenant == "" {
		o.tenant = fmt.Sprintf("catalog-bench-%d", time.Now().Unix())
	}
	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "catalog-bench:", err)
		os.Exit(1)
	}
}

func run(o options) error {
	ctx := context.Background()
	db, err := postgres.Open(ctx, o.db)
	if err != nil {
		return err
	}
	defer db.Close()
	repo := postgres.NewCatalogRepository(db)
	if !o.keep {
		defer func() {
			if err := catalogbench.Remove(context.Background(), db.Pool, o.tenant); err != nil {
				fmt.Fprintln(os.Stderr, "catalog-bench: remove the synthetic tenant:", err)
			}
		}()
	}

	var results []step
	var populated int64
	base := time.Now().UTC()
	for _, target := range o.steps {
		if target <= populated {
			continue
		}
		hit0, read0, err := blocks(ctx, db)
		if err != nil {
			return err
		}
		res, err := catalogbench.Populate(ctx, db.Pool, catalogbench.Spec{
			TenantID: o.tenant, From: populated, Objects: target - populated, VersionsPerObject: o.versions,
			Datasets: o.datasets, BatchSize: o.batch, Base: base,
		}, func(done int64) { fmt.Fprintf(os.Stderr, "\rpopulated %d/%d", populated+done, target) })
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		populated = target
		if _, err := db.Pool.Exec(ctx, `ANALYZE catalog_objects; ANALYZE assets; ANALYZE upload_sessions`); err != nil {
			return err
		}

		s := step{
			Objects: populated, Versions: populated * int64(o.versions), PopulateSeconds: res.Duration.Seconds(),
			ObjectsPerSecond: float64(res.Objects) / res.Duration.Seconds(), VersionsPerSecond: float64(res.Versions) / res.Duration.Seconds(),
		}
		stats, err := repo.Stats(ctx)
		if err != nil {
			return err
		}
		s.TableBytes, s.IndexBytes = stats.TableBytes, stats.IndexBytes
		if err := db.Pool.QueryRow(ctx, `SELECT pg_database_size(current_database())`).Scan(&s.DatabaseBytes); err != nil {
			return err
		}
		if s.Queries, err = measure(ctx, db, repo, o, populated, base); err != nil {
			return err
		}
		hit1, read1, err := blocks(ctx, db)
		if err != nil {
			return err
		}
		s.BlocksHit, s.BlocksRead = hit1-hit0, read1-read0
		results = append(results, s)
		printStep(s)
	}

	if o.out != "" {
		raw, err := json.MarshalIndent(map[string]any{"tenant": o.tenant, "versions_per_object": o.versions,
			"datasets": o.datasets, "batch": o.batch, "repetitions": o.reps, "steps": results}, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(o.out, raw, 0o644)
	}
	return nil
}

func blocks(ctx context.Context, db *postgres.DB) (hit, read int64, err error) {
	err = db.Pool.QueryRow(ctx, `SELECT blks_hit, blks_read FROM pg_stat_database WHERE datname = current_database()`).Scan(&hit, &read)
	return hit, read, err
}

// measure runs each catalog read reps times and explains the listings.
func measure(ctx context.Context, db *postgres.DB, repo *postgres.CatalogRepository, o options, size int64, newest time.Time) ([]query, error) {
	// A continuation from the middle of the listing; finding the position
	// uses OFFSET once, outside the measurement.
	var mid model.CatalogPosition
	if err := db.Pool.QueryRow(ctx, `
		SELECT dataset, path_key FROM catalog_objects WHERE tenant_id = $1 AND current_asset_id IS NOT NULL
		ORDER BY dataset, path_key OFFSET $2 LIMIT 1
	`, o.tenant, size/2).Scan(&mid.Dataset, &mid.PathKey); err != nil {
		return nil, err
	}
	var sampleObject, samplePath, sampleDataset string
	if err := db.Pool.QueryRow(ctx, `
		SELECT id, path_key, dataset FROM catalog_objects WHERE tenant_id = $1 AND path_key = $2 AND dataset = $3
	`, o.tenant, mid.PathKey, mid.Dataset).Scan(&sampleObject, &samplePath, &sampleDataset); err != nil {
		return nil, err
	}

	base := model.CatalogFilter{TenantID: o.tenant, AnyDataset: true, Order: model.OrderByPath, Limit: 100}
	with := func(change func(f *model.CatalogFilter)) model.CatalogFilter {
		f := base
		change(&f)
		return f
	}
	listings := []struct {
		name string
		f    model.CatalogFilter
	}{
		{"first page (path order)", base},
		{"page from the middle (keyset)", with(func(f *model.CatalogFilter) { f.After = &mid })},
		{"one dataset", with(func(f *model.CatalogFilter) { f.AnyDataset, f.Dataset = false, "ds-1" })},
		{"prefix photos/2024/", with(func(f *model.CatalogFilter) { f.PathPrefix = "photos/2024/" })},
		{"metadata camera=c3", with(func(f *model.CatalogFilter) { f.Metadata = map[string]string{"camera": "c3"} })},
		{"metadata, rare: camera=c3 batch=13", with(func(f *model.CatalogFilter) { f.Metadata = map[string]string{"camera": "c3", "batch": "13"} })},
		{"content type and size (unindexed filters)", with(func(f *model.CatalogFilter) { f.ContentType, f.MinSize, f.MaxSize = "video/mp4", 5000, 6000 })},
		{"time order", with(func(f *model.CatalogFilter) { f.Order = model.OrderByUpdated })},
		{"time range", with(func(f *model.CatalogFilter) {
			// The newest updates: the generator's timestamps, not the clock's.
			f.Order, f.UpdatedAfter = model.OrderByUpdated, newest.Add(-5*time.Minute)
		})},
	}

	var out []query
	for _, l := range listings {
		var rows int
		q, err := timed(l.name, o.reps, func() error {
			page, err := repo.List(ctx, l.f, 30*time.Second)
			rows = len(page.Objects)
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", l.name, err)
		}
		q.Rows = rows
		if err := explain(ctx, db, l.f, &q); err != nil {
			return nil, fmt.Errorf("explain %s: %w", l.name, err)
		}
		out = append(out, q)
	}

	q, err := timed("version history (newest 100)", o.reps, func() error {
		_, err := repo.ListVersions(ctx, o.tenant, sampleObject, 0, 100)
		return err
	})
	if err != nil {
		return nil, err
	}
	out = append(out, q)
	q, err = timed("lookup by path", o.reps, func() error {
		_, err := repo.GetByPath(ctx, o.tenant, sampleDataset, samplePath)
		return err
	})
	if err != nil {
		return nil, err
	}
	return append(out, q), nil
}

func timed(name string, reps int, fn func() error) (query, error) {
	durations := make([]float64, 0, reps)
	for i := 0; i < reps; i++ {
		started := time.Now()
		if err := fn(); err != nil {
			return query{}, err
		}
		durations = append(durations, float64(time.Since(started).Microseconds())/1000)
	}
	sort.Float64s(durations)
	return query{Name: name, P50Ms: durations[len(durations)/2], P95Ms: durations[(len(durations)*95)/100], MaxMs: durations[len(durations)-1]}, nil
}

// explain runs EXPLAIN (ANALYZE, BUFFERS) of exactly the SQL the
// repository runs, and records the indexes, buffers and time.
func explain(ctx context.Context, db *postgres.DB, f model.CatalogFilter, q *query) error {
	sql, args, err := postgres.ListQuery(f)
	if err != nil {
		return err
	}
	var raw []byte
	if err := db.Pool.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+sql, args...).Scan(&raw); err != nil {
		return err
	}
	var doc []struct {
		Plan          map[string]any `json:"Plan"`
		ExecutionTime float64        `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	q.ExplainMs = doc[0].ExecutionTime
	root := doc[0].Plan
	q.SharedHit, q.SharedRead = number(root["Shared Hit Blocks"]), number(root["Shared Read Blocks"])
	var nodes []string
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		nodes = append(nodes, fmt.Sprint(node["Node Type"]))
		if node["Relation Name"] == "catalog_objects" && node["Node Type"] == "Seq Scan" {
			q.SeqScan = true
		}
		if name, ok := node["Index Name"].(string); ok {
			q.Indexes = append(q.Indexes, name)
		}
		if children, ok := node["Plans"].([]any); ok {
			for _, child := range children {
				walk(child.(map[string]any))
			}
		}
	}
	walk(root)
	q.PlanTopNodes = strings.Join(nodes, " > ")
	return nil
}

func number(v any) int64 {
	f, _ := v.(float64)
	return int64(f)
}

func printStep(s step) {
	fmt.Printf("\n== %d objects, %d versions\n", s.Objects, s.Versions)
	fmt.Printf("populate: %.1fs, %.0f objects/s, %.0f versions/s; database %d MiB; blocks hit %d, read %d\n",
		s.PopulateSeconds, s.ObjectsPerSecond, s.VersionsPerSecond, s.DatabaseBytes>>20, s.BlocksHit, s.BlocksRead)
	fmt.Printf("catalog_objects table %d MiB; indexes:", s.TableBytes["catalog_objects"]>>20)
	names := make([]string, 0, len(s.IndexBytes))
	for name := range s.IndexBytes {
		if strings.Contains(name, "catalog") || strings.Contains(name, "object") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Printf(" %s=%dMiB", name, s.IndexBytes[name]>>20)
	}
	fmt.Println()
	fmt.Printf("%-44s %8s %8s %8s %5s %9s  %s\n", "query", "p50 ms", "p95 ms", "max ms", "rows", "explain", "indexes")
	for _, q := range s.Queries {
		explain := ""
		if q.ExplainMs > 0 {
			explain = fmt.Sprintf("%.2fms", q.ExplainMs)
		}
		scan := strings.Join(q.Indexes, ",")
		if q.SeqScan {
			scan += " SEQ SCAN"
		}
		fmt.Printf("%-44s %8.2f %8.2f %8.2f %5d %9s  %s\n", q.Name, q.P50Ms, q.P95Ms, q.MaxMs, q.Rows, explain, scan)
	}
}
