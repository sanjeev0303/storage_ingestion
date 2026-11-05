package processing

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/hex"
	"hash/crc32"
	"math"
	"testing"
)

// expect checks that a file gives exactly the wanted finding code, or none
// when want is empty.
func expect(t *testing.T, name string, data []byte, want string) {
	t.Helper()
	got := findingDetails(t, name, data)
	if want == "" {
		if len(got) != 0 {
			t.Fatalf("%s: unexpected findings %v", name, got)
		}
		return
	}
	if _, ok := got[want]; !ok {
		t.Fatalf("%s: want %s, got %v", name, want, got)
	}
}

func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

func riffChunk(id string, data []byte) []byte {
	out := append([]byte(id), le32(uint32(len(data)))...)
	out = append(out, data...)
	if len(data)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

func wav(chunks ...[]byte) []byte {
	body := []byte("WAVE")
	for _, c := range chunks {
		body = append(body, c...)
	}
	return append(append([]byte("RIFF"), le32(uint32(len(body)))...), body...)
}

func TestWAV(t *testing.T) {
	fmtChunk := riffChunk("fmt ", make([]byte, 16))
	data := riffChunk("data", make([]byte, 8))
	good := wav(fmtChunk, data)
	expect(t, "a.wav", good, "")
	expect(t, "cut.wav", good[:len(good)-4], "format.truncated_media")
	expect(t, "order.wav", wav(data, fmtChunk), "format.corrupt_media")
	expect(t, "nodata.wav", wav(fmtChunk), "format.corrupt_media")
}

func TestFLAC(t *testing.T) {
	good := append([]byte("fLaC"), 0x80, 0, 0, 34)
	good = append(good, make([]byte, 34)...)
	metadataOnly := append([]byte(nil), good...)
	good = append(good, 0xFF, 0xF8, 0x69, 0x08)
	expect(t, "a.flac", good, "")
	expect(t, "cut.flac", metadataOnly, "format.truncated_media")
	notStreamInfo := append([]byte("fLaC"), 0x84, 0, 0, 4, 0, 0, 0, 0, 0xFF, 0xF8)
	expect(t, "bad.flac", notStreamInfo, "format.corrupt_media")
}

// mp3Frame is an MPEG-1 Layer III frame at 128 kbit/s and 44.1 kHz: 417 bytes.
func mp3Frame() []byte {
	f := make([]byte, 417)
	copy(f, []byte{0xFF, 0xFB, 0x90, 0x00})
	return f
}

func TestMP3(t *testing.T) {
	id3 := append([]byte("ID3\x04\x00\x00"), 0, 0, 0, 10)
	id3 = append(id3, make([]byte, 10)...)
	good := append(append(append([]byte(nil), id3...), mp3Frame()...), mp3Frame()...)
	expect(t, "a.mp3", good, "")
	expect(t, "tagged.mp3", append(append([]byte(nil), good...), append([]byte("TAG"), make([]byte, 125)...)...), "")
	expect(t, "cut.mp3", good[:len(good)-100], "format.truncated_media")
	expect(t, "noise.mp3", append(append([]byte(nil), id3...), bytes.Repeat([]byte{0x11}, 600)...), "format.corrupt_media")
}

func box(kind string, body []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(8+len(body)))
	return append(append(out, kind...), body...)
}

func TestMP4(t *testing.T) {
	ftyp := box("ftyp", []byte("isom\x00\x00\x02\x00isomiso2"))
	moov := box("moov", box("mvhd", make([]byte, 100)))
	mdat := box("mdat", make([]byte, 64))
	expect(t, "a.mp4", append(append(append([]byte(nil), ftyp...), moov...), mdat...), "")
	expect(t, "late-index.mp4", append(append(append([]byte(nil), ftyp...), mdat...), moov...), "")
	expect(t, "noindex.mp4", append(append([]byte(nil), ftyp...), mdat...), "format.corrupt_media")
	full := append(append(append([]byte(nil), ftyp...), moov...), mdat...)
	expect(t, "cut.mp4", full[:len(full)-20], "format.truncated_media")
	heic := append(box("ftyp", []byte("heic\x00\x00\x00\x00mif1heic")), box("meta", make([]byte, 20))...)
	expect(t, "a.heic", heic, "")
}

// ebml writes an element with a one-byte size, enough for test files.
func ebml(id []byte, body []byte) []byte {
	return append(append(append([]byte(nil), id...), 0x80|byte(len(body))), body...)
}

