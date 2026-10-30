package adoption

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// A source is read into entries, numbered in the order read. An entry the
// source describes badly is still emitted, with Code set, so it shows in
// the results instead of vanishing.

// objectReader opens objects on the job's connection.
type objectReader interface {
	Open(ctx context.Context, key string) (io.ReadCloser, error)
}

type emitter struct {
	line int64
	emit func(model.AdoptionEntry) error
}

func (e *emitter) add(entry model.AdoptionEntry) error {
	e.line++
	entry.Line = e.line
	if entry.State == "" {
		entry.State = model.AdoptPending
	}
	if entry.Code != "" {
		entry.State = model.AdoptSkipped
	}
	if entry.Path == "" {
		entry.Path = entry.ObjectKey
	}
	if entry.Code == "" {
		normalized, _, err := model.NormalizePath(entry.Path, model.DefaultPathPolicy)
		if err != nil {
			entry.State, entry.Code, entry.Detail = model.AdoptSkipped, "invalid_path", err.Error()
		} else {
			entry.Path = normalized
		}
	}
	return e.emit(entry)
}

const maxSourceLine = 1 << 20

// manifestLine is one JSON line of a MANIFEST or URL_LIST source.
type manifestLine struct {
	Key         string            `json:"key"`
	URL         string            `json:"url"`
	Path        string            `json:"path"`
	Size        int64             `json:"size"`
	ContentType string            `json:"content_type"`
	Digests     map[string]string `json:"digests"`
	Metadata    map[string]string `json:"metadata"`
	// Digests may also be top-level fields: {"key": ..., "md5": ...}.
	MD5    string `json:"md5"`
	SHA1   string `json:"sha1"`
	SHA256 string `json:"sha256"`
	SHA512 string `json:"sha512"`
	CRC32C string `json:"crc32c"`
	XXH64  string `json:"xxh64"`
	XXH3   string `json:"xxh3"`
	XXH128 string `json:"xxh128"`
	C4     string `json:"c4"`
}

func (l manifestLine) digests() model.Digests {
	d := model.Digests{}
	for alg, v := range l.Digests {
		d[alg] = v
	}
	for alg, v := range map[string]string{"md5": l.MD5, "sha1": l.SHA1, "sha256": l.SHA256, "sha512": l.SHA512,
		"crc32c": l.CRC32C, "xxh64": l.XXH64, "xxh3": l.XXH3, "xxh128": l.XXH128, "c4": l.C4} {
		if v != "" {
			d[alg] = v
		}
	}
	return d
}

// readLines parses a JSON Lines source. For URL_LIST a line may also be a
// bare URL, optionally followed by whitespace and the path to give it.
func readLines(r io.Reader, urls bool, e *emitter) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxSourceLine)
	for sc.Scan() {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		var l manifestLine
		if urls && !strings.HasPrefix(text, "{") {
			fields := strings.Fields(text)
			l.URL = fields[0]
			if len(fields) > 1 {
				l.Path = fields[1]
			}
		} else if err := json.Unmarshal([]byte(text), &l); err != nil {
			if err := e.add(model.AdoptionEntry{Code: "invalid_json", Detail: err.Error()}); err != nil {
				return err
			}
			continue
		}
		entry := model.AdoptionEntry{ObjectKey: l.Key, URL: l.URL, Path: l.Path, SizeBytes: l.Size, ContentType: l.ContentType, Metadata: l.Metadata}
		expected, err := l.digests().Normalize()
		switch {
		case err != nil:
			entry.Code, entry.Detail = "invalid_digest", err.Error()
		case urls && l.URL == "":
			entry.Code = "url_required"
		case !urls && l.Key == "":
			entry.Code = "key_required"
		}
		if urls && entry.Path == "" && l.URL != "" {
			entry.Path = pathFromURL(l.URL)
		}
		entry.Expected = expected
		if err := e.add(entry); err != nil {
			return err
		}
	}
	return sc.Err()
}

func pathFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(path.Clean("/"+u.Hostname()+u.Path), "/")
}

// ─── S3 Inventory ────────────────────────────────────────────────────

// inventoryManifest is an S3 Inventory report's manifest.json.
type inventoryManifest struct {
	SourceBucket string `json:"sourceBucket"`
	FileFormat   string `json:"fileFormat"`
	FileSchema   string `json:"fileSchema"`
	Files        []struct {
		Key         string `json:"key"`
		Size        int64  `json:"size"`
		MD5Checksum string `json:"MD5checksum"`
	} `json:"files"`
}

// readInventory reads an S3 Inventory report: its manifest.json, then each
// CSV data file it lists (checked against its MD5). Keys are URL-encoded
// in the report. Delete markers and older versions are skipped. A
// single-part object's ETag is its MD5, so it becomes the expected digest;
// a multipart ETag is not a digest of the bytes and is not used.
func readInventory(ctx context.Context, r objectReader, manifestKey, bucket string, e *emitter) error {
	body, err := r.Open(ctx, manifestKey)
	if err != nil {
		return fmt.Errorf("open the inventory manifest: %w", err)
	}
	var m inventoryManifest
	err = json.NewDecoder(io.LimitReader(body, 16<<20)).Decode(&m)
	body.Close()
	if err != nil {
		return fmt.Errorf("read the inventory manifest: %w", err)
	}
	if !strings.EqualFold(m.FileFormat, "CSV") {
		return fmt.Errorf("inventory reports in %s are not supported; configure the report as CSV", m.FileFormat)
	}
	if m.SourceBucket != "" && bucket != "" && m.SourceBucket != bucket {
		return fmt.Errorf("the report lists bucket %s, but the connection's bucket is %s", m.SourceBucket, bucket)
	}
	column := map[string]int{}
	for i, name := range strings.Split(m.FileSchema, ",") {
		column[strings.TrimSpace(name)] = i
	}
	keyCol, ok := column["Key"]
	if !ok {
		return errors.New("the inventory schema has no Key column")
	}
	field := func(row []string, name string) string {
		if i, ok := column[name]; ok && i < len(row) {
			return row[i]
		}
		return ""
	}
	for _, f := range m.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := readChecked(ctx, r, f.Key, f.MD5Checksum)
		if err != nil {
			return err
		}
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return fmt.Errorf("inventory file %s: %w", f.Key, err)
		}
		rows := csv.NewReader(zr)
		rows.FieldsPerRecord = -1
		for {
			row, err := rows.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("inventory file %s: %w", f.Key, err)
			}
			if keyCol >= len(row) {
				continue
			}
			if field(row, "IsDeleteMarker") == "true" || field(row, "IsLatest") == "false" {
				continue
			}
			key, err := url.QueryUnescape(row[keyCol])
			entry := model.AdoptionEntry{ObjectKey: key}
			if err != nil {
				entry.ObjectKey, entry.Code = row[keyCol], "invalid_key"
			}
			entry.SizeBytes, _ = strconv.ParseInt(field(row, "Size"), 10, 64)
			if etag := strings.Trim(field(row, "ETag"), `"`); len(etag) == 32 && !strings.Contains(etag, "-") {
				entry.Expected = model.Digests{"md5": strings.ToLower(etag)}
			}
			if strings.HasSuffix(key, "/") && entry.SizeBytes == 0 {
				continue // a folder placeholder
			}
			if err := e.add(entry); err != nil {
				return err
			}
		}
	}
	return nil
}

// readChecked reads a small object whole (an inventory data file) and
// checks its MD5 when the report gives one.
func readChecked(ctx context.Context, r objectReader, key, wantMD5 string) ([]byte, error) {
	body, err := r.Open(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", key, err)
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, 1<<30))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", key, err)
	}
	if wantMD5 != "" {
		sum := md5.Sum(raw)
		if !strings.EqualFold(hex.EncodeToString(sum[:]), wantMD5) {
			return nil, fmt.Errorf("%s does not match the MD5 the report gives", key)
		}
	}
	return raw, nil
}

