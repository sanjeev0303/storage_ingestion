package processing

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Audio and video containers: each check walks the container's own
// structure (chunks, boxes, elements, pages, frames) to the end of the file.
// A recorder or a transfer that stopped early leaves a container whose last
// piece runs past the end, and players fail on it far from the upload.
// Codecs inside are not decoded.

// mediaWalk carries the bytes read so far, so a length can be checked
// against what is left of the object.
type mediaWalk struct {
	br     *bufio.Reader
	read   int64
	size   int64
	source string
	in     *inspection
}

func newMediaWalk(r io.Reader, size int64, source string, in *inspection) *mediaWalk {
	return &mediaWalk{br: bufio.NewReaderSize(r, 64<<10), size: size, source: source, in: in}
}

func (w *mediaWalk) left() int64 { return w.size - w.read }

func (w *mediaWalk) full(p []byte) error {
	n, err := io.ReadFull(w.br, p)
	w.read += int64(n)
	return err
}

func (w *mediaWalk) skip(n int64) error {
	got, err := io.CopyN(io.Discard, w.br, n)
	w.read += got
	return err
}

// truncated and corrupt record the finding, unless the read itself failed,
// which goes up to be retried.
func (w *mediaWalk) truncated(err error, detail string) error {
	if passthrough(err) {
		return err
	}
	w.in.find(w.source, "format.truncated_media", detail)
	return nil
}

func (w *mediaWalk) corrupt(err error, detail string) error {
	if passthrough(err) {
		return err
	}
	w.in.find(w.source, "format.corrupt_media", detail)
	return nil
}

// ─── RIFF: WAV, AVI, WebP ─────────────────────────────────────────────

// inspectRIFF walks a RIFF file's chunks. A WAV must say how its samples
// are stored (fmt) before the samples themselves (data). RF64 and BW64,
// the WAV forms for files over 4 GiB, carry their real sizes in ds64.
func inspectRIFF(r io.Reader, size int64, in *inspection) error {
	w := newMediaWalk(r, size, "riff", in)
	var hdr [12]byte
	if err := w.full(hdr[:]); err != nil {
		return w.truncated(err, "the RIFF header is cut short")
	}
	id, form := string(hdr[0:4]), string(hdr[8:12])
	riffSize := binary.LittleEndian.Uint32(hdr[4:8])
	large := id == "RF64" || id == "BW64"
	if form == "WAVE" {
		w.source = "wav"
	}
	// The chunks end where the header says, or at the end of the file when
	// the size is a placeholder (0, or all ones from a streaming writer).
	end := size
	if !large && riffSize != 0 && riffSize != 0xFFFFFFFF {
		declared := int64(riffSize) + 8
		if declared > size {
			return w.truncated(nil, fmt.Sprintf("the RIFF header says %d bytes but the file has %d: it was cut short", declared, size))
		}
		end = declared
	}
	sawFormat, sawData := false, false
	var dataSize64 int64 = -1
	for end-w.read >= 8 {
		var ch [8]byte
		if err := w.full(ch[:]); err != nil {
			return w.truncated(err, fmt.Sprintf("a chunk header at byte %d is cut short", w.read))
		}
		name := string(ch[0:4])
		n := int64(binary.LittleEndian.Uint32(ch[4:8]))
		if large && name == "data" && n == 0xFFFFFFFF && dataSize64 >= 0 {
			n = dataSize64 // the real size is in ds64
		}
		if n > end-w.read {
			return w.truncated(nil, fmt.Sprintf("the %q chunk needs %d bytes but only %d are left: the file was cut short", name, n, end-w.read))
		}
		switch name {
		case "ds64":
			if n < 16 {
				return w.corrupt(nil, "the ds64 chunk is too short")
			}
			var ds [16]byte
			if err := w.full(ds[:]); err != nil {
				return w.truncated(err, "the ds64 chunk is cut short")
			}
			dataSize64 = int64(binary.LittleEndian.Uint64(ds[8:16]))
			n -= 16
		case "fmt ":
			sawFormat = true
		case "data":
			if form == "WAVE" && !sawFormat {
				return w.corrupt(nil, "the samples (data) come before their format (fmt)")
			}
			sawData = true
		}
		if n%2 == 1 && end-w.read > n {
			n++ // chunks are padded to an even length; a last odd chunk often has no pad
		}
		if err := w.skip(n); err != nil {
			return w.truncated(err, fmt.Sprintf("the %q chunk is cut short", name))
		}
	}
	if form == "WAVE" && !sawData {
		return w.corrupt(nil, "the WAV has no data chunk")
	}
	return nil
}

