package processing

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// TIFF, BigTIFF, GeoTIFF and Cloud Optimized GeoTIFF. The check walks every
// image directory (IFD) and checks that each strip or tile it points at lies
// inside the file. When the file carries georeferencing, the GeoKey
// directory and the raster-to-model tie are checked, and so is the layout a
// Cloud Optimized GeoTIFF needs for ranged reads: tiled, directories before
// the image data, and overviews from largest to smallest.

const (
	tiffNewSubfileType   = 254
	tiffImageWidth       = 256
	tiffImageLength      = 257
	tiffStripOffsets     = 273
	tiffStripByteCounts  = 279
	tiffTileWidth        = 322
	tiffTileOffsets      = 324
	tiffTileByteCounts   = 325
	tiffModelPixelScale  = 33550
	tiffModelTiepoint    = 33922
	tiffModelTransform   = 34264
	tiffGeoKeyDirectory  = 34735
	tiffJPEGOffset       = 513
	tiffJPEGLength       = 514
	tiffMaxDirectories   = 10000
	tiffMaxOffsetsPerIFD = 1 << 24
	cogOverviewThreshold = 512 // a main image larger than this should have overviews
)

// tiffTypeSizes are the byte sizes of TIFF field types 1-18.
var tiffTypeSizes = map[uint16]int64{1: 1, 2: 1, 3: 2, 4: 4, 5: 8, 6: 1, 7: 1, 8: 2, 9: 4, 10: 8, 11: 4, 12: 8, 13: 4, 16: 8, 17: 8, 18: 8}

var errTIFF = errors.New("invalid TIFF structure")

type tiffFile struct {
	r     io.ReaderAt
	size  int64
	order binary.ByteOrder
	big   bool
}

func (t *tiffFile) at(off, n int64) ([]byte, error) {
	if off < 0 || n < 0 || off+n > t.size {
		return nil, errTIFF
	}
	b := make([]byte, n)
	if _, err := t.r.ReadAt(b, off); err != nil {
		return nil, sourceError{err}
	}
	return b, nil
}

func (t *tiffFile) offset(b []byte) int64 {
	if t.big {
		return int64(t.order.Uint64(b))
	}
	return int64(t.order.Uint32(b))
}

// tiffEntry is one field of a directory, with its values not yet read.
type tiffEntry struct {
	kind  uint16
	count int64
	data  []byte // the inline value bytes, when they fit in the entry
	off   int64  // otherwise where the values are
}

// values reads a field's values as unsigned integers (SHORT, LONG, LONG8).
func (t *tiffFile) values(e tiffEntry) ([]uint64, error) {
	size := tiffTypeSizes[e.kind]
	if size == 0 || e.count > tiffMaxOffsetsPerIFD {
		return nil, errTIFF
	}
	raw := e.data
	if raw == nil {
		var err error
		if raw, err = t.at(e.off, size*e.count); err != nil {
			return nil, err
		}
	}
	out := make([]uint64, e.count)
	for i := range out {
		v := raw[int64(i)*size:]
		switch size {
		case 1:
			out[i] = uint64(v[0])
		case 2:
			out[i] = uint64(t.order.Uint16(v))
		case 4:
			out[i] = uint64(t.order.Uint32(v))
		default:
			out[i] = t.order.Uint64(v)
		}
	}
	return out, nil
}

type tiffImage struct {
	dir                   int
	width, height         uint64
	tiled, overview       bool
	dataStart, dirOffset  int64
	geoKeys               []uint64
	scale, tie, transform bool
}