// ─── ASC MHL ─────────────────────────────────────────────────────────

// mhlFile is one hashed file in an ASC MHL list, v1 or v2.
type mhlFile struct {
	// v2: <path size="...">rel</path> with digests as siblings.
	Path struct {
		Value string `xml:",chardata"`
		Size  int64  `xml:"size,attr"`
	} `xml:"path"`
	// v1: <file>, <size>.
	File string `xml:"file"`
	Size int64  `xml:"size"`

	MD5        string `xml:"md5"`
	SHA1       string `xml:"sha1"`
	XXH64      string `xml:"xxh64"`
	XXH64BE    string `xml:"xxhash64be"`
	XXH64LE    string `xml:"xxhash64"`
	XXH3       string `xml:"xxh3"`
	XXH128     string `xml:"xxh128"`
	C4         string `xml:"c4"`
	SHA256     string `xml:"sha256"`
	Directory  bool
	LastChange string `xml:"lastmodificationdate"`
}

type mhlList struct {
	XMLName xml.Name  `xml:"hashlist"`
	Version string    `xml:"version,attr"`
	V2      []mhlFile `xml:"hashes>hash"`
	V1      []mhlFile `xml:"hash"`
}

// readMHL reads an ASC MHL list (v2, or legacy v1). Paths are relative to
// the media root: the folder that holds the ascmhl folder for v2, the
// list's own folder for v1.
func readMHL(ctx context.Context, r objectReader, key string, e *emitter) error {
	body, err := r.Open(ctx, key)
	if err != nil {
		return fmt.Errorf("open the MHL: %w", err)
	}
	defer body.Close()
	var list mhlList
	if err := xml.NewDecoder(io.LimitReader(body, 512<<20)).Decode(&list); err != nil {
		return fmt.Errorf("read the MHL: %w", err)
	}
	root := path.Dir(key)
	if i := strings.Index(key, "ascmhl/"); i >= 0 {
		root = strings.TrimSuffix(key[:i], "/")
	}
	files := list.V2
	if len(files) == 0 {
		files = list.V1
	}
	for _, f := range files {
		rel, size := strings.TrimSpace(f.Path.Value), f.Path.Size
		if rel == "" {
			rel, size = strings.TrimSpace(f.File), f.Size
		}
		if rel == "" {
			continue
		}
		d := model.Digests{}
		for alg, v := range map[string]string{"md5": f.MD5, "sha1": f.SHA1, "xxh64": f.XXH64, "xxh3": f.XXH3,
			"xxh128": f.XXH128, "c4": f.C4, "sha256": f.SHA256} {
			if v = strings.TrimSpace(v); v != "" {
				d[alg] = v
			}
		}
		if v := strings.TrimSpace(f.XXH64BE); v != "" {
			d["xxh64"] = v
		} else if v := strings.TrimSpace(f.XXH64LE); v != "" && len(v) == 16 {
			// v1's little-endian xxhash64, turned round.
			var b []byte
			for i := 14; i >= 0; i -= 2 {
				b = append(b, v[i], v[i+1])
			}
			d["xxh64"] = string(b)
		}
		entry := model.AdoptionEntry{ObjectKey: joinKey(root, rel), Path: rel, SizeBytes: size}
		expected, err := d.Normalize()
		if err != nil {
			entry.Code, entry.Detail = "invalid_digest", err.Error()
		}
		entry.Expected = expected
		if err := e.add(entry); err != nil {
			return err
		}
	}
	return nil
}

func joinKey(root, rel string) string {
	rel = strings.TrimPrefix(path.Clean("/"+rel), "/")
	if root == "" || root == "." {
		return rel
	}
	return strings.TrimSuffix(root, "/") + "/" + rel
}

// ─── BagIt ───────────────────────────────────────────────────────────

