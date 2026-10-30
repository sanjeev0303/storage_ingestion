package adoption

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

func TestDigestVectors(t *testing.T) {
	d := newDigester(model.Digests{"c4": "", "crc32c": "", "xxh3": "", "sha1": ""})
	d.Write([]byte(""))
	got := d.Digests()
	want := map[string]string{
		"md5":    "d41d8cd98f00b204e9800998ecf8427e",
		"sha1":   "da39a3ee5e6b4b0d3255bfef95601890afd80709",
		"sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"xxh64":  "ef46db3751d8e999",
		"xxh3":   "2d06800538d394c2",
		"crc32c": "00000000",
		"c4":     "c459dsjfscH38cYeXXYogktxf4Cd9ibshE3BHUo6a58hBXmRQdZrAkZzsWcbWtDg5oQstpDuni4Hirj75GEmTc1sFT",
	}
	for alg, w := range want {
		if got[alg] != w {
			t.Errorf("%s of nothing: %s, want %s", alg, got[alg], w)
		}
	}
	d = newDigester(model.Digests{"crc32c": ""})
	d.Write([]byte("123456789"))
	if c := d.Digests()["crc32c"]; c != "e3069283" {
		t.Errorf("crc32c check value: %s", c)
	}
	if bad := mismatch(model.Digests{"md5": "00", "xxh64": "ef46db3751d8e999"}, got); len(bad) != 1 || bad[0] != "md5" {
		t.Errorf("mismatch: %v", bad)
	}
}

// files is an objectReader over a map.
type files map[string][]byte

