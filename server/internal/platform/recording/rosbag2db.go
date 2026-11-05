package recording

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// ParseRosbag2DB reads a rosbag2 SQLite storage file (.db3): its topics
// table, and from the messages table each message's topic and timestamp.
// It needs no metadata.yaml, which a recorder that crashed never wrote.
//
// It is a read-only walk of SQLite's file format (sqlite.org/fileformat2),
// enough for rowid tables: B-tree pages, records, overflow pages. Message
// payloads are not read beyond their first columns.
//
// ponytail: every leaf page of the messages table is read, so the whole
// bag is read once; a bag's data lives in those pages. Index the time range
// from the first and last leaf only if full reads become too slow.
func ParseRosbag2DB(r io.ReaderAt, size int64) (Summary, error) {
	out := Summary{Kind: KindRosbag2}
	db, err := openSQLite(r, size)
	if err != nil {
		return out, err
	}
	tables, err := db.schema()
	if err != nil {
		return out, err
	}
	topicsTable, ok1 := tables["topics"]
	messagesTable, ok2 := tables["messages"]
	if !ok1 || !ok2 {
		return out, errors.New("not a rosbag2 database: no topics and messages tables")
	}

	topicCols := columnIndexes(topicsTable.sql, "id", "name", "type", "serialization_format")
	if topicCols["name"] < 0 {
		return out, errors.New("the topics table has no name column")
	}
	type topicRow struct{ name, schema, encoding string }
	topics := map[int64]topicRow{}
	err = db.walk(topicsTable.root, func(rowid int64, rec []any) error {
		id := rowid
		if v, ok := column(rec, topicCols["id"]).(int64); ok {
			id = v
		}
		name, _ := column(rec, topicCols["name"]).(string)
		schema, _ := column(rec, topicCols["type"]).(string)
		encoding, _ := column(rec, topicCols["serialization_format"]).(string)
		topics[id] = topicRow{name, schema, encoding}
		return nil
	}, -1)
	if err != nil {
		return out, fmt.Errorf("read topics: %w", err)
	}

	msgCols := columnIndexes(messagesTable.sql, "topic_id", "timestamp")
	if msgCols["topic_id"] < 0 || msgCols["timestamp"] < 0 {
		return out, errors.New("the messages table has no topic_id or timestamp column")
	}
	need := max(msgCols["topic_id"], msgCols["timestamp"])
	counts := map[int64]int64{}
	first := true
	err = db.walk(messagesTable.root, func(_ int64, rec []any) error {
		topic, _ := column(rec, msgCols["topic_id"]).(int64)
		ts, ok := column(rec, msgCols["timestamp"]).(int64)
		if !ok {
			return nil
		}
		counts[topic]++
		out.MessageCount++
		if first || ts < out.StartNS {
			out.StartNS = ts
		}
		if first || ts > out.EndNS {
			out.EndNS = ts
		}
		first = false
		return nil
	}, need)
	if err != nil {
		return out, fmt.Errorf("read messages: %w", err)
	}
	for id, t := range topics {
		out.Topics = append(out.Topics, Topic{Topic: t.name, Schema: t.schema, Encoding: t.encoding, MessageCount: counts[id]})
	}
	sort.Slice(out.Topics, func(a, b int) bool { return out.Topics[a].Topic < out.Topics[b].Topic })
	return out, nil
}

func column(rec []any, i int) any {
	if i < 0 || i >= len(rec) {
		return nil
	}
	return rec[i]
}

// columnIndexes finds where named columns sit in a CREATE TABLE statement.
// A missing column is -1.
func columnIndexes(sql string, names ...string) map[string]int {
	out := map[string]int{}
	for _, n := range names {
		out[n] = -1
	}
	open, close := strings.Index(sql, "("), strings.LastIndex(sql, ")")
	if open < 0 || close <= open {
		return out
	}
	depth, start, index := 0, open+1, 0
	body := sql[:close]
	for i := open + 1; i <= len(body); i++ {
		if i < len(body) {
			switch body[i] {
			case '(':
				depth++
				continue
			case ')':
				depth--
				continue
			case ',':
				if depth > 0 {
					continue
				}
			default:
				continue
			}
		}
		def := strings.Fields(strings.TrimSpace(body[start:i]))
		if len(def) > 0 {
			name := strings.Trim(def[0], "\"`[]")
			upper := strings.ToUpper(name)
			if upper != "PRIMARY" && upper != "UNIQUE" && upper != "CONSTRAINT" && upper != "FOREIGN" && upper != "CHECK" {
				if _, wanted := out[name]; wanted {
					out[name] = index
				}
				index++
			}
		}
		start = i + 1
	}
	return out
}

