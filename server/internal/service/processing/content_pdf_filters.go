package processing

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/ascii85"
	"errors"
	"io"
	"regexp"
	"strings"
)

// PDF stream filters. Names hidden in a stream are only seen once the stream
// is decoded, and a file can chain filters (/ASCII85Decode then
// /FlateDecode) to keep them from a scanner that only inflates. Image
// filters (DCT, JPX, JBIG2, CCITT) carry pixels, not dictionaries, and are
// left alone.

var (
	pdfFilterRe      = regexp.MustCompile(`/Filter\s*(\[[^\]]*\]|/[A-Za-z0-9]+)`)
	pdfFilterNameRe  = regexp.MustCompile(`/([A-Za-z0-9]+)`)
	pdfEarlyChangeRe = regexp.MustCompile(`/EarlyChange\s+0\b`)
)

var pdfFilterAliases = map[string]string{
	"Fl": "FlateDecode", "AHx": "ASCIIHexDecode", "A85": "ASCII85Decode", "LZW": "LZWDecode", "RL": "RunLengthDecode",
}

var errPDFFilter = errors.New("the PDF filter cannot be decoded")

// pdfStreamFilters reads the filters of the stream whose dictionary ends
// in dict: nil when it names none, ok false when one is not decodable.
func pdfStreamFilters(dict []byte) (filters []string, earlyChange int, ok bool) {
	// The dictionary is what follows the object's own "obj".
	if i := bytes.LastIndex(dict, []byte("obj")); i >= 0 {
		dict = dict[i+3:]
	}
	earlyChange = 1
	if pdfEarlyChangeRe.Match(dict) {
		earlyChange = 0
	}
	m := pdfFilterRe.FindSubmatch(dict)
	if m == nil {
		return nil, earlyChange, true
	}
	for _, name := range pdfFilterNameRe.FindAllSubmatch(m[1], -1) {
		filter := pdfDecodeName(name[1])
		if full, short := pdfFilterAliases[filter]; short {
			filter = full
		}
		switch filter {
		case "FlateDecode", "ASCIIHexDecode", "ASCII85Decode", "LZWDecode", "RunLengthDecode":
			filters = append(filters, filter)
		default:
			return nil, earlyChange, false
		}
	}
	return filters, earlyChange, true
}

// pdfDecode runs data through the filters in order, reading at most limit
// bytes out of each.
func pdfDecode(r io.Reader, filters []string, earlyChange int, limit int64) ([]byte, error) {
	var out []byte
	for i, filter := range filters {
		if i > 0 {
			r = bytes.NewReader(out)
		}
		var err error
		switch filter {
		case "FlateDecode":
			var zr io.ReadCloser
			if zr, err = zlib.NewReader(r); err == nil {
				// A stream inflated in part still has names worth seeing.
				out, _ = io.ReadAll(io.LimitReader(zr, limit))
				zr.Close()
			}
		case "ASCIIHexDecode":
			out, err = asciiHexDecode(r, limit)
		case "ASCII85Decode":
			out, err = ascii85Decode(r, limit)
		case "LZWDecode":
			out, err = lzwDecode(r, earlyChange, limit)
		case "RunLengthDecode":
			out, err = runLengthDecode(r, limit)
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// asciiHexDecode: pairs of hex digits, whitespace ignored, ending at '>'.
func asciiHexDecode(r io.Reader, limit int64) ([]byte, error) {
	br := bufio.NewReader(r)
	var out []byte
	high, odd := byte(0), false
	for int64(len(out)) < limit {
		c, err := br.ReadByte()
		if err == io.EOF || c == '>' {
			break
		}
		if err != nil {
			return nil, err
		}
		var v byte
		switch {
		case c >= '0' && c <= '9':
			v = c - '0'
		case c >= 'a' && c <= 'f':
			v = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			v = c - 'A' + 10
		case strings.IndexByte(" \t\r\n\f\x00", c) >= 0:
			continue
		default:
			return nil, errPDFFilter
		}
		if odd {
			out = append(out, high<<4|v)
		} else {
			high = v
		}
		odd = !odd
	}
	if odd {
		out = append(out, high<<4) // a missing last digit is 0
	}
	return out, nil
}

// ascii85Decode: ASCII base-85 up to its "~>" end, after an optional "<~".
func ascii85Decode(r io.Reader, limit int64) ([]byte, error) {
	br := bufio.NewReader(r)
	var encoded []byte
	for int64(len(encoded)) < limit*5/4+5 {
		c, err := br.ReadByte()
		if err == io.EOF || c == '~' {
			break
		}
		if err != nil {
			return nil, err
		}
		encoded = append(encoded, c)
	}
	encoded = bytes.TrimPrefix(bytes.TrimSpace(encoded), []byte("<~"))
	out, err := io.ReadAll(io.LimitReader(ascii85.NewDecoder(bytes.NewReader(encoded)), limit))
	if err != nil {
		return nil, errPDFFilter
	}
	return out, nil
}

// lzwDecode is PDF's LZW: 9 to 12 bit codes, most significant bit first,
// 256 clears the table and 257 ends the data. With earlyChange 1 (the
// default) the code width grows one code early, as TIFF does.
func lzwDecode(r io.Reader, earlyChange int, limit int64) ([]byte, error) {
	br := bufio.NewReader(r)
	var bits uint32
	nbits := 0
	width := 9
	table := make([][]byte, 258, 4096)
	reset := func() {
		table = table[:258]
		for i := 0; i < 256; i++ {
			table[i] = []byte{byte(i)}
		}
		width = 9
	}
	reset()
	var out, prev []byte
	for int64(len(out)) < limit {
		for nbits < width {
			c, err := br.ReadByte()
			if err == io.EOF {
				return out, nil // no end code: what was decoded stands
			}
			if err != nil {
				return nil, err
			}
			bits = bits<<8 | uint32(c)
			nbits += 8
		}
		code := int(bits >> (nbits - width) & (1<<width - 1))
		nbits -= width

		switch {
		case code == 256:
			reset()
			prev = nil
			continue
		case code == 257:
			return out, nil
		}
		var entry []byte
		switch {
		case code < len(table) && (code < 256 || code > 257):
			entry = table[code]
		case code == len(table) && prev != nil:
			entry = append(append([]byte{}, prev...), prev[0])
		default:
			return nil, errPDFFilter
		}
		out = append(out, entry...)
		if prev != nil && len(table) < 4096 {
			table = append(table, append(append([]byte{}, prev...), entry[0]))
		}
		prev = entry
		if len(table)+earlyChange >= 1<<width && width < 12 {
			width++
		}
	}
	return out[:limit], nil
}

// runLengthDecode: a length byte n, then n+1 literal bytes (n < 128), one
// byte repeated 257-n times (n > 128), or the end (n = 128).
func runLengthDecode(r io.Reader, limit int64) ([]byte, error) {
	br := bufio.NewReader(r)
	var out []byte
	for int64(len(out)) < limit {
		n, err := br.ReadByte()
		if err == io.EOF || n == 128 {
			break
		}
		if err != nil {
			return nil, err
		}
		if n < 128 {
			run := make([]byte, int(n)+1)
			if _, err := io.ReadFull(br, run); err != nil {
				return nil, errPDFFilter
			}
			out = append(out, run...)
			continue
		}
		b, err := br.ReadByte()
		if err != nil {
			return nil, errPDFFilter
		}
		out = append(out, bytes.Repeat([]byte{b}, 257-int(n))...)
	}
	return out[:min(int64(len(out)), limit)], nil
}
