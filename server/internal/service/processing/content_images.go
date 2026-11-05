package processing

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"sort"
	"strings"
)

// Image checks. Signature antivirus passes images whose metadata attacks the
// tool that reads them: a PNG text chunk named "profile" makes ImageMagick
// embed a server file in the thumbnail it writes (CVE-2022-44268), and bytes
// after an image's end turn it into another file (a zip or a script) for
// whoever reads past it.

var pngMagic = []byte("\x89PNG\r\n\x1a\n")

// pngKnownChunks are the chunks of the PNG specification (third edition),
// APNG and the registered extensions. Anything else is a private chunk.
var pngKnownChunks = map[string]bool{
	"IHDR": true, "PLTE": true, "IDAT": true, "IEND": true,
	"tRNS": true, "cHRM": true, "gAMA": true, "iCCP": true, "sBIT": true, "sRGB": true, "cICP": true, "mDCV": true, "cLLI": true,
	"tEXt": true, "zTXt": true, "iTXt": true, "bKGD": true, "hIST": true, "pHYs": true, "sPLT": true, "eXIf": true, "tIME": true,
	"acTL": true, "fcTL": true, "fdAT": true,
	"oFFs": true, "pCAL": true, "sCAL": true, "sTER": true, "gIFg": true, "gIFx": true, "gIFt": true, "dSIG": true,
	"iDOT": true, // written by macOS screenshots
}

func inspectPNG(r io.Reader, in *inspection) error {
	br := bufio.NewReaderSize(r, 64<<10)
	if _, err := io.ReadFull(br, make([]byte, len(pngMagic))); err != nil {
		return corruptImage(err, in, "png", "the file ends inside its signature")
	}
	var unknown []string
	header := make([]byte, 8)
	for first := true; ; first = false {
		if _, err := io.ReadFull(br, header); err != nil {
			return corruptImage(err, in, "png", "the file ends before its IEND chunk: it was cut short")
		}
		length := binary.BigEndian.Uint32(header[:4])
		kind := string(header[4:8])
		if length > 1<<31-1 || !pngChunkName(header[4:8]) {
			in.find("png", "format.corrupt_png", fmt.Sprintf("chunk %q has an impossible length or name", kind))
			return nil
		}
		if first && kind != "IHDR" {
			in.find("png", "format.corrupt_png", "the first chunk is "+kind+", not IHDR")
			return nil
		}

		// The chunk's data goes through the CRC; text chunks keep their start.
		crc := crc32.NewIEEE()
		crc.Write(header[4:8])
		var head bytes.Buffer
		keep := int64(0)
		if kind == "tEXt" || kind == "zTXt" || kind == "iTXt" {
			keep = min(int64(length), 80)
		}
		if _, err := io.CopyN(io.MultiWriter(crc, &head), br, keep); err != nil {
			return corruptImage(err, in, "png", "chunk "+kind+" is cut short")
		}
		if _, err := io.CopyN(crc, br, int64(length)-keep); err != nil {
			return corruptImage(err, in, "png", "chunk "+kind+" is cut short")
		}
		stored := make([]byte, 4)
		if _, err := io.ReadFull(br, stored); err != nil {
			return corruptImage(err, in, "png", "chunk "+kind+" is cut short")
		}
		if binary.BigEndian.Uint32(stored) != crc.Sum32() {
			in.find("png", "format.corrupt_png", "chunk "+kind+" fails its CRC check")
			return nil
		}

		switch {
		case kind == "IEND":
			pngUnknown(unknown, in)
			return trailingBytes(br, in, "png", "after its IEND chunk")
		case keep > 0:
			keyword, _, _ := bytes.Cut(head.Bytes(), []byte{0})
			if strings.EqualFold(string(keyword), "profile") {
				in.find("png", "image.png_profile_text", "a "+kind+" chunk named \"profile\": ImageMagick reads it as a file to embed (CVE-2022-44268)")
			}
		case !pngKnownChunks[kind] && len(unknown) < 5:
			unknown = append(unknown, kind)
		}
	}
}

// pngChunkName: four ASCII letters.
func pngChunkName(b []byte) bool {
	for _, c := range b {
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
			return false
		}
	}
	return true
}

