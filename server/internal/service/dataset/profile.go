package dataset

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Column profiling: what each column of a version's tables holds, kept
// small enough to compare one version with another. Tables are CSV, TSV,
// JSON lines (each optionally gzipped) and Parquet.

const (
	profileSample   = 2000  // reservoir values kept per numeric column
	distinctCap     = 10000 // exact distinct counts up to this many values
	profileTop      = 20    // most common values listed
	profileColumns  = 500   // columns profiled per table
	profileTables   = 500   // tables profiled per version
	jsonLineMax     = 16 << 20
	parquetBatchRow = 8192
)

// Table formats.
const (
	formatCSV     = "csv"
	formatTSV     = "tsv"
	formatJSONL   = "jsonl"
	formatParquet = "parquet"
)

var errRowLimit = errors.New("row limit reached")

// tableFormat says how a member is read as a table, and whether it is
// gzipped; "" when it is not a table.
func tableFormat(p, contentType string) (string, bool) {
	name := strings.ToLower(p)
	gz := strings.HasSuffix(name, ".gz")
	name = strings.TrimSuffix(name, ".gz")
	switch path.Ext(name) {
	case ".csv":
		return formatCSV, gz
	case ".tsv", ".tab":
		return formatTSV, gz
	case ".jsonl", ".ndjson":
		return formatJSONL, gz
	case ".parquet", ".pq":
		if gz {
			return "", false
		}
		return formatParquet, false
	}
	switch strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0])) {
	case "text/csv":
		return formatCSV, gz
	case "text/tab-separated-values":
		return formatTSV, gz
	case "application/x-ndjson", "application/jsonl", "application/jsonlines":
		return formatJSONL, gz
	case "application/vnd.apache.parquet", "application/x-parquet":
		return formatParquet, false
	}
	return "", false
}

// tableName groups a member into a table: the directory of its parts, or
// the file itself when it sits at the root.
func tableName(p string) string {
	if dir := path.Dir(p); dir != "." && dir != "/" {
		return dir
	}
	return p
}

// value is one cell, typed.
type value struct {
	kind string // "" for null
	num  float64
	str  string
}

type columnStats struct {
	name   string
	kinds  map[string]int64
	count  int64
	values map[string]int64
	capped bool
	// Welford's running mean and variance over numbers and times.
	n, mean, m2, min, max float64
	sample                []float64
	seen                  int64
	rng                   *rand.Rand
	minLen, maxLen        int64
	hasLen                bool
}

type tableStats struct {
	name    string
	format  string
	files   int64
	rows    int64
	partial bool
	order   []string
	columns map[string]*columnStats
	seed    string
	maxRows int64
}

func newTable(name, format, seed string, maxRows int64) *tableStats {
	return &tableStats{name: name, format: format, columns: map[string]*columnStats{}, seed: seed, maxRows: maxRows}
}