var bagManifests = []struct{ file, alg string }{
	{"manifest-sha512.txt", "sha512"}, {"manifest-sha256.txt", "sha256"},
	{"manifest-sha1.txt", "sha1"}, {"manifest-md5.txt", "md5"},
}

// readBag reads a BagIt bag (RFC 8493) at prefix: its declaration, its
// payload manifests (every algorithm present), and checks the payload
// file count against bag-info's Payload-Oxum. bag-info fields become each
// entry's metadata, as bag.<field>.
func readBag(ctx context.Context, r objectReader, prefix string, e *emitter) error {
	prefix = strings.TrimSuffix(prefix, "/")
	decl, err := readSmall(ctx, r, joinKey(prefix, "bagit.txt"))
	if err != nil {
		return fmt.Errorf("not a bag: %w", err)
	}
	if !strings.Contains(decl, "BagIt-Version:") {
		return errors.New("bagit.txt has no BagIt-Version")
	}
	info := map[string]string{}
	oxumCount := int64(-1)
	if text, err := readSmall(ctx, r, joinKey(prefix, "bag-info.txt")); err == nil {
		for _, line := range strings.Split(text, "\n") {
			k, v, ok := strings.Cut(line, ":")
			if !ok || strings.HasPrefix(line, " ") {
				continue
			}
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			if k == "Payload-Oxum" {
				if _, count, ok := strings.Cut(v, "."); ok {
					oxumCount, _ = strconv.ParseInt(count, 10, 64)
				}
			}
			if len(info) < 20 && len(k) <= 50 && len(v) <= 500 {
				info["bag."+strings.ToLower(strings.ReplaceAll(k, " ", "-"))] = v
			}
		}
	}
	files := map[string]model.Digests{}
	var order []string
	for _, m := range bagManifests {
		text, err := readSmall(ctx, r, joinKey(prefix, m.file))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(text, "\n") {
			sum, rel, ok := strings.Cut(strings.TrimSpace(line), " ")
			if !ok {
				continue
			}
			rel = strings.NewReplacer("%0A", "\n", "%0D", "\r", "%25", "%").Replace(strings.TrimSpace(rel))
			if files[rel] == nil {
				files[rel] = model.Digests{}
				order = append(order, rel)
			}
			files[rel][m.alg] = strings.ToLower(sum)
		}
	}
	if len(order) == 0 {
		return errors.New("the bag has no payload manifest")
	}
	if oxumCount >= 0 && oxumCount != int64(len(order)) {
		return fmt.Errorf("the manifests list %d files but Payload-Oxum says %d: the bag is incomplete", len(order), oxumCount)
	}
	for _, rel := range order {
		entry := model.AdoptionEntry{ObjectKey: joinKey(prefix, rel), Path: strings.TrimPrefix(rel, "data/"), Metadata: info}
		expected, err := files[rel].Normalize()
		if err != nil {
			entry.Code, entry.Detail = "invalid_digest", err.Error()
		}
		entry.Expected = expected
		if !strings.HasPrefix(rel, "data/") {
			entry.Code, entry.Detail = "outside_payload", "payload files live under data/"
		}
		if err := e.add(entry); err != nil {
			return err
		}
	}
	return nil
}

func readSmall(ctx context.Context, r objectReader, key string) (string, error) {
	body, err := r.Open(ctx, key)
	if err != nil {
		return "", err
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, 256<<20))
	return string(raw), err
}

// ─── STAC ────────────────────────────────────────────────────────────

// fetcher gets JSON documents by URL (guarded against private addresses).
type fetcher interface {
	JSON(ctx context.Context, url string, out any) error
}

type stacLink struct {
	Rel  string `json:"rel"`
	Href string `json:"href"`
}

type stacAsset struct {
	Href     string   `json:"href"`
	Type     string   `json:"type"`
	Roles    []string `json:"roles"`
	Checksum string   `json:"file:checksum"`
	Size     int64    `json:"file:size"`
}