// inspectTIFF walks every directory of a classic or Big TIFF.
func inspectTIFF(r io.ReaderAt, size int64, ext string, in *inspection) error {
	t := &tiffFile{r: r, size: size}
	if size < 8 {
		in.find("tiff", "format.corrupt_tiff", "the TIFF header is cut short")
		return nil
	}
	head, err := t.at(0, min(size, 16))
	if err != nil {
		return err
	}
	switch string(head[0:2]) {
	case "II":
		t.order = binary.LittleEndian
	case "MM":
		t.order = binary.BigEndian
	default:
		in.find("tiff", "format.corrupt_tiff", "the byte-order mark is not II or MM")
		return nil
	}
	var next int64
	switch t.order.Uint16(head[2:4]) {
	case 42:
		next = int64(t.order.Uint32(head[4:8]))
	case 43:
		if len(head) < 16 {
			in.find("tiff", "format.corrupt_tiff", "the BigTIFF header is cut short")
			return nil
		}
		t.big = true
		next = int64(t.order.Uint64(head[8:16]))
	default:
		in.find("tiff", "format.corrupt_tiff", "the TIFF version is neither 42 nor 43 (BigTIFF)")
		return nil
	}

	var images []tiffImage
	seen := map[int64]bool{}
	for next != 0 {
		if seen[next] || len(images) >= tiffMaxDirectories {
			in.find("tiff", "format.corrupt_tiff", "the directories loop back on themselves")
			return nil
		}
		seen[next] = true
		img, following, err := t.directory(next, len(images)+1)
		if err != nil {
			if isSourceError(err) {
				return err
			}
			in.find("tiff", "format.corrupt_tiff", fmt.Sprintf("directory %d: %v", len(images)+1, err))
			return nil
		}
		images = append(images, img)
		next = following
	}
	if len(images) == 0 {
		in.find("tiff", "format.corrupt_tiff", "the file has no image directory")
		return nil
	}
	checkGeoTIFF(images, ext, in)
	return nil
}

// directory reads one IFD: its fields, and that each strip or tile it
// names lies inside the file.
func (t *tiffFile) directory(off int64, number int) (tiffImage, int64, error) {
	img := tiffImage{dir: number, dirOffset: off, dataStart: -1}
	countBytes, entryBytes, inline := int64(2), int64(12), int64(4)
	if t.big {
		countBytes, entryBytes, inline = 8, 20, 8
	}
	raw, err := t.at(off, countBytes)
	if err != nil {
		return img, 0, errors.New("it lies past the end of the file")
	}
	var count int64
	if t.big {
		count = int64(t.order.Uint64(raw))
	} else {
		count = int64(t.order.Uint16(raw))
	}
	if count > 65535 {
		return img, 0, errors.New("it claims too many fields")
	}
	body, err := t.at(off+countBytes, count*entryBytes+inline)
	if err != nil {
		return img, 0, errors.New("its fields run past the end of the file: it was cut short")
	}
	entries := map[uint16]tiffEntry{}
	for i := int64(0); i < count; i++ {
		e := body[i*entryBytes:]
		tag, kind := t.order.Uint16(e[0:2]), t.order.Uint16(e[2:4])
		var n int64
		valueAt := e[8:]
		if t.big {
			n = int64(t.order.Uint64(e[4:12]))
			valueAt = e[12:]
		} else {
			n = int64(t.order.Uint32(e[4:8]))
		}
		size := tiffTypeSizes[kind]
		if size == 0 {
			continue // an unknown type is skipped, as readers do
		}
		entry := tiffEntry{kind: kind, count: n}
		if size*n <= inline {
			entry.data = valueAt[:inline]
		} else {
			entry.off = t.offset(valueAt)
			if n > tiffMaxOffsetsPerIFD || entry.off+size*n > t.size {
				return img, 0, fmt.Errorf("field %d's values run past the end of the file: it was cut short", tag)
			}
		}
		entries[tag] = entry
	}
	following := t.offset(body[count*entryBytes:])

	first := func(tag uint16) uint64 {
		if e, ok := entries[tag]; ok {
			if v, err := t.values(e); err == nil && len(v) > 0 {
				return v[0]
			}
		}
		return 0
	}
	img.width, img.height = first(tiffImageWidth), first(tiffImageLength)
	img.overview = first(tiffNewSubfileType)&1 == 1
	_, img.tiled = entries[tiffTileWidth]
	offsetsTag, countsTag := uint16(tiffStripOffsets), uint16(tiffStripByteCounts)
	if img.tiled {
		offsetsTag, countsTag = tiffTileOffsets, tiffTileByteCounts
	}
	offE, okO := entries[offsetsTag]
	cntE, okC := entries[countsTag]
	if !okO || !okC {
		// No strips or tiles: a camera RAW's thumbnail directory points at a
		// JPEG instead (JPEGInterchangeFormat and its length), or holds no
		// image at all. Only the JPEG's place is checked.
		if at, length := first(tiffJPEGOffset), first(tiffJPEGLength); at > 0 && at+length > uint64(t.size) {
			return img, 0, fmt.Errorf("its JPEG needs bytes up to %d but the file has %d: it was cut short", at+length, t.size)
		}
		return img, following, nil
	}
	offsets, err := t.values(offE)
	if err != nil {
		return img, 0, errors.New("its data offsets cannot be read")
	}
	counts, err := t.values(cntE)
	if err != nil || len(counts) != len(offsets) {
		return img, 0, errors.New("its data offsets and byte counts do not match")
	}
	for i, o := range offsets {
		if counts[i] == 0 {
			continue // a sparse tile, allowed in a COG
		}
		if o+counts[i] > uint64(t.size) {
			return img, 0, fmt.Errorf("block %d needs bytes up to %d but the file has %d: it was cut short", i, o+counts[i], t.size)
		}
		if img.dataStart < 0 || int64(o) < img.dataStart {
			img.dataStart = int64(o)
		}
	}
	if e, ok := entries[tiffGeoKeyDirectory]; ok {
		img.geoKeys, _ = t.values(e)
	}
	_, img.scale = entries[tiffModelPixelScale]
	_, img.tie = entries[tiffModelTiepoint]
	_, img.transform = entries[tiffModelTransform]
	return img, following, nil
}