func pngUnknown(kinds []string, in *inspection) {
	if len(kinds) == 0 {
		return
	}
	sort.Strings(kinds)
	in.find("png", "image.png_unknown_chunk", "chunks outside the PNG specification: "+strings.Join(kinds, ", "))
}

// inspectJPEG walks a JPEG's segments to its end-of-image marker, through
// the entropy-coded data after each start-of-scan.
func inspectJPEG(r io.Reader, in *inspection) error {
	br := bufio.NewReaderSize(r, 64<<10)
	if _, err := io.ReadFull(br, make([]byte, 2)); err != nil {
		return corruptImage(err, in, "jpeg", "the file ends inside its first marker")
	}
	multiPicture := false
	next := byte(0) // a marker the scan data ended at, already read
	for {
		marker, err := next, error(nil)
		next = 0
		if marker == 0 {
			marker, err = jpegMarker(br)
		}
		if err != nil {
			return truncatedJPEG(err, in)
		}
		switch {
		case marker == 0xD9: // end of image
			if multiPicture {
				return nil // MPF keeps the other pictures after the first one's end
			}
			return trailingBytes(br, in, "jpeg", "after its end-of-image marker")
		case marker == 0x01 || (marker >= 0xD0 && marker <= 0xD8):
			continue // markers without a length
		}
		size := make([]byte, 2)
		if _, err := io.ReadFull(br, size); err != nil {
			return truncatedJPEG(err, in)
		}
		length := int64(binary.BigEndian.Uint16(size))
		if length < 2 {
			in.find("jpeg", "format.corrupt_jpeg", fmt.Sprintf("segment FF%02X has an impossible length", marker))
			return nil
		}
		var head bytes.Buffer
		keep := min(length-2, 4)
		if _, err := io.CopyN(&head, br, keep); err != nil {
			return truncatedJPEG(err, in)
		}
		if _, err := io.CopyN(io.Discard, br, length-2-keep); err != nil {
			return truncatedJPEG(err, in)
		}
		if marker == 0xE2 && bytes.HasPrefix(head.Bytes(), []byte("MPF\x00")) {
			multiPicture = true
		}
		if marker == 0xDA { // start of scan: entropy-coded data until the next marker
			if next, err = skipScan(br); err != nil {
				return truncatedJPEG(err, in)
			}
		}
	}
}

// jpegMarker reads the next marker, skipping fill bytes.
func jpegMarker(br *bufio.Reader) (byte, error) {
	b, err := br.ReadByte()
	if err != nil {
		return 0, err
	}
	if b != 0xFF {
		return 0, errNotMarker
	}
	for b == 0xFF {
		if b, err = br.ReadByte(); err != nil {
			return 0, err
		}
	}
	return b, nil
}

var errNotMarker = errors.New("expected a marker")

// skipScan reads entropy-coded data up to the next marker that is neither a
// stuffed zero nor a restart, and returns that marker.
func skipScan(br *bufio.Reader) (byte, error) {
	for {
		b, err := br.ReadByte()
		if err != nil {
			return 0, err
		}
		if b != 0xFF {
			continue
		}
		for b == 0xFF { // fill bytes
			if b, err = br.ReadByte(); err != nil {
				return 0, err
			}
		}
		if b == 0x00 || (b >= 0xD0 && b <= 0xD7) {
			continue
		}
		return b, nil
	}
}

func truncatedJPEG(err error, in *inspection) error {
	if passthrough(err) {
		return err
	}
	if errors.Is(err, errNotMarker) {
		in.find("jpeg", "format.corrupt_jpeg", "bytes between segments where a marker should be")
		return nil
	}
	// Viewers show a JPEG cut short, so it flags rather than fails the format.
	in.find("jpeg", "image.truncated_jpeg", "the file ends before its end-of-image marker: it was cut short")
	return nil
}

func corruptImage(err error, in *inspection, source, detail string) error {
	if passthrough(err) {
		return err
	}
	in.find(source, "format.corrupt_"+source, detail)
	return nil
}

// trailingBytes: anything after the image's end is another file riding along.
func trailingBytes(br *bufio.Reader, in *inspection, source, where string) error {
	n, err := io.Copy(io.Discard, br)
	if err != nil {
		return err
	}
	if n > 0 {
		in.find(source, "image.trailing_data", fmt.Sprintf("%d bytes %s", n, where))
	}
	return nil
}