type stacDoc struct {
	Type       string                     `json:"type"`
	ID         string                     `json:"id"`
	Collection string                     `json:"collection"`
	Properties map[string]any             `json:"properties"`
	Assets     map[string]stacAsset       `json:"assets"`
	Links      []stacLink                 `json:"links"`
	Features   []json.RawMessage          `json:"features"`
	Extra      map[string]json.RawMessage `json:"-"`
}

const maxSTACItems = 10000

// readSTAC mirrors a STAC Item, ItemCollection, Collection or Catalog:
// every asset of every item becomes an import entry, under
// collection/item/asset, with the item's properties as metadata and its
// file:checksum (a multihash) as the expected digest. Catalogs and
// collections are followed one level of child links deep.
func readSTAC(ctx context.Context, f fetcher, root string, e *emitter) error {
	seen := map[string]bool{}
	items := 0
	var visit func(ref string, depth int) error
	visit = func(ref string, depth int) error {
		if seen[ref] || items >= maxSTACItems {
			return nil
		}
		seen[ref] = true
		var doc stacDoc
		if err := f.JSON(ctx, ref, &doc); err != nil {
			return fmt.Errorf("read %s: %w", ref, err)
		}
		switch doc.Type {
		case "Feature":
			items++
			return stacItem(ref, doc, e)
		case "FeatureCollection":
			for _, raw := range doc.Features {
				var item stacDoc
				if json.Unmarshal(raw, &item) == nil && items < maxSTACItems {
					items++
					if err := stacItem(ref, item, e); err != nil {
						return err
					}
				}
			}
			return nil
		}
		// Collection or Catalog.
		for _, l := range doc.Links {
			next, err := resolve(ref, l.Href)
			if err != nil {
				continue
			}
			switch {
			case l.Rel == "item":
				if err := visit(next, depth); err != nil {
					return err
				}
			case l.Rel == "child" && depth < 1:
				if err := visit(next, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return visit(root, 0)
}

func stacItem(base string, item stacDoc, e *emitter) error {
	meta := map[string]string{"stac.item": item.ID}
	if item.Collection != "" {
		meta["stac.collection"] = item.Collection
	}
	for k, v := range item.Properties {
		if len(meta) >= 30 {
			break
		}
		switch v := v.(type) {
		case string:
			if len(v) <= 500 {
				meta["stac."+strings.ReplaceAll(k, ":", ".")] = v
			}
		case float64, bool:
			meta["stac."+strings.ReplaceAll(k, ":", ".")] = fmt.Sprint(v)
		}
	}
	collection := item.Collection
	if collection == "" {
		collection = "items"
	}
	for name, a := range item.Assets {
		href, err := resolve(base, a.Href)
		entry := model.AdoptionEntry{URL: href, ContentType: a.Type, SizeBytes: a.Size, Metadata: meta}
		entry.Path = fmt.Sprintf("%s/%s/%s%s", collection, item.ID, name, path.Ext(strings.Split(a.Href, "?")[0]))
		if err != nil {
			entry.URL, entry.Code = a.Href, "invalid_url"
		}
		if d, ok := multihash(a.Checksum); ok {
			entry.Expected = d
		}
		if err := e.add(entry); err != nil {
			return err
		}
	}
	return nil
}

// multihash reads a file:checksum multihash in hex: SHA-256 (1220),
// SHA-512 (1340) or MD5 (d50110).
func multihash(v string) (model.Digests, bool) {
	v = strings.ToLower(v)
	for _, m := range []struct{ prefix, alg string }{{"1220", "sha256"}, {"1340", "sha512"}, {"d50110", "md5"}} {
		prefix, alg := m.prefix, m.alg
		if strings.HasPrefix(v, prefix) {
			d, err := model.Digests{alg: v[len(prefix):]}.Normalize()
			return d, err == nil
		}
	}
	return nil, false
}

func resolve(base, ref string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	return b.ResolveReference(r).String(), nil
}