// checkGeoTIFF looks at a georeferenced file's keys and layout. A TIFF with
// no geo fields at all is an ordinary image and is left alone.
func checkGeoTIFF(images []tiffImage, ext string, in *inspection) {
	main := images[0]
	hasGeo := main.geoKeys != nil || main.scale || main.tie || main.transform
	if !hasGeo {
		return
	}
	keys := main.geoKeys
	switch {
	case keys == nil:
		in.find("tiff", "geo.missing_geokeys", "the raster is tied to model space but has no GeoKey directory, so its coordinate reference system is unknown")
	case len(keys) < 4 || keys[0] != 1 || int(keys[3])*4+4 > len(keys):
		in.find("tiff", "geo.invalid_geokeys", "the GeoKey directory is malformed")
	case !main.transform && !(main.scale && main.tie):
		in.find("tiff", "geo.missing_georeference", "the GeoKeys name a coordinate system but the raster is not tied to it (no pixel scale and tiepoint, or transformation)")
	}

	var problems []string
	if !main.tiled {
		problems = append(problems, "the main image is in strips, not tiles")
	}
	lastData := int64(-1)
	for _, img := range images {
		if img.dataStart >= 0 && (lastData < 0 || img.dataStart < lastData) {
			lastData = img.dataStart
		}
	}
	for _, img := range images {
		if lastData >= 0 && img.dirOffset > lastData {
			problems = append(problems, fmt.Sprintf("directory %d comes after image data, so a reader needs more than one request to find every image", img.dir))
			break
		}
	}
	overviews := 0
	prevWidth := main.width
	for _, img := range images[1:] {
		if !img.overview {
			continue
		}
		overviews++
		if img.width >= prevWidth {
			problems = append(problems, "the overviews are not ordered from largest to smallest")
			break
		}
		prevWidth = img.width
	}
	if overviews == 0 && (main.width > cogOverviewThreshold || main.height > cogOverviewThreshold) {
		problems = append(problems, fmt.Sprintf("a %dx%d image has no overviews", main.width, main.height))
	}
	if len(problems) > 0 {
		in.find("tiff", "geo.not_cloud_optimized", "not a Cloud Optimized GeoTIFF: "+strings.Join(problems, "; "))
	} else if ext == ".tif" || ext == ".tiff" {
		in.note("tiff: a valid Cloud Optimized GeoTIFF")
	}
}