// ─── FLAC ─────────────────────────────────────────────────────────────

// inspectFLAC walks the metadata blocks, STREAMINFO first, to the block
// marked last, and checks that audio frames follow.
func inspectFLAC(r io.Reader, size int64, in *inspection) error {
	w := newMediaWalk(r, size, "flac", in)
	var magic [4]byte
	if err := w.full(magic[:]); err != nil {
		return w.truncated(err, "the file is too short to be FLAC")
	}
	if string(magic[:3]) == "ID3" {
		// Some taggers put an ID3v2 tag before the stream; skip it.
		var rest [6]byte
		if err := w.full(rest[:]); err != nil {
			return w.truncated(err, "the ID3 tag is cut short")
		}
		n := int64(rest[2]&0x7F)<<21 | int64(rest[3]&0x7F)<<14 | int64(rest[4]&0x7F)<<7 | int64(rest[5]&0x7F)
		if rest[1]&0x10 != 0 {
			n += 10
		}
		if n > w.left() {
			return w.truncated(nil, "the ID3 tag runs past the end of the file")
		}
		if err := w.skip(n); err != nil {
			return w.truncated(err, "the ID3 tag is cut short")
		}
		if err := w.full(magic[:]); err != nil {
			return w.truncated(err, "the file ends after its ID3 tag")
		}
	}
	if string(magic[:]) != "fLaC" {
		return w.corrupt(nil, "the fLaC marker is missing")
	}
	for first := true; ; first = false {
		var hdr [4]byte
		if err := w.full(hdr[:]); err != nil {
			return w.truncated(err, "a metadata block header is cut short")
		}
		last, kind := hdr[0]&0x80 != 0, hdr[0]&0x7F
		n := int64(hdr[1])<<16 | int64(hdr[2])<<8 | int64(hdr[3])
		if first && (kind != 0 || n != 34) {
			return w.corrupt(nil, "the first metadata block is not STREAMINFO")
		}
		if kind == 127 {
			return w.corrupt(nil, "a metadata block has the invalid type 127")
		}
		if n > w.left() {
			return w.truncated(nil, fmt.Sprintf("a metadata block needs %d bytes but only %d are left: the file was cut short", n, w.left()))
		}
		if err := w.skip(n); err != nil {
			return w.truncated(err, "a metadata block is cut short")
		}
		if last {
			break
		}
	}
	var sync [2]byte
	if err := w.full(sync[:]); err != nil {
		return w.truncated(err, "the file ends after its metadata: there is no audio")
	}
	if sync[0] != 0xFF || sync[1]&0xFE != 0xF8 {
		return w.corrupt(nil, "the audio does not start with a FLAC frame")
	}
	return nil
}

// ─── MP3 ──────────────────────────────────────────────────────────────

var (
	mp3Bitrates = [2][3][16]int{ // [MPEG-1, MPEG-2/2.5][layer I, II, III], kbit/s
		{
			{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448, 0},
			{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 0},
			{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0},
		},
		{
			{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256, 0},
			{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},
			{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},
		},
	}
	mp3Rates = [3][3]int{{44100, 48000, 32000}, {22050, 24000, 16000}, {11025, 12000, 8000}} // MPEG-1, 2, 2.5
)