// ─── A read-only SQLite reader ────────────────────────────────────────

type sqliteDB struct {
	r        io.ReaderAt
	size     int64
	pageSize int64
	usable   int64
	pages    int64
}

type sqliteTable struct {
	root int64
	sql  string
}

func openSQLite(r io.ReaderAt, size int64) (*sqliteDB, error) {
	h := make([]byte, 100)
	if size < 100 {
		return nil, errors.New("not a SQLite database")
	}
	if _, err := r.ReadAt(h, 0); err != nil {
		return nil, err
	}
	if string(h[:16]) != "SQLite format 3\x00" {
		return nil, errors.New("not a SQLite database")
	}
	ps := int64(binary.BigEndian.Uint16(h[16:18]))
	if ps == 1 {
		ps = 65536
	}
	if ps < 512 || ps&(ps-1) != 0 {
		return nil, errors.New("invalid SQLite page size")
	}
	return &sqliteDB{r: r, size: size, pageSize: ps, usable: ps - int64(h[20]), pages: size / ps}, nil
}

func (db *sqliteDB) page(n int64) ([]byte, error) {
	if n < 1 || n > db.pages {
		return nil, fmt.Errorf("page %d is outside the file (it may be cut short)", n)
	}
	b := make([]byte, db.pageSize)
	if _, err := db.r.ReadAt(b, (n-1)*db.pageSize); err != nil {
		return nil, err
	}
	return b, nil
}

// schema reads sqlite_master, whose root is page 1.
func (db *sqliteDB) schema() (map[string]sqliteTable, error) {
	out := map[string]sqliteTable{}
	err := db.walk(1, func(_ int64, rec []any) error {
		kind, _ := column(rec, 0).(string)
		name, _ := column(rec, 1).(string)
		root, _ := column(rec, 3).(int64)
		sql, _ := column(rec, 4).(string)
		if kind == "table" {
			out[name] = sqliteTable{root: root, sql: sql}
		}
		return nil
	}, -1)
	return out, err
}

// walk visits every row of a table B-tree in rowid order. need is the last
// column the caller reads (-1 for all), so large payloads' overflow pages
// are followed only as far as those columns.
func (db *sqliteDB) walk(root int64, visit func(rowid int64, rec []any) error, need int) error {
	stack := []int64{root}
	seen := map[int64]bool{}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[n] {
			return errors.New("the B-tree loops back on itself")
		}
		seen[n] = true
		p, err := db.page(n)
		if err != nil {
			return err
		}
		hdr := 0
		if n == 1 {
			hdr = 100
		}
		kind := p[hdr]
		cells := int(binary.BigEndian.Uint16(p[hdr+3:]))
		switch kind {
		case 0x05: // interior table page: children left to right, then the right-most
			var children []int64
			for i := 0; i < cells; i++ {
				off := int(binary.BigEndian.Uint16(p[hdr+12+2*i:]))
				if off+4 > len(p) {
					return errors.New("a cell lies outside its page")
				}
				children = append(children, int64(binary.BigEndian.Uint32(p[off:])))
			}
			children = append(children, int64(binary.BigEndian.Uint32(p[hdr+8:])))
			for i := len(children) - 1; i >= 0; i-- {
				stack = append(stack, children[i])
			}
		case 0x0D: // leaf table page
			for i := 0; i < cells; i++ {
				off := int(binary.BigEndian.Uint16(p[hdr+8+2*i:]))
				rowid, rec, err := db.leafCell(p, off, need)
				if err != nil {
					return err
				}
				if err := visit(rowid, rec); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("page %d is not a table B-tree page", n)
		}
	}
	return nil
}