func TestMatroska(t *testing.T) {
	header := ebml([]byte{0x1A, 0x45, 0xDF, 0xA3}, ebml([]byte{0x42, 0x82}, []byte("webm")))
	tracks := ebml([]byte{0x16, 0x54, 0xAE, 0x6B}, make([]byte, 4))
	cluster := ebml([]byte{0x1F, 0x43, 0xB6, 0x75}, make([]byte, 6))
	segment := ebml([]byte{0x18, 0x53, 0x80, 0x67}, append(append([]byte(nil), tracks...), cluster...))
	good := append(append([]byte(nil), header...), segment...)
	expect(t, "a.webm", good, "")
	expect(t, "cut.webm", good[:len(good)-3], "format.truncated_media")
	noCluster := append(append([]byte(nil), header...), ebml([]byte{0x18, 0x53, 0x80, 0x67}, tracks)...)
	expect(t, "empty.webm", noCluster, "format.corrupt_media")
	// A live recording: segment and cluster sizes unknown until it ends.
	live := append(append(append([]byte(nil), header...), 0x18, 0x53, 0x80, 0x67, 0xFF), tracks...)
	live = append(live, 0x1F, 0x43, 0xB6, 0x75, 0xFF)
	live = append(live, ebml([]byte{0xA3}, []byte{0x81, 0, 0, 0x80, 1, 2})...) // a SimpleBlock
	expect(t, "live.webm", live, "")
	expect(t, "live-cut.webm", live[:len(live)-2], "format.truncated_media")
}

func oggPage(flags byte, serial uint32, seq uint32, body []byte) []byte {
	h := []byte("OggS")
	h = append(h, 0, flags)
	h = append(h, make([]byte, 8)...) // granule
	h = append(h, le32(serial)...)
	h = append(h, le32(seq)...)
	h = append(h, 0, 0, 0, 0, 1, byte(len(body)))
	page := append(h, body...)
	binary.LittleEndian.PutUint32(page[22:26], oggCRC(0, page))
	return page
}

func TestOgg(t *testing.T) {
	first, last := oggPage(0x02, 7, 0, []byte("OpusHead")), oggPage(0x04, 7, 1, []byte("audio"))
	expect(t, "a.ogg", append(append([]byte(nil), first...), last...), "")
	expect(t, "cut.ogg", first, "format.truncated_media")
	damaged := append(append([]byte(nil), first...), last...)
	damaged[len(damaged)-1] ^= 0xFF
	expect(t, "damaged.ogg", damaged, "format.corrupt_media")
}

const vcfHead = "##fileformat=VCFv4.3\n##contig=<ID=1>\n#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\tFORMAT\tS1\n"

func TestVCF(t *testing.T) {
	good := vcfHead + "1\t100\trs1\tA\tG\t50\tPASS\tDP=10\tGT\t0/1\n1\t200\t.\tAC\tA\t.\t.\t.\tGT\t1/1\n"
	expect(t, "a.vcf", []byte(good), "")
	expect(t, "cut.vcf", []byte(vcfHead+"1\t100\trs1\tA\tG\t50\tPASS\tDP=10\tGT\n"), "format.invalid_vcf")
	expect(t, "pos.vcf", []byte(vcfHead+"1\tx\trs1\tA\tG\t50\tPASS\tDP=10\tGT\t0/1\n"), "format.invalid_vcf")
	expect(t, "noformat.vcf", []byte("#CHROM\tPOS\n"), "format.invalid_vcf")

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(vcfHead + "1\t100\trs1\tA\t\t50\tPASS\tDP=10\tGT\t0/1\n"))
	_ = zw.Close()
	expect(t, "calls.vcf.gz", gz.Bytes(), "format.invalid_vcf")
}

// cramEOF3 is the end-of-file container every CRAM 3 file ends with.
var cramEOF3, _ = hex.DecodeString("0f000000ffffffff0fe0454f4600000000010005bdd94f00010006060100010001" + "00ee63014b")

func cramContainer(payload []byte) []byte {
	head := le32(uint32(len(payload)))
	head = append(head, 0, 0, 0, 1, 0, 0, 1, 0) // ref, start, span, records, counter, bases, blocks, landmarks
	out := append(append([]byte(nil), head...), le32(crc32.ChecksumIEEE(head))...)
	return append(out, payload...)
}

func TestCRAM(t *testing.T) {
	def := append([]byte("CRAM\x03\x00"), make([]byte, 20)...)
	body := append(append([]byte(nil), def...), cramContainer(make([]byte, 30))...)
	expect(t, "a.cram", append(append([]byte(nil), body...), cramEOF3...), "")
	expect(t, "cut.cram", body, "format.truncated_cram")
	damaged := append(append([]byte(nil), body...), cramEOF3...)
	damaged[len(def)+5] ^= 0x01 // the header's start, so its CRC no longer matches
	expect(t, "damaged.cram", damaged, "format.invalid_cram")
}

// tiffBuilder lays out a little-endian TIFF: one directory, then values.
type tiffField struct {
	tag, kind uint16
	values    []byte // already encoded
	count     uint32
}