// mp3FrameLength reads a frame header and answers the frame's length in
// bytes, 0 when the four bytes are not a valid header, or -1 for a valid
// free-format frame, whose length is not in its header.
func mp3FrameLength(h []byte) int {
	if h[0] != 0xFF || h[1]&0xE0 != 0xE0 {
		return 0
	}
	if (h[1]>>3)&3 != 1 && (h[1]>>1)&3 != 0 && h[2]>>4 == 0 && (h[2]>>2)&3 != 3 {
		return -1
	}
	version := (h[1] >> 3) & 3 // 0: 2.5, 2: 2, 3: 1
	layer := (h[1] >> 1) & 3   // 1: III, 2: II, 3: I
	bitrateIndex := h[2] >> 4
	rateIndex := (h[2] >> 2) & 3
	padding := int(h[2]>>1) & 1
	if version == 1 || layer == 0 || bitrateIndex == 0 || bitrateIndex == 15 || rateIndex == 3 {
		return 0 // reserved values, or free format, whose length is not in the header
	}
	row, rateRow := 1, 1
	switch version {
	case 3:
		row, rateRow = 0, 0
	case 0:
		rateRow = 2
	}
	layerIndex := 3 - int(layer) // 0: I, 1: II, 2: III
	bitrate := mp3Bitrates[row][layerIndex][bitrateIndex] * 1000
	rate := mp3Rates[rateRow][rateIndex]
	switch {
	case layerIndex == 0:
		return (12*bitrate/rate + padding) * 4
	case layerIndex == 2 && row == 1:
		return 72*bitrate/rate + padding
	default:
		return 144*bitrate/rate + padding
	}
}

// inspectMP3 skips an ID3v2 tag and walks every MPEG audio frame to the
// end. Tags that follow the audio (ID3v1, APE, Lyrics3) end the walk.
func inspectMP3(r io.Reader, size int64, in *inspection) error {
	w := newMediaWalk(r, size, "mp3", in)
	head, _ := w.br.Peek(10)
	if len(head) == 10 && string(head[:3]) == "ID3" {
		n := int64(head[6]&0x7F)<<21 | int64(head[7]&0x7F)<<14 | int64(head[8]&0x7F)<<7 | int64(head[9]&0x7F)
		n += 10
		if head[5]&0x10 != 0 {
			n += 10 // footer
		}
		if n > size {
			return w.truncated(nil, "the ID3 tag runs past the end of the file")
		}
		if err := w.skip(n); err != nil {
			return w.truncated(err, "the ID3 tag is cut short")
		}
	}
	// Some taggers pad past the tag's declared size with zeros.
	for padding := 0; padding < 64<<10; padding++ {
		b, err := w.br.Peek(1)
		if err != nil || b[0] != 0 {
			break
		}
		_, _ = w.br.ReadByte()
		w.read++
	}
	frames := 0
	var h [4]byte
	for w.left() > 0 {
		peek, err := w.br.Peek(4)
		if len(peek) < 4 {
			if frames > 0 && w.left() < 4 {
				return w.truncated(err, "the last frame is cut short")
			}
			return w.truncated(err, "the file ends where a frame should start")
		}
		copy(h[:], peek)
		if frames > 0 && (string(h[:3]) == "TAG" || string(h[:3]) == "APE" || string(h[:3]) == "LYR") {
			return nil
		}
		n := mp3FrameLength(h[:])
		if n < 0 {
			in.note("mp3: free-format frames end the check at byte %d", w.read)
			return nil
		}
		if n == 0 {
			if frames == 0 {
				return w.corrupt(nil, "no MPEG audio frame where the audio should start")
			}
			return w.corrupt(nil, fmt.Sprintf("frame %d at byte %d has an invalid header", frames+1, w.read))
		}
		if int64(n) > w.left() {
			return w.truncated(nil, fmt.Sprintf("frame %d needs %d bytes but only %d are left: the file was cut short", frames+1, n, w.left()))
		}
		if err := w.skip(int64(n)); err != nil {
			return w.truncated(err, "a frame is cut short")
		}
		frames++
	}
	if frames == 0 {
		return w.corrupt(nil, "the file has no audio frames")
	}
	return nil
}