func (f files) Open(_ context.Context, key string) (io.ReadCloser, error) {
	b, ok := f[key]
	if !ok {
		return nil, errors.New("no such object " + key)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func collectEntries(t *testing.T, read func(*emitter) error) []model.AdoptionEntry {
	t.Helper()
	var out []model.AdoptionEntry
	e := &emitter{emit: func(entry model.AdoptionEntry) error { out = append(out, entry); return nil }}
	if err := read(e); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestManifestAndURLLines(t *testing.T) {
	src := `{"key":"raw/a.bin","path":"a.bin","size":3,"md5":"900150983CD24FB0D6963F7D28E17F72","metadata":{"camera":"a7"}}
{"key":"raw/b.bin","digests":{"xxh64":"0123456789abcdef"}}
not json
{"key":"raw/c.bin","md5":"short"}
{"path":"no-key"}
`
	got := collectEntries(t, func(e *emitter) error { return readLines(strings.NewReader(src), false, e) })
	if len(got) != 5 {
		t.Fatalf("%d entries", len(got))
	}
	if got[0].Expected["md5"] != "900150983cd24fb0d6963f7d28e17f72" || got[0].Path != "a.bin" || got[0].Metadata["camera"] != "a7" ||
		got[0].State != model.AdoptPending || got[0].Line != 1 {
		t.Fatalf("first: %+v", got[0])
	}
	if got[1].Path != "raw/b.bin" || got[1].Expected["xxh64"] != "0123456789abcdef" {
		t.Fatalf("second: %+v", got[1])
	}
	for i, code := range []string{"invalid_json", "invalid_digest", "key_required"} {
		if got[i+2].Code != code || got[i+2].State != model.AdoptSkipped {
			t.Fatalf("line %d: %+v, want %s", i+3, got[i+2], code)
		}
	}

	urls := collectEntries(t, func(e *emitter) error {
		return readLines(strings.NewReader("https://example.org/data/x.tif\nhttps://example.org/y.csv tables/y.csv\n"), true, e)
	})
	if urls[0].Path != "example.org/data/x.tif" || urls[1].Path != "tables/y.csv" || urls[1].URL != "https://example.org/y.csv" {
		t.Fatalf("urls: %+v", urls)
	}
}

func gzipped(s string) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

func TestS3Inventory(t *testing.T) {
	data := gzipped(`"lab","run%201/a.fastq","120","2026-01-01T00:00:00.000Z","900150983cd24fb0d6963f7d28e17f72","true","false"
"lab","run%201/old.fastq","10","2026-01-01T00:00:00.000Z","aa","false","false"
"lab","run%201/big.bam","999","2026-01-01T00:00:00.000Z","abcdef0123456789abcdef0123456789-3","true","false"
"lab","run%201/gone.txt","0","2026-01-01T00:00:00.000Z","","true","true"
"lab","run%201/","0","2026-01-01T00:00:00.000Z","","true","false"
`)
	sum := md5.Sum(data)
	manifest, _ := json.Marshal(map[string]any{
		"sourceBucket": "lab", "fileFormat": "CSV",
		"fileSchema": "Bucket, Key, Size, LastModifiedDate, ETag, IsLatest, IsDeleteMarker",
		"files":      []map[string]any{{"key": "inv/data/1.csv.gz", "size": len(data), "MD5checksum": hex.EncodeToString(sum[:])}},
	})
	r := files{"inv/manifest.json": manifest, "inv/data/1.csv.gz": data}
	got := collectEntries(t, func(e *emitter) error { return readInventory(context.Background(), r, "inv/manifest.json", "lab", e) })
	if len(got) != 2 {
		t.Fatalf("%d entries: %+v", len(got), got)
	}
	if got[0].ObjectKey != "run 1/a.fastq" || got[0].SizeBytes != 120 || got[0].Expected["md5"] != "900150983cd24fb0d6963f7d28e17f72" {
		t.Fatalf("single-part: %+v", got[0])
	}
	if got[1].ObjectKey != "run 1/big.bam" || len(got[1].Expected) != 0 {
		t.Fatalf("a multipart ETag is not a digest: %+v", got[1])
	}
	if err := readInventory(context.Background(), r, "inv/manifest.json", "other", &emitter{emit: func(model.AdoptionEntry) error { return nil }}); err == nil {
		t.Fatal("a report of another bucket was read")
	}
	r["inv/data/1.csv.gz"] = gzipped("tampered")
	if err := readInventory(context.Background(), r, "inv/manifest.json", "lab", &emitter{emit: func(model.AdoptionEntry) error { return nil }}); err == nil {
		t.Fatal("a data file that does not match its MD5 was read")
	}
}

func TestMHL(t *testing.T) {
	v2 := `<?xml version="1.0" encoding="UTF-8"?>
<hashlist version="2.0" xmlns="urn:ASC:MHL:v2.0">
  <hashes>
    <hash>
      <path size="1024" lastmodificationdate="2026-01-01T00:00:00Z">Clips/A001.mov</path>
      <xxh64 action="original" hashdate="2026-01-01T00:00:00Z">0EA03B369A463D9D</xxh64>
    </hash>
    <hash>
      <path size="10">Sound/A001.wav</path>
      <md5 action="original">900150983cd24fb0d6963f7d28e17f72</md5>
    </hash>
  </hashes>
</hashlist>`
	got := collectEntries(t, func(e *emitter) error {
		return readMHL(context.Background(), files{"shoot/day1/ascmhl/0001_day1.mhl": []byte(v2)}, "shoot/day1/ascmhl/0001_day1.mhl", e)
	})
	if len(got) != 2 || got[0].ObjectKey != "shoot/day1/Clips/A001.mov" || got[0].SizeBytes != 1024 ||
		got[0].Expected["xxh64"] != "0ea03b369a463d9d" || got[1].Expected["md5"] == "" {
		t.Fatalf("v2: %+v", got)
	}

	v1 := `<?xml version="1.0"?><hashlist version="1.1">
<hash><file>A001/clip.mov</file><size>5</size><xxhash64>9d3d469a363ba00e</xxhash64></hash></hashlist>`
	got = collectEntries(t, func(e *emitter) error {
		return readMHL(context.Background(), files{"cards/A001.mhl": []byte(v1)}, "cards/A001.mhl", e)
	})
	if len(got) != 1 || got[0].ObjectKey != "cards/A001/clip.mov" || got[0].Expected["xxh64"] != "0ea03b369a463d9d" {
		t.Fatalf("v1 little-endian xxhash64: %+v", got)
	}
}

func TestBagIt(t *testing.T) {
	bag := files{
		"bags/b1/bagit.txt":           []byte("BagIt-Version: 1.0\nTag-File-Character-Encoding: UTF-8\n"),
		"bags/b1/bag-info.txt":        []byte("Source-Organization: Lab\nPayload-Oxum: 6.2\n"),
		"bags/b1/manifest-sha256.txt": []byte(strings.Repeat("a", 64) + "  data/one.txt\n" + strings.Repeat("b", 64) + "  data/sub/two%25.txt\n"),
		"bags/b1/manifest-md5.txt":    []byte(strings.Repeat("c", 32) + "  data/one.txt\n"),
	}
	got := collectEntries(t, func(e *emitter) error { return readBag(context.Background(), bag, "bags/b1/", e) })
	if len(got) != 2 || got[0].ObjectKey != "bags/b1/data/one.txt" || got[0].Path != "one.txt" ||
		len(got[0].Expected) != 2 || got[1].Path != "sub/two%.txt" || got[0].Metadata["bag.source-organization"] != "Lab" {
		t.Fatalf("bag: %+v", got)
	}
	bag["bags/b1/bag-info.txt"] = []byte("Payload-Oxum: 6.3\n")
	if err := readBag(context.Background(), bag, "bags/b1", &emitter{emit: func(model.AdoptionEntry) error { return nil }}); err == nil {
		t.Fatal("an incomplete bag was read")
	}
	if err := readBag(context.Background(), files{}, "bags/none", &emitter{emit: func(model.AdoptionEntry) error { return nil }}); err == nil {
		t.Fatal("a folder without bagit.txt was read as a bag")
	}
}

type stacSite map[string]string

func (s stacSite) JSON(_ context.Context, url string, out any) error {
	doc, ok := s[url]
	if !ok {
		return errors.New("404 " + url)
	}
	return json.Unmarshal([]byte(doc), out)
}

func TestSTAC(t *testing.T) {
	site := stacSite{
		"https://stac.example.org/catalog.json": `{"type":"Catalog","id":"root","links":[{"rel":"child","href":"collections/s2/collection.json"}]}`,
		"https://stac.example.org/collections/s2/collection.json": `{"type":"Collection","id":"s2","links":[{"rel":"item","href":"items/t1.json"}]}`,
		"https://stac.example.org/collections/s2/items/t1.json": `{"type":"Feature","id":"t1","collection":"s2",
			"properties":{"datetime":"2026-05-01T10:00:00Z","eo:cloud_cover":12.5,"platform":"sentinel-2a"},
			"assets":{"B04":{"href":"../../../data/t1_B04.tif","type":"image/tiff; application=geotiff",
				"file:checksum":"1220` + strings.Repeat("ab", 32) + `"}}}`,
	}
	got := collectEntries(t, func(e *emitter) error { return readSTAC(context.Background(), site, "https://stac.example.org/catalog.json", e) })
	if len(got) != 1 {
		t.Fatalf("%d entries", len(got))
	}
	a := got[0]
	if a.URL != "https://stac.example.org/data/t1_B04.tif" || a.Path != "s2/t1/B04.tif" || a.Expected["sha256"] != strings.Repeat("ab", 32) ||
		a.Metadata["stac.platform"] != "sentinel-2a" || a.Metadata["stac.eo.cloud_cover"] != "12.5" || a.Metadata["stac.item"] != "t1" {
		t.Fatalf("asset: %+v", a)
	}
}

func TestPathsAreNormalized(t *testing.T) {
	got := collectEntries(t, func(e *emitter) error {
		return readLines(strings.NewReader(`{"key":"x","path":"a//b/./c.txt"}`+"\n"+`{"key":"y","path":"../escape"}`), false, e)
	})
	if got[0].Path != "a/b/c.txt" || got[1].Code != "invalid_path" {
		t.Fatalf("paths: %+v", got)
	}
}