func (t *tableStats) column(name string) *columnStats {
	if c, ok := t.columns[name]; ok {
		return c
	}
	if len(t.order) >= profileColumns {
		return nil
	}
	// Each column samples with its own seed, so a profile of the same
	// bytes comes out the same.
	h := fnv.New64a()
	h.Write([]byte(t.seed + "\x00" + t.name + "\x00" + name))
	seed := h.Sum64()
	c := &columnStats{name: name, kinds: map[string]int64{}, values: map[string]int64{}, rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
	t.columns[name] = c
	t.order = append(t.order, name)
	return c
}

// row counts one more row, refusing past the table's limit.
func (t *tableStats) row() error {
	if t.maxRows > 0 && t.rows >= t.maxRows {
		t.partial = true
		return errRowLimit
	}
	t.rows++
	return nil
}

func (c *columnStats) add(v value) {
	if c == nil || v.kind == "" {
		return
	}
	c.count++
	c.kinds[v.kind]++
	key := v.str
	if v.kind == model.KindNumber || v.kind == model.KindTime {
		key = strconv.FormatFloat(v.num, 'g', -1, 64)
		c.addNumber(v.num)
	}
	if v.kind == model.KindString {
		n := int64(len([]rune(v.str)))
		if !c.hasLen || n < c.minLen {
			c.minLen = n
		}
		if !c.hasLen || n > c.maxLen {
			c.maxLen = n
		}
		c.hasLen = true
	}
	if v.kind == model.KindNested {
		return
	}
	if _, ok := c.values[key]; ok || len(c.values) < distinctCap {
		c.values[key]++
	} else {
		c.capped = true
	}
}

func (c *columnStats) addNumber(x float64) {
	if c.n == 0 || x < c.min {
		c.min = x
	}
	if c.n == 0 || x > c.max {
		c.max = x
	}
	c.n++
	d := x - c.mean
	c.mean += d / c.n
	c.m2 += d * (x - c.mean)
	// Reservoir sampling (Algorithm R).
	c.seen++
	if len(c.sample) < profileSample {
		c.sample = append(c.sample, x)
	} else if j := c.rng.Int64N(c.seen); j < profileSample {
		c.sample[j] = x
	}
}

func (c *columnStats) profile(rows int64) model.ColumnProfile {
	out := model.ColumnProfile{Name: c.name, Count: c.count, Nulls: rows - c.count, Distinct: int64(len(c.values)), DistinctCapped: c.capped}
	out.Kind = model.KindEmpty
	for k, n := range c.kinds {
		switch {
		case out.Kind == model.KindEmpty:
			out.Kind = k
		case out.Kind != k && n > 0:
			out.Kind = model.KindMixed
		}
	}
	if c.n > 0 {
		lo, hi, mean := c.min, c.max, c.mean
		out.Min, out.Max, out.Mean = &lo, &hi, &mean
		sd := 0.0
		if c.n > 1 {
			sd = math.Sqrt(c.m2 / (c.n - 1))
		}
		out.StdDev = &sd
		out.Sample = slices.Clone(c.sample)
		slices.Sort(out.Sample)
	}
	if c.hasLen {
		lo, hi := c.minLen, c.maxLen
		out.MinLength, out.MaxLength = &lo, &hi
	}
	if out.Kind == model.KindString || out.Kind == model.KindBool || out.Kind == model.KindMixed {
		for v, n := range c.values {
			out.Top = append(out.Top, model.ValueCount{Value: v, Count: n})
		}
		slices.SortFunc(out.Top, func(a, b model.ValueCount) int {
			if a.Count != b.Count {
				return int(b.Count - a.Count)
			}
			return strings.Compare(a.Value, b.Value)
		})
		if len(out.Top) > profileTop {
			out.Top = out.Top[:profileTop]
		}
		// Every value once (identifiers): no value is common.
		if len(out.Top) > 0 && out.Top[0].Count == 1 {
			out.Top = nil
		}
	}
	return out
}

func (t *tableStats) profile() model.TableProfile {
	out := model.TableProfile{Table: t.name, Format: t.format, Files: t.files, Rows: t.rows, Partial: t.partial, Columns: []model.ColumnProfile{}}
	for _, name := range t.order {
		out.Columns = append(out.Columns, t.columns[name].profile(t.rows))
	}
	return out
}

// Text cells. Empty cells and the usual null spellings are nulls.
var nullWords = map[string]bool{"": true, "null": true, "NULL": true, "Null": true, "NA": true, "N/A": true, "NaN": true, "nan": true, "None": true}

func parseCell(s string) value {
	s = strings.TrimSpace(s)
	if nullWords[s] {
		return value{}
	}
	switch strings.ToLower(s) {
	case "true", "false":
		return value{kind: model.KindBool, str: strings.ToLower(s)}
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) {
		return value{kind: model.KindNumber, num: f}
	}
	return value{kind: model.KindString, str: s}
}

func profileDelimited(r io.Reader, sep rune, t *tableStats) error {
	cr := csv.NewReader(r)
	cr.Comma = sep
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	cr.ReuseRecord = true
	header, err := cr.Read()
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the header: %w", err)
	}
	names := make([]string, len(header))
	cols := make([]*columnStats, len(header))
	for i, h := range header {
		names[i] = strings.TrimSpace(strings.TrimPrefix(h, "\ufeff"))
		if names[i] == "" {
			names[i] = fmt.Sprintf("column_%d", i+1)
		}
		cols[i] = t.column(names[i])
	}
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("row %d: %w", t.rows+2, err)
		}
		if err := t.row(); err != nil {
			return err
		}
		for i, c := range cols {
			if i < len(rec) {
				c.add(parseCell(rec[i]))
			}
		}
	}
}

func jsonCell(v any) value {
	switch x := v.(type) {
	case nil:
		return value{}
	case bool:
		return value{kind: model.KindBool, str: strconv.FormatBool(x)}
	case json.Number:
		if f, err := x.Float64(); err == nil && !math.IsInf(f, 0) {
			return value{kind: model.KindNumber, num: f}
		}
		return value{kind: model.KindString, str: x.String()}
	case string:
		return value{kind: model.KindString, str: x}
	default:
		return value{kind: model.KindNested}
	}
}

func profileJSONLines(r io.Reader, t *tableStats) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), jsonLineMax)
	line := 0
	for sc.Scan() {
		line++
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil {
			return fmt.Errorf("line %d is not a JSON object: %w", line, err)
		}
		if err := t.row(); err != nil {
			return err
		}
		// Keys in a stable order, so new columns are added the same way
		// every time.
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			t.column(k).add(jsonCell(obj[k]))
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("line %d: %w", line+1, err)
	}
	return nil
}

