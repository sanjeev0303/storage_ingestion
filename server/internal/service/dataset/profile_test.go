package dataset

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

func profileOf(t *testing.T, name, body string, gz bool) model.TableProfile {
	t.Helper()
	format, isGz := tableFormat(name, "")
	if isGz != gz {
		t.Fatalf("%s: gzip %v", name, isGz)
	}
	var r = strings.NewReader(body)
	tab := newTable(tableName(name), format, "v1", 0)
	if gz {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		zw.Write([]byte(body))
		zw.Close()
		if err := profileStream(context.Background(), &buf, format, true, tab, t.TempDir()); err != nil {
			t.Fatal(err)
		}
		return tab.profile()
	}
	if err := profileStream(context.Background(), r, format, false, tab, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	return tab.profile()
}

func column(t *testing.T, p model.TableProfile, name string) model.ColumnProfile {
	t.Helper()
	for _, c := range p.Columns {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no column %s in %+v", name, p.Columns)
	return model.ColumnProfile{}
}

func TestProfileCSV(t *testing.T) {
	p := profileOf(t, "scenes/part-1.csv.gz", "\ufeffid,band,cloud,ok\n1,B04,0.5,true\n2,B04,,false\n3,B08,NA,true\n4,B04,2.5,TRUE\n", true)
	if p.Table != "scenes" || p.Rows != 4 || p.Format != formatCSV {
		t.Fatalf("table %+v", p)
	}
	cloud := column(t, p, "cloud")
	if cloud.Kind != model.KindNumber || cloud.Count != 2 || cloud.Nulls != 2 || *cloud.Mean != 1.5 || *cloud.Min != 0.5 || *cloud.Max != 2.5 {
		t.Fatalf("cloud %+v", cloud)
	}
	band := column(t, p, "band")
	if band.Kind != model.KindString || band.Distinct != 2 || band.Top[0] != (model.ValueCount{Value: "B04", Count: 3}) {
		t.Fatalf("band %+v", band)
	}
	if ok := column(t, p, "ok"); ok.Kind != model.KindBool || ok.Top[0].Value != "true" || ok.Top[0].Count != 3 {
		t.Fatalf("ok %+v", ok)
	}
}

func TestProfileJSONLines(t *testing.T) {
	p := profileOf(t, "events.jsonl", `{"user":"a","n":1,"tags":["x"]}
{"user":"b","n":"two"}

{"user":"a","extra":null}
`, false)
	if p.Table != "events.jsonl" || p.Rows != 3 {
		t.Fatalf("table %+v", p)
	}
	if n := column(t, p, "n"); n.Kind != model.KindMixed || n.Count != 2 || n.Nulls != 1 {
		t.Fatalf("n %+v", n)
	}
	if tags := column(t, p, "tags"); tags.Kind != model.KindNested || tags.Count != 1 {
		t.Fatalf("tags %+v", tags)
	}
	if extra := column(t, p, "extra"); extra.Kind != model.KindEmpty || extra.Nulls != 3 {
		t.Fatalf("extra %+v", extra)
	}
	tab := newTable("bad", formatJSONL, "v1", 0)
	if err := profileStream(context.Background(), strings.NewReader("[1,2]\n"), formatJSONL, false, tab, ""); err == nil {
		t.Fatal("a line that is not an object was accepted")
	}
}

func TestProfileParquet(t *testing.T) {
	schema := arrow.NewSchema([]arrow.Field{
		{Name: "reflectance", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
		{Name: "baseline", Type: arrow.BinaryTypes.String},
		{Name: "taken", Type: &arrow.TimestampType{Unit: arrow.Millisecond}},
	}, nil)
	b := array.NewRecordBuilder(memory.DefaultAllocator, schema)
	defer b.Release()
	for i := 0; i < 100; i++ {
		if i%10 == 0 {
			b.Field(0).(*array.Float64Builder).AppendNull()
		} else {
			b.Field(0).(*array.Float64Builder).Append(float64(i))
		}
		b.Field(1).(*array.StringBuilder).Append("04.00")
		b.Field(2).(*array.TimestampBuilder).Append(arrow.Timestamp(1_700_000_000_000 + int64(i)*1000))
	}
	rec := b.NewRecordBatch()
	defer rec.Release()
	tbl := array.NewTableFromRecords(schema, []arrow.RecordBatch{rec})
	defer tbl.Release()
	path := filepath.Join(t.TempDir(), "s2.parquet")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := pqarrow.WriteTable(tbl, f, 32, nil, pqarrow.DefaultWriterProps()); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	tab := newTable("tiles", formatParquet, "v1", 0)
	if err := profileStream(context.Background(), bytes.NewReader(data), formatParquet, false, tab, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	p := tab.profile()
	if p.Rows != 100 {
		t.Fatalf("rows %d", p.Rows)
	}
	r := column(t, p, "reflectance")
	if r.Kind != model.KindNumber || r.Nulls != 10 || r.Count != 90 || len(r.Sample) != 90 {
		t.Fatalf("reflectance %+v", r)
	}
	if taken := column(t, p, "taken"); taken.Kind != model.KindTime || *taken.Min != 1_700_000_000 {
		t.Fatalf("taken %+v", taken)
	}
	if bl := column(t, p, "baseline"); bl.Distinct != 1 || bl.Top[0].Value != "04.00" {
		t.Fatalf("baseline %+v", bl)
	}
}

func TestProfileStopsAtTheRowLimitAndSamplesTheSameWay(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("x\n")
	for i := 0; i < 10000; i++ {
		fmt.Fprintf(&sb, "%d\n", i)
	}
	run := func(limit int64) model.TableProfile {
		tab := newTable("t.csv", formatCSV, "v1", limit)
		if err := profileStream(context.Background(), strings.NewReader(sb.String()), formatCSV, false, tab, ""); err != nil {
			t.Fatal(err)
		}
		return tab.profile()
	}
	if p := run(500); p.Rows != 500 || !p.Partial {
		t.Fatalf("limit: %d rows, partial %v", p.Rows, p.Partial)
	}
	a, b := run(0), run(0)
	if len(a.Columns[0].Sample) != profileSample || !reflect.DeepEqual(a, b) {
		t.Fatal("two profiles of the same bytes differ")
	}
}

func numbers(from, to int) string {
	var sb strings.Builder
	sb.WriteString("v,city,id\n")
	for i := from; i < to; i++ {
		city := "north"
		if i%3 == 0 {
			city = "south"
		}
		fmt.Fprintf(&sb, "%d,%s,id-%d\n", i, city, i)
	}
	return sb.String()
}

func TestDrift(t *testing.T) {
	lim := model.DriftLimits{MaxPSI: 0.2, MaxNullChange: 0.1, MaxRowChange: 0.5}
	base := profileOf(t, "a.csv", numbers(0, 1000), false)
	same := profileOf(t, "a.csv", numbers(0, 1000), false)
	r := compareProfiles("b", "c", []model.TableProfile{base}, []model.TableProfile{same}, lim)
	if r.Breaches != 0 || *r.Tables[0].Columns[0].PSI > 0.001 {
		t.Fatalf("same data drifted: %+v", r)
	}
	if id := r.Tables[0].Columns[2]; id.PSI != nil || !strings.Contains(id.NotCompared, "identifiers") {
		t.Fatalf("identifiers compared: %+v", id)
	}

	shifted := profileOf(t, "a.csv", numbers(600, 1600), false)
	r = compareProfiles("b", "c", []model.TableProfile{base}, []model.TableProfile{shifted}, lim)
	v := r.Tables[0].Columns[0]
	if v.PSI == nil || *v.PSI < 1 || v.Breached[0] != "distribution" || r.Breaches == 0 {
		t.Fatalf("shift not found: %+v", v)
	}

	// 0..299 against 0..999: fewer rows, and the values moved too.
	halved := profileOf(t, "a.csv", numbers(0, 300), false)
	gone := compareProfiles("b", "c", []model.TableProfile{base, {Table: "old.csv"}}, []model.TableProfile{halved}, lim)
	if gone.Tables[0].Breached[0] != "rows" || gone.NotCompared[0] != "old.csv (gone)" || gone.Breaches != 3 {
		t.Fatalf("rows and a lost table: %+v", gone)
	}
}

func TestTableFormat(t *testing.T) {
	cases := map[string]string{"a/b.CSV": formatCSV, "x.tsv.gz": formatTSV, "x.ndjson": formatJSONL, "x.parquet": formatParquet,
		"x.parquet.gz": "", "x.png": "", "x.json": ""}
	for name, want := range cases {
		if got, _ := tableFormat(name, ""); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	if got, _ := tableFormat("blob", "text/csv; charset=utf-8"); got != formatCSV {
		t.Errorf("content type: %q", got)
	}
}
