package adoption

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
)

func TestS3InventoryParquet(t *testing.T) {
	schema := arrow.NewSchema([]arrow.Field{
		{Name: "bucket", Type: arrow.BinaryTypes.String},
		{Name: "key", Type: arrow.BinaryTypes.String},
		{Name: "size", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
		{Name: "e_tag", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "is_latest", Type: arrow.FixedWidthTypes.Boolean},
		{Name: "is_delete_marker", Type: arrow.FixedWidthTypes.Boolean},
	}, nil)
	b := array.NewRecordBuilder(memory.DefaultAllocator, schema)
	defer b.Release()
	rows := []struct {
		key, etag      string
		size           int64
		latest, marker bool
	}{
		{"run 1/a.fastq", "900150983cd24fb0d6963f7d28e17f72", 120, true, false}, // keys are not URL-encoded
		{"run 1/old.fastq", "aa", 10, false, false},
		{"run 1/big.bam", "abcdef0123456789abcdef0123456789-3", 999, true, false},
		{"run 1/gone.txt", "", 0, true, true},
		{"run 1/100%.txt", "", 3, true, false},
	}
	for _, r := range rows {
		b.Field(0).(*array.StringBuilder).Append("lab")
		b.Field(1).(*array.StringBuilder).Append(r.key)
		b.Field(2).(*array.Int64Builder).Append(r.size)
		b.Field(3).(*array.StringBuilder).Append(r.etag)
		b.Field(4).(*array.BooleanBuilder).Append(r.latest)
		b.Field(5).(*array.BooleanBuilder).Append(r.marker)
	}
	rec := b.NewRecordBatch()
	defer rec.Release()
	tbl := array.NewTableFromRecords(schema, []arrow.RecordBatch{rec})
	defer tbl.Release()
	var data bytes.Buffer
	if err := pqarrow.WriteTable(tbl, &data, 64, nil, pqarrow.DefaultWriterProps()); err != nil {
		t.Fatal(err)
	}
	sum := md5.Sum(data.Bytes())
	manifest, _ := json.Marshal(map[string]any{
		"sourceBucket": "lab", "fileFormat": "Parquet",
		"fileSchema": "message s3.inventory { required binary bucket (STRING); required binary key (STRING); }",
		"files":      []map[string]any{{"key": "inv/data/1.parquet", "size": data.Len(), "MD5checksum": hex.EncodeToString(sum[:])}},
	})
	r := files{"inv/manifest.json": manifest, "inv/data/1.parquet": data.Bytes()}
	got := collectEntries(t, func(e *emitter) error { return readInventory(context.Background(), r, "inv/manifest.json", "lab", e) })
	if len(got) != 3 {
		t.Fatalf("%d entries: %+v", len(got), got)
	}
	if got[0].ObjectKey != "run 1/a.fastq" || got[0].SizeBytes != 120 || got[0].Expected["md5"] != "900150983cd24fb0d6963f7d28e17f72" {
		t.Fatalf("first: %+v", got[0])
	}
	if got[1].ObjectKey != "run 1/big.bam" || len(got[1].Expected) != 0 {
		t.Fatalf("a multipart ETag is not a digest: %+v", got[1])
	}
	if got[2].ObjectKey != "run 1/100%.txt" || got[2].Code != "" {
		t.Fatalf("a percent sign in a Parquet key is literal: %+v", got[2])
	}

	manifest, _ = json.Marshal(map[string]any{"sourceBucket": "lab", "fileFormat": "ORC", "files": []any{}})
	r["inv/manifest.json"] = manifest
	if err := readInventory(context.Background(), r, "inv/manifest.json", "lab", &emitter{}); err == nil {
		t.Fatal("an ORC report was accepted")
	}
}