// arrowCell reads row i of a column.
func arrowCell(arr arrow.Array, i int) value {
	if arr.IsNull(i) {
		return value{}
	}
	switch a := arr.(type) {
	case *array.Int8:
		return value{kind: model.KindNumber, num: float64(a.Value(i))}
	case *array.Int16:
		return value{kind: model.KindNumber, num: float64(a.Value(i))}
	case *array.Int32:
		return value{kind: model.KindNumber, num: float64(a.Value(i))}
	case *array.Int64:
		return value{kind: model.KindNumber, num: float64(a.Value(i))}
	case *array.Uint8:
		return value{kind: model.KindNumber, num: float64(a.Value(i))}
	case *array.Uint16:
		return value{kind: model.KindNumber, num: float64(a.Value(i))}
	case *array.Uint32:
		return value{kind: model.KindNumber, num: float64(a.Value(i))}
	case *array.Uint64:
		return value{kind: model.KindNumber, num: float64(a.Value(i))}
	case *array.Float16:
		return value{kind: model.KindNumber, num: float64(a.Value(i).Float32())}
	case *array.Float32:
		f := float64(a.Value(i))
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return value{}
		}
		return value{kind: model.KindNumber, num: f}
	case *array.Float64:
		f := a.Value(i)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return value{}
		}
		return value{kind: model.KindNumber, num: f}
	case *array.Decimal128:
		scale := a.DataType().(*arrow.Decimal128Type).Scale
		return value{kind: model.KindNumber, num: a.Value(i).ToFloat64(scale)}
	case *array.Boolean:
		return value{kind: model.KindBool, str: strconv.FormatBool(a.Value(i))}
	case *array.String:
		return value{kind: model.KindString, str: a.Value(i)}
	case *array.LargeString:
		return value{kind: model.KindString, str: a.Value(i)}
	case *array.Timestamp:
		unit := a.DataType().(*arrow.TimestampType).Unit
		return value{kind: model.KindTime, num: float64(a.Value(i).ToTime(unit).UnixNano()) / 1e9}
	case *array.Date32:
		return value{kind: model.KindTime, num: float64(a.Value(i).ToTime().Unix())}
	case *array.Date64:
		return value{kind: model.KindTime, num: float64(a.Value(i).ToTime().Unix())}
	case *array.Dictionary:
		return arrowCell(a.Dictionary(), a.GetValueIndex(i))
	default:
		return value{kind: model.KindNested}
	}
}

func profileParquet(ctx context.Context, f *os.File, t *tableStats) error {
	pf, err := file.NewParquetReader(f)
	if err != nil {
		return fmt.Errorf("not a Parquet file: %w", err)
	}
	defer pf.Close()
	fr, err := pqarrow.NewFileReader(pf, pqarrow.ArrowReadProperties{BatchSize: parquetBatchRow}, memory.DefaultAllocator)
	if err != nil {
		return err
	}
	rr, err := fr.GetRecordReader(ctx, nil, nil)
	if err != nil {
		return err
	}
	defer rr.Release()
	schema := rr.Schema()
	cols := make([]*columnStats, schema.NumFields())
	for i, field := range schema.Fields() {
		cols[i] = t.column(field.Name)
	}
	for rr.Next() {
		rec := rr.RecordBatch()
		for row := 0; row < int(rec.NumRows()); row++ {
			if err := t.row(); err != nil {
				return err
			}
			for i, c := range cols {
				if c != nil {
					c.add(arrowCell(rec.Column(i), row))
				}
			}
		}
	}
	return rr.Err()
}

// profileStream reads one file of a table. Parquet needs random access,
// so it is spooled to a temporary file first.
func profileStream(ctx context.Context, body io.Reader, format string, gz bool, t *tableStats, tmpDir string) error {
	t.files++
	if gz {
		zr, err := gzip.NewReader(body)
		if err != nil {
			return fmt.Errorf("not gzip: %w", err)
		}
		defer zr.Close()
		body = zr
	}
	var err error
	switch format {
	case formatCSV:
		err = profileDelimited(body, ',', t)
	case formatTSV:
		err = profileDelimited(body, '\t', t)
	case formatJSONL:
		err = profileJSONLines(body, t)
	case formatParquet:
		var tmp *os.File
		tmp, err = os.CreateTemp(tmpDir, "profile-*.parquet")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		defer tmp.Close()
		if _, err = io.Copy(tmp, body); err != nil {
			return err
		}
		err = profileParquet(ctx, tmp, t)
	default:
		err = fmt.Errorf("unknown format %q", format)
	}
	if errors.Is(err, errRowLimit) {
		return nil
	}
	return err
}
