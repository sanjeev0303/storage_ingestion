package adoption

import (
	"context"
	"errors"
	"strconv"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
)

// inventoryParquet reads a Parquet data file of an S3 Inventory report. Its
// columns are snake_case (key, size, e_tag, is_latest, is_delete_marker),
// and its keys are not URL-encoded.
func inventoryParquet(ctx context.Context, data parquet.ReaderAtSeeker, emit func(inventoryRow) error) error {
	pf, err := file.NewParquetReader(data)
	if err != nil {
		return err
	}
	defer pf.Close()
	fr, err := pqarrow.NewFileReader(pf, pqarrow.ArrowReadProperties{BatchSize: 4096}, memory.DefaultAllocator)
	if err != nil {
		return err
	}
	rr, err := fr.GetRecordReader(ctx, nil, nil)
	if err != nil {
		return err
	}
	defer rr.Release()
	index := map[string]int{}
	for i, field := range rr.Schema().Fields() {
		index[field.Name] = i
	}
	if _, ok := index["key"]; !ok {
		return errors.New("the inventory file has no key column")
	}
	for rr.Next() {
		rec := rr.RecordBatch()
		cell := func(name string, row int) string {
			i, ok := index[name]
			if !ok {
				return ""
			}
			return arrowString(rec.Column(i), row)
		}
		for row := 0; row < int(rec.NumRows()); row++ {
			err := emit(inventoryRow{key: cell("key", row), size: cell("size", row), etag: cell("e_tag", row),
				isLatest: cell("is_latest", row), isDeleteMarker: cell("is_delete_marker", row)})
			if err != nil {
				return err
			}
		}
	}
	return rr.Err()
}

// arrowString is one cell as text; nulls and other types are empty.
func arrowString(col arrow.Array, i int) string {
	if col.IsNull(i) {
		return ""
	}
	switch a := col.(type) {
	case *array.String:
		return a.Value(i)
	case *array.LargeString:
		return a.Value(i)
	case *array.Binary:
		return string(a.Value(i))
	case *array.Int64:
		return strconv.FormatInt(a.Value(i), 10)
	case *array.Int32:
		return strconv.FormatInt(int64(a.Value(i)), 10)
	case *array.Boolean:
		return strconv.FormatBool(a.Value(i))
	case *array.Dictionary:
		return arrowString(a.Dictionary(), a.GetValueIndex(i))
	}
	return ""
}
