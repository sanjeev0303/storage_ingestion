package processing

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"strconv"
)

// inspectVCF checks a variant file's header and every record: the
// fileformat line first, the column header, and in each record the same
// number of columns as the header, a whole positive position and reference
// bases. A record cut in half by a failed transfer has too few columns.
func inspectVCF(r io.Reader, source string, in *inspection) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 256<<20) // a line holds every sample's genotype
	bad := func(detail string) error {
		in.find(source, "format.invalid_vcf", detail)
		return nil
	}
	columns := 0
	line := int64(0)
	records := int64(0)
	for sc.Scan() {
		line++
		text := bytes.TrimRight(sc.Bytes(), "\r")
		switch {
		case line == 1:
			if !bytes.HasPrefix(text, []byte("##fileformat=VCFv")) {
				return bad("the first line is not ##fileformat=VCFv4.x")
			}
		case bytes.HasPrefix(text, []byte("##")):
			if columns > 0 {
				return bad(fmt.Sprintf("line %d: a meta-information line after the column header", line))
			}
		case bytes.HasPrefix(text, []byte("#")):
			fields := bytes.Split(text, []byte("\t"))
			if columns > 0 || !vcfHeaderOK(fields) {
				return bad(fmt.Sprintf("line %d: the column header is not #CHROM POS ID REF ALT QUAL FILTER INFO, tab-separated", line))
			}
			columns = len(fields)
		case len(text) == 0:
			continue
		default:
			if columns == 0 {
				return bad(fmt.Sprintf("line %d: a record before the column header", line))
			}
			records++
			if detail := vcfRecordProblem(text, columns); detail != "" {
				return bad(fmt.Sprintf("line %d: %s", line, detail))
			}
		}
	}
	if err := sc.Err(); err != nil {
		if passthrough(err) {
			return err
		}
		return bad(fmt.Sprintf("line %d cannot be read: %v", line+1, err))
	}
	if columns == 0 {
		return bad("there is no #CHROM column header")
	}
	return nil
}

var vcfColumns = []string{"#CHROM", "POS", "ID", "REF", "ALT", "QUAL", "FILTER", "INFO"}

func vcfHeaderOK(fields [][]byte) bool {
	if len(fields) < len(vcfColumns) {
		return false
	}
	for i, want := range vcfColumns {
		if string(fields[i]) != want {
			return false
		}
	}
	// Sample columns need a FORMAT column before them.
	return len(fields) == len(vcfColumns) || string(fields[8]) == "FORMAT"
}

func vcfRecordProblem(text []byte, columns int) string {
	fields := bytes.Split(text, []byte("\t"))
	if len(fields) != columns {
		return fmt.Sprintf("%d columns where the header has %d: the record is cut short or damaged", len(fields), columns)
	}
	if len(fields[0]) == 0 {
		return "the chromosome is empty"
	}
	pos, err := strconv.ParseInt(string(fields[1]), 10, 64)
	if err != nil || pos < 0 {
		return fmt.Sprintf("the position %q is not a whole number", fields[1])
	}
	if len(fields[3]) == 0 {
		return "the reference allele is empty"
	}
	for _, c := range fields[3] {
		switch c {
		case 'A', 'C', 'G', 'T', 'N', 'a', 'c', 'g', 't', 'n':
		default:
			return fmt.Sprintf("the reference allele has the character %q", c)
		}
	}
	if len(fields[4]) == 0 {
		return "the alternate allele is empty (use . for none)"
	}
	return ""
}

// ─── CRAM ─────────────────────────────────────────────────────────────

var cramMagic = []byte("CRAM")

// cramEOFStart is the alignment start ("EOF" in ASCII) that marks the empty
// container closing every complete CRAM file.
const cramEOFStart = 0x454F46

// cramReader reads CRAM's integers: ITF8 (up to 32 bits in 1-5 bytes) and
// LTF8 (up to 64 bits in 1-9 bytes), and keeps the bytes read for the
// container header's CRC.
type cramReader struct {
	br  *bufio.Reader
	raw []byte
}

func (c *cramReader) byte() (byte, error) {
	b, err := c.br.ReadByte()
	if err == nil {
		c.raw = append(c.raw, b)
	}
	return b, err
}