func short(v uint16) []byte { return binary.LittleEndian.AppendUint16(nil, v) }

func tiff(fields []tiffField, image []byte) []byte {
	dirAt := uint32(8)
	extraAt := dirAt + 2 + uint32(len(fields))*12 + 4
	var dir, extra []byte
	dir = append(dir, short(uint16(len(fields)))...)
	for _, f := range fields {
		dir = append(dir, short(f.tag)...)
		dir = append(dir, short(f.kind)...)
		dir = append(dir, le32(f.count)...)
		if len(f.values) <= 4 {
			v := append(append([]byte(nil), f.values...), make([]byte, 4-len(f.values))...)
			dir = append(dir, v...)
		} else {
			dir = append(dir, le32(extraAt+uint32(len(extra)))...)
			extra = append(extra, f.values...)
		}
	}
	dir = append(dir, 0, 0, 0, 0)
	out := append([]byte("II*\x00"), le32(dirAt)...)
	out = append(append(out, dir...), extra...)
	return append(out, image...)
}

func doubles(vs ...float64) []byte {
	var out []byte
	for _, v := range vs {
		out = binary.LittleEndian.AppendUint64(out, math.Float64bits(v))
	}
	return out
}

func TestTIFF(t *testing.T) {
	image := make([]byte, 16)
	fields := func(stripBytes uint32, geo ...tiffField) []tiffField {
		base := []tiffField{
			{256, 3, short(4), 1}, {257, 3, short(4), 1},
			{273, 4, nil, 1}, {279, 4, le32(stripBytes), 1},
		}
		return append(base, geo...)
	}
	// The strip offset depends on the layout, so build once to measure it.
	build := func(stripBytes uint32, geo ...tiffField) []byte {
		f := fields(stripBytes, geo...)
		f[2].values = le32(0)
		size := len(tiff(f, image))
		f[2].values = le32(uint32(size - len(image)))
		return tiff(f, image)
	}
	expect(t, "a.tif", build(16), "")
	expect(t, "cut.tif", build(1000), "format.corrupt_tiff")

	scale := tiffField{33550, 12, doubles(10, 10, 0), 3}
	tie := tiffField{33922, 12, doubles(0, 0, 0, 500000, 4000000, 0), 6}
	keys := tiffField{34735, 3, append(append(append(short(1), short(1)...), short(0)...), short(0)...), 4}
	expect(t, "strips.tif", build(16, scale, tie, keys), "geo.not_cloud_optimized")
	expect(t, "nokeys.tif", build(16, scale, tie), "geo.missing_geokeys")
	expect(t, "notie.tif", build(16, keys), "geo.missing_georeference")
}

func dcmElement(group, elem uint16, vr string, value []byte) []byte {
	out := append(short(group), short(elem)...)
	out = append(out, vr...)
	switch vr {
	case "OB", "OW", "SQ", "UN", "UT":
		out = append(out, 0, 0)
		out = append(out, le32(uint32(len(value)))...)
	default:
		out = append(out, short(uint16(len(value)))...)
	}
	return append(out, value...)
}

func dicomFile(dataSet ...[]byte) []byte {
	out := append(make([]byte, 128), "DICM"...)
	out = append(out, dcmElement(0x0002, 0x0010, "UI", []byte("1.2.840.10008.1.2.1\x00"))...)
	for _, e := range dataSet {
		out = append(out, e...)
	}
	return out
}