// ─── ISO base media: MP4, MOV, M4A, HEIF ──────────────────────────────

var bmffBoxes = map[string]bool{"ftyp": true, "styp": true, "moov": true, "mdat": true, "free": true, "skip": true, "wide": true, "pnot": true, "uuid": true}

// inspectBMFF walks the top-level boxes. A movie needs its index (moov), a
// fragment needs moof, and an image (HEIF, AVIF) needs meta. An index after
// the media data is noted: the file plays only once it has all arrived.
func inspectBMFF(r io.Reader, size int64, in *inspection) error {
	w := newMediaWalk(r, size, "mp4", in)
	seen := map[string]bool{}
	brand := ""
	order := []string{}
	for w.left() > 0 {
		if w.left() < 8 {
			// Too few to be a box: padding some writers leave, or a cut box header.
			in.note("mp4: %d bytes after the last box are not a box", w.left())
			break
		}
		var hdr [8]byte
		if err := w.full(hdr[:]); err != nil {
			return w.truncated(err, "a box header is cut short")
		}
		n := int64(binary.BigEndian.Uint32(hdr[0:4]))
		kind := string(hdr[4:8])
		headerBytes := int64(8)
		switch n {
		case 0:
			n = w.left() + 8 // runs to the end of the file
		case 1:
			var large [8]byte
			if err := w.full(large[:]); err != nil {
				return w.truncated(err, "a box's 64-bit size is cut short")
			}
			n = int64(binary.BigEndian.Uint64(large[:]))
			headerBytes = 16
		}
		if n < headerBytes {
			return w.corrupt(nil, fmt.Sprintf("the %q box at byte %d has an invalid size", kind, w.read-headerBytes))
		}
		body := n - headerBytes
		if body > w.left() {
			return w.truncated(nil, fmt.Sprintf("the %q box needs %d bytes but only %d are left: the file was cut short", kind, body, w.left()))
		}
		if kind == "ftyp" && body >= 4 {
			b := make([]byte, 4)
			if err := w.full(b); err != nil {
				return w.truncated(err, "the ftyp box is cut short")
			}
			brand = string(b)
			body -= 4
		}
		seen[kind] = true
		order = append(order, kind)
		if err := w.skip(body); err != nil {
			return w.truncated(err, fmt.Sprintf("the %q box is cut short", kind))
		}
	}
	switch {
	case brand == "heic" || brand == "heix" || brand == "mif1" || brand == "msf1" || brand == "avif" || brand == "avis":
		if !seen["meta"] {
			return w.corrupt(nil, "the image has no meta box")
		}
	case seen["styp"] || (seen["moof"] && !seen["moov"]):
		if !seen["moof"] {
			return w.corrupt(nil, "the media segment has no moof box")
		}
	case !seen["moov"]:
		return w.corrupt(nil, "the movie has no moov box: players cannot find its tracks")
	default:
		for _, kind := range order {
			if kind == "moov" {
				break
			}
			if kind == "mdat" {
				in.note("mp4: the index (moov) comes after the media data, so the file plays only once it has fully arrived; move it to the front for streaming")
				break
			}
		}
	}
	return nil
}

// ─── Matroska and WebM ────────────────────────────────────────────────

const (
	ebmlHeaderID = 0x1A45DFA3
	ebmlSegment  = 0x18538067
	ebmlTracks   = 0x1654AE6B
	ebmlCluster  = 0x1F43B675
	ebmlUnknown  = -1
)