func (c *cramReader) itf8() (int32, error) {
	b0, err := c.byte()
	if err != nil {
		return 0, err
	}
	extra := 0
	switch {
	case b0&0x80 == 0:
		return int32(b0), nil
	case b0&0xC0 == 0x80:
		extra = 1
	case b0&0xE0 == 0xC0:
		extra = 2
	case b0&0xF0 == 0xE0:
		extra = 3
	default:
		extra = 4
	}
	value := uint32(b0) & (0xFF >> (extra + 1))
	if extra == 4 {
		value = uint32(b0) & 0x0F // 1111xxxx: the first byte keeps four bits
	}
	for i := 0; i < extra; i++ {
		b, err := c.byte()
		if err != nil {
			return 0, err
		}
		if i == 3 {
			value = value<<4 | uint32(b&0x0F) // the fifth byte adds only four bits
		} else {
			value = value<<8 | uint32(b)
		}
	}
	return int32(value), nil
}

func (c *cramReader) ltf8() (int64, error) {
	b0, err := c.byte()
	if err != nil {
		return 0, err
	}
	extra := 0
	for mask := byte(0x80); extra < 8 && b0&mask != 0; mask >>= 1 {
		extra++
	}
	value := uint64(0)
	if extra < 7 {
		value = uint64(b0) & (0xFF >> (extra + 1))
	}
	for i := 0; i < extra; i++ {
		b, err := c.byte()
		if err != nil {
			return 0, err
		}
		value = value<<8 | uint64(b)
	}
	return int64(value), nil
}

// inspectCRAM walks every container to the end-of-file container. CRAM 3
// containers carry a CRC-32 of their header, which is checked; CRAM 2.1
// has none. A file without the EOF container was cut short.
func inspectCRAM(r io.Reader, size int64, in *inspection) error {
	bad := func(err error, code, detail string) error {
		if passthrough(err) {
			return err
		}
		in.find("cram", code, detail)
		return nil
	}
	br := bufio.NewReaderSize(r, 64<<10)
	var def [26]byte
	if _, err := io.ReadFull(br, def[:]); err != nil || !bytes.HasPrefix(def[:], cramMagic) {
		return bad(err, "format.invalid_cram", "the CRAM file definition is missing")
	}
	major := def[4]
	if major != 2 && major != 3 {
		in.note("cram: version %d.%d is not checked", def[4], def[5])
		return nil
	}
	read := int64(len(def))
	for container := 1; ; container++ {
		c := &cramReader{br: br}
		var lenBytes [4]byte
		for i := range lenBytes {
			b, err := c.byte()
			if err != nil {
				return bad(err, "format.truncated_cram", fmt.Sprintf("the file ends after %d containers with no end-of-file container: it was cut short", container-1))
			}
			lenBytes[i] = b
		}
		length := int64(int32(binary.LittleEndian.Uint32(lenBytes[:])))
		ref, err1 := c.itf8()
		start, err2 := c.itf8()
		_, err3 := c.itf8() // span
		records, err4 := c.itf8()
		_, err5 := c.ltf8() // record counter
		_, err6 := c.ltf8() // bases
		_, err7 := c.itf8() // blocks
		landmarks, err8 := c.itf8()
		for _, err := range []error{err1, err2, err3, err4, err5, err6, err7, err8} {
			if err != nil {
				return bad(err, "format.truncated_cram", fmt.Sprintf("container %d's header is cut short", container))
			}
		}
		if landmarks < 0 || landmarks > 1<<20 {
			return bad(nil, "format.invalid_cram", fmt.Sprintf("container %d has an invalid landmark count", container))
		}
		for range landmarks {
			if _, err := c.itf8(); err != nil {
				return bad(err, "format.truncated_cram", fmt.Sprintf("container %d's header is cut short", container))
			}
		}
		if major == 3 {
			var crc [4]byte
			if _, err := io.ReadFull(br, crc[:]); err != nil {
				return bad(err, "format.truncated_cram", fmt.Sprintf("container %d's header is cut short", container))
			}
			if crc32.ChecksumIEEE(c.raw) != binary.LittleEndian.Uint32(crc[:]) {
				return bad(nil, "format.invalid_cram", fmt.Sprintf("container %d's header checksum does not match: it is damaged", container))
			}
			read += 4
		}
		read += int64(len(c.raw))
		if length < 0 || length > size-read {
			return bad(nil, "format.truncated_cram", fmt.Sprintf("container %d needs %d bytes but only %d are left: the file was cut short", container, length, size-read))
		}
		if _, err := io.CopyN(io.Discard, br, length); err != nil {
			return bad(err, "format.truncated_cram", fmt.Sprintf("container %d is cut short", container))
		}
		read += length
		if ref == -1 && start == cramEOFStart && records == 0 {
			if read < size {
				in.note("cram: %d bytes follow the end-of-file container", size-read)
			}
			return nil
		}
	}
}