func TestDICOM(t *testing.T) {
	pixels := dcmElement(0x7FE0, 0x0010, "OB", make([]byte, 8))
	name := dcmElement(0x0010, 0x0010, "PN", []byte("DOE^JOHN"))
	expect(t, "anon.dcm", dicomFile(pixels), "")
	expect(t, "named.dcm", dicomFile(name, pixels), "phi.dicom_identifiers")
	removed := dcmElement(0x0012, 0x0062, "CS", []byte("YES "))
	expect(t, "removed.dcm", dicomFile(name, removed, pixels), "")
	full := dicomFile(pixels)
	expect(t, "cut.dcm", full[:len(full)-3], "format.invalid_dicom")
	burned := dcmElement(0x0028, 0x0301, "CS", []byte("YES "))
	expect(t, "burned.dcm", dicomFile(burned, pixels), "phi.burned_in_annotation")

	// Compressed pixel data: an empty offset table, one fragment, the delimiter.
	encapsulated := append(short(0x7FE0), short(0x0010)...)
	encapsulated = append(encapsulated, "OB\x00\x00"...)
	encapsulated = append(encapsulated, le32(0xFFFFFFFF)...)
	encapsulated = append(encapsulated, append(append(short(0xFFFE), short(0xE000)...), le32(0)...)...)
	encapsulated = append(encapsulated, append(append(short(0xFFFE), short(0xE000)...), le32(4)...)...)
	encapsulated = append(encapsulated, 1, 2, 3, 4)
	encapsulated = append(encapsulated, append(append(short(0xFFFE), short(0xE0DD)...), le32(0)...)...)
	expect(t, "compressed.dcm", dicomFile(encapsulated), "")
	expect(t, "compressed-cut.dcm", dicomFile(encapsulated[:len(encapsulated)-10]), "format.invalid_dicom")

	// A sequence of undefined length holding one item of undefined length.
	sequence := append(short(0x0008), short(0x1140)...)
	sequence = append(sequence, "SQ\x00\x00"...)
	sequence = append(sequence, le32(0xFFFFFFFF)...)
	sequence = append(sequence, append(append(short(0xFFFE), short(0xE000)...), le32(0xFFFFFFFF)...)...)
	sequence = append(sequence, dcmElement(0x0008, 0x1150, "UI", []byte("1.2.3\x00"))...)
	sequence = append(sequence, append(append(short(0xFFFE), short(0xE00D)...), le32(0)...)...)
	sequence = append(sequence, append(append(short(0xFFFE), short(0xE0DD)...), le32(0)...)...)
	expect(t, "sequence.dcm", dicomFile(sequence, pixels), "")
}

func sqliteFile(pageSize uint16, pages uint32, size int) []byte {
	b := make([]byte, size)
	copy(b, sqliteMagic)
	binary.BigEndian.PutUint16(b[16:], pageSize)
	binary.BigEndian.PutUint32(b[24:], 3) // change counter
	binary.BigEndian.PutUint32(b[28:], pages)
	binary.BigEndian.PutUint32(b[92:], 3) // valid for the same change
	return b
}

func TestSQLite(t *testing.T) {
	expect(t, "bag.db3", sqliteFile(512, 2, 1024), "")
	expect(t, "partial.db3", sqliteFile(512, 2, 900), "format.truncated_sqlite")
	expect(t, "short.db3", sqliteFile(512, 4, 1024), "format.truncated_sqlite")
}

// Files real writers produce that a strict reading would wrongly refuse.
func TestRealWorldVariants(t *testing.T) {
	// A camera RAW's thumbnail directory points at a JPEG, not strips.
	jpeg := make([]byte, 20)
	raw := tiff([]tiffField{{513, 4, le32(0), 1}, {514, 4, le32(20), 1}}, jpeg)
	binary.LittleEndian.PutUint32(raw[8+2+8:], uint32(len(raw)-len(jpeg))) // the JPEG offset
	expect(t, "IMG_0001.CR2", raw, "")
	binary.LittleEndian.PutUint32(raw[8+2+12+8:], 2000) // a length past the end
	expect(t, "cut.CR2", raw, "format.corrupt_tiff")

	// Zero padding after the last MP4 box.
	padded := append(append(box("ftyp", []byte("isom\x00\x00\x02\x00")), box("moov", nil)...), 0, 0, 0)
	expect(t, "padded.mp4", padded, "")

	// FLAC behind an ID3v2 tag, and AAC behind one (not taken for MP3).
	id3 := append([]byte("ID3\x04\x00\x00"), 0, 0, 0, 4, 1, 2, 3, 4)
	flac := append([]byte("fLaC"), 0x80, 0, 0, 34)
	flac = append(append(flac, make([]byte, 34)...), 0xFF, 0xF8)
	expect(t, "tagged.flac", append(append([]byte(nil), id3...), flac...), "")
	expect(t, "tagged.aac", append(append([]byte(nil), id3...), 0xFF, 0xF1, 0x50, 0x80), "")

	// A free-format MP3 frame (bitrate index 0) ends the walk without a finding.
	expect(t, "free.mp3", append([]byte{0xFF, 0xFB, 0x00, 0x00}, make([]byte, 100)...), "")

	// A UN element of undefined length holds an implicit VR sequence.
	un := append(short(0x0009), short(0x1010)...)
	un = append(un, "UN\x00\x00"...)
	un = append(un, le32(0xFFFFFFFF)...)
	un = append(un, append(append(short(0xFFFE), short(0xE000)...), le32(0xFFFFFFFF)...)...)
	un = append(un, append(append(short(0x0009), short(0x1011)...), le32(2)...)...) // implicit: tag, 32-bit length
	un = append(un, 'A', 'B')
	un = append(un, append(append(short(0xFFFE), short(0xE00D)...), le32(0)...)...)
	un = append(un, append(append(short(0xFFFE), short(0xE0DD)...), le32(0)...)...)
	expect(t, "private.dcm", dicomFile(un, dcmElement(0x7FE0, 0x0010, "OB", make([]byte, 4))), "")
}