// ebmlVint reads an EBML variable-length integer. For an element ID the
// marker bit is kept; for a size it is dropped, and all ones means unknown.
func (w *mediaWalk) ebmlVint(isID bool) (int64, error) {
	b, err := w.br.ReadByte()
	if err != nil {
		return 0, err
	}
	w.read++
	length := 1
	for mask := byte(0x80); length <= 8 && b&mask == 0; mask >>= 1 {
		length++
	}
	if length > 8 || (isID && length > 4) {
		return 0, errors.New("invalid EBML number")
	}
	value := int64(b)
	if !isID {
		value &= int64(0xFF >> length)
	}
	allOnes := value == int64(0xFF>>length)
	for i := 1; i < length; i++ {
		c, err := w.br.ReadByte()
		if err != nil {
			return 0, err
		}
		w.read++
		value = value<<8 | int64(c)
		if c != 0xFF {
			allOnes = false
		}
	}
	if !isID && allOnes {
		return ebmlUnknown, nil
	}
	return value, nil
}

// inspectMatroska reads the EBML header, then walks the Segment's top
// elements. A complete file has tracks and at least one cluster. A segment
// or cluster of unknown size, as live recorders write, is walked into.
func inspectMatroska(r io.Reader, size int64, in *inspection) error {
	w := newMediaWalk(r, size, "matroska", in)
	id, err := w.ebmlVint(true)
	if err != nil || id != ebmlHeaderID {
		return w.corrupt(err, "the EBML header is missing")
	}
	n, err := w.ebmlVint(false)
	if err != nil || n == ebmlUnknown || n > w.left() {
		return w.truncated(err, "the EBML header is cut short")
	}
	if err := w.skip(n); err != nil {
		return w.truncated(err, "the EBML header is cut short")
	}
	for w.left() > 0 {
		id, err := w.ebmlVint(true)
		if err != nil {
			return w.truncated(err, "an element ID is cut short")
		}
		n, err := w.ebmlVint(false)
		if err != nil {
			return w.truncated(err, "an element size is cut short")
		}
		if id != ebmlSegment {
			if n == ebmlUnknown || n > w.left() {
				return w.truncated(nil, "an element after the header runs past the end of the file")
			}
			if err := w.skip(n); err != nil {
				return w.truncated(err, "an element is cut short")
			}
			continue
		}
		if n != ebmlUnknown && n > w.left() {
			return w.truncated(nil, fmt.Sprintf("the segment needs %d bytes but only %d are left: the file was cut short", n, w.left()))
		}
		return w.matroskaSegment(n)
	}
	return w.corrupt(nil, "the file has no segment")
}

func (w *mediaWalk) matroskaSegment(n int64) error {
	end := w.size
	if n != ebmlUnknown {
		end = w.read + n
	}
	tracks, clusters := false, 0
	for w.read < end {
		id, err := w.ebmlVint(true)
		if err != nil {
			return w.truncated(err, fmt.Sprintf("an element ID at byte %d is cut short", w.read))
		}
		size, err := w.ebmlVint(false)
		if err != nil {
			return w.truncated(err, fmt.Sprintf("an element size at byte %d is cut short", w.read))
		}
		switch id {
		case ebmlTracks:
			tracks = true
		case ebmlCluster:
			clusters++
		}
		if size == ebmlUnknown {
			if id == ebmlCluster {
				// A live recorder writes clusters of unknown size. Their blocks
				// have sizes, so walk into the cluster: the next cluster or
				// top-level element simply follows its last block.
				continue
			}
			w.in.note("matroska: an element of unknown size ends the check at byte %d", w.read)
			return nil
		}
		if size > end-w.read {
			return w.truncated(nil, fmt.Sprintf("an element at byte %d needs %d bytes but only %d are left: the file was cut short", w.read, size, end-w.read))
		}
		if err := w.skip(size); err != nil {
			return w.truncated(err, "an element is cut short")
		}
	}
	if !tracks {
		return w.corrupt(nil, "the segment has no tracks")
	}
	if clusters == 0 {
		return w.corrupt(nil, "the segment has no clusters: there is no media")
	}
	return nil
}

// ─── Ogg: Vorbis, Opus, FLAC in Ogg ───────────────────────────────────