// leafCell reads one row: payload size, rowid, then the record, following
// overflow pages only as far as the needed columns.
func (db *sqliteDB) leafCell(p []byte, off, need int) (int64, []any, error) {
	if off >= len(p) {
		return 0, nil, errors.New("a cell lies outside its page")
	}
	payloadLen, n := varint(p[off:])
	off += n
	rowid, n := varint(p[off:])
	off += n
	local := db.localPayload(int64(payloadLen))
	if off+int(local) > len(p) {
		return 0, nil, errors.New("a cell runs past its page")
	}
	payload := append([]byte(nil), p[off:off+int(local)]...)
	overflow := int64(0)
	if int64(payloadLen) > local {
		if off+int(local)+4 > len(p) {
			return 0, nil, errors.New("a cell's overflow pointer runs past its page")
		}
		overflow = int64(binary.BigEndian.Uint32(p[off+int(local):]))
	}
	// more reads overflow pages until the payload holds upTo bytes, and
	// answers the payload so far.
	more := func(upTo int) ([]byte, error) {
		for len(payload) < upTo && overflow != 0 && int64(len(payload)) < int64(payloadLen) {
			op, err := db.page(overflow)
			if err != nil {
				return payload, err
			}
			overflow = int64(binary.BigEndian.Uint32(op))
			chunk := op[4:db.usable]
			if left := int64(payloadLen) - int64(len(payload)); int64(len(chunk)) > left {
				chunk = chunk[:left]
			}
			payload = append(payload, chunk...)
		}
		return payload, nil
	}
	rec, err := decodeRecord(payload, need, more)
	return int64(rowid), rec, err
}

// localPayload is how much of a payload stays on its leaf page.
func (db *sqliteDB) localPayload(p int64) int64 {
	u := db.usable
	maxLocal := u - 35
	if p <= maxLocal {
		return p
	}
	minLocal := (u-12)*32/255 - 23
	k := minLocal + (p-minLocal)%(u-4)
	if k <= maxLocal {
		return k
	}
	return minLocal
}

// decodeRecord decodes a record's columns up to need (all when need is -1).
func decodeRecord(payload []byte, need int, more func(int) ([]byte, error)) ([]any, error) {
	if len(payload) == 0 {
		return nil, errors.New("an empty record")
	}
	headerLen, n := varint(payload)
	payload, err := more(int(headerLen))
	if err != nil {
		return nil, err
	}
	if int(headerLen) > len(payload) {
		return nil, errors.New("a record header runs past its payload")
	}
	var types []uint64
	for pos := n; pos < int(headerLen); {
		t, w := varint(payload[pos:int(headerLen)])
		if w == 0 {
			return nil, errors.New("a malformed record header")
		}
		types = append(types, t)
		pos += w
	}
	if need >= 0 && need+1 < len(types) {
		types = types[:need+1]
	}
	out := make([]any, 0, len(types))
	pos := int(headerLen)
	for _, t := range types {
		size := serialSize(t)
		if payload, err = more(pos + size); err != nil {
			return nil, err
		}
		if pos+size > len(payload) {
			return nil, errors.New("a record runs past its payload")
		}
		v := payload[pos : pos+size]
		switch {
		case t == 0:
			out = append(out, nil)
		case t >= 1 && t <= 6:
			out = append(out, signed(v))
		case t == 7:
			out = append(out, nil) // floats are not needed here
		case t == 8:
			out = append(out, int64(0))
		case t == 9:
			out = append(out, int64(1))
		case t >= 13 && t%2 == 1:
			out = append(out, string(v))
		default:
			out = append(out, v)
		}
		pos += size
	}
	return out, nil
}

func serialSize(t uint64) int {
	switch {
	case t <= 4:
		return []int{0, 1, 2, 3, 4}[t]
	case t == 5:
		return 6
	case t == 6 || t == 7:
		return 8
	case t >= 12:
		return int((t - 12) / 2)
	}
	return 0
}

func signed(b []byte) int64 {
	var v int64
	for _, c := range b {
		v = v<<8 | int64(c)
	}
	shift := 64 - 8*len(b)
	return v << shift >> shift // sign-extend
}

// varint reads SQLite's big-endian variable-length integer (1-9 bytes).
// A width of 0 means the bytes ran out.
func varint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < 9 && i < len(b); i++ {
		if i == 8 {
			return v<<8 | uint64(b[i]), 9
		}
		v = v<<7 | uint64(b[i]&0x7F)
		if b[i]&0x80 == 0 {
			return v, i + 1
		}
	}
	return v, 0
}