// oggCRC is Ogg's CRC-32: polynomial 0x04C11DB7, not reflected, starting at 0.
var oggCRCTable = func() [256]uint32 {
	var t [256]uint32
	for i := range t {
		c := uint32(i) << 24
		for range 8 {
			if c&0x80000000 != 0 {
				c = c<<1 ^ 0x04C11DB7
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return t
}()

func oggCRC(crc uint32, b []byte) uint32 {
	for _, c := range b {
		crc = crc<<8 ^ oggCRCTable[byte(crc>>24)^c]
	}
	return crc
}

// inspectOgg checks every page's checksum, and that each logical stream
// ends with an end-of-stream page; without one the recording was cut short.
func inspectOgg(r io.Reader, size int64, in *inspection) error {
	w := newMediaWalk(r, size, "ogg", in)
	open := map[uint32]bool{}
	pages := 0
	for w.left() > 0 {
		var hdr [27]byte
		if err := w.full(hdr[:]); err != nil {
			return w.truncated(err, fmt.Sprintf("page %d's header is cut short", pages+1))
		}
		if string(hdr[0:4]) != "OggS" || hdr[4] != 0 {
			return w.corrupt(nil, fmt.Sprintf("page %d at byte %d does not start with OggS", pages+1, w.read-27))
		}
		segments := make([]byte, hdr[26])
		if err := w.full(segments); err != nil {
			return w.truncated(err, fmt.Sprintf("page %d's segment table is cut short", pages+1))
		}
		bodyLen := 0
		for _, s := range segments {
			bodyLen += int(s)
		}
		if int64(bodyLen) > w.left() {
			return w.truncated(nil, fmt.Sprintf("page %d needs %d bytes but only %d are left: the file was cut short", pages+1, bodyLen, w.left()))
		}
		body := make([]byte, bodyLen)
		if err := w.full(body); err != nil {
			return w.truncated(err, fmt.Sprintf("page %d is cut short", pages+1))
		}
		want := binary.LittleEndian.Uint32(hdr[22:26])
		check := hdr
		binary.LittleEndian.PutUint32(check[22:26], 0)
		crc := oggCRC(oggCRC(oggCRC(0, check[:]), segments), body)
		if crc != want {
			return w.corrupt(nil, fmt.Sprintf("page %d's checksum does not match: the page is damaged", pages+1))
		}
		serial := binary.LittleEndian.Uint32(hdr[14:18])
		flags := hdr[5]
		if flags&0x02 != 0 {
			open[serial] = true
		}
		if flags&0x04 != 0 {
			delete(open, serial)
		}
		pages++
	}
	if pages == 0 {
		return w.corrupt(nil, "the file has no pages")
	}
	if len(open) > 0 {
		return w.truncated(nil, "a stream has no end-of-stream page: the recording was cut short")
	}
	return nil
}

// mediaFormat names the container check for an object, by its bytes and,
// for MP3, its name.
func mediaFormat(head []byte, ext string) string {
	switch {
	case len(head) >= 12 && (bytes.HasPrefix(head, []byte("RIFF")) || bytes.HasPrefix(head, []byte("RF64")) || bytes.HasPrefix(head, []byte("BW64"))):
		return "riff"
	case bytes.HasPrefix(head, []byte("fLaC")), bytes.HasPrefix(head, []byte("ID3")) && ext == ".flac":
		return "flac"
	case bytes.HasPrefix(head, []byte("OggS")):
		return "ogg"
	case bytes.HasPrefix(head, []byte{0x1A, 0x45, 0xDF, 0xA3}):
		return "matroska"
	case len(head) >= 8 && (string(head[4:8]) == "ftyp" || string(head[4:8]) == "styp"):
		return "bmff"
	case len(head) >= 8 && bmffBoxes[string(head[4:8])] && bmffNames[ext]:
		return "bmff" // a QuickTime file may start with moov, mdat or wide
	case ext == ".mp3":
		return "mp3" // by name only: an ID3 tag also fronts AAC and FLAC files
	}
	return ""
}

var bmffNames = map[string]bool{".mp4": true, ".m4a": true, ".m4v": true, ".mov": true, ".3gp": true, ".heic": true, ".avif": true}
