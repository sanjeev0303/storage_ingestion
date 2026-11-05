// Package dicom reads the identifying attributes from a DICOM file's
// header: enough to index an instance and answer DICOMweb searches. It
// stops at the pixel data and never decodes images.
package dicom

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

var ErrNotDICOM = errors.New("not a DICOM file")

// Header is what an instance's header says. Tags holds the attributes in
// DICOM JSON (PS3.18 F.2), keyed by the eight-digit tag.
type Header struct {
	SOPClass, SOPInstance, Study, Series string
	Modality, PatientID, StudyDate       string
	Tags                                 map[string]map[string]any
}

type tagInfo struct {
	vr string
}

// The attributes kept: those QIDO-RS requires in a response, and what a
// viewer needs to list and sort.
var wanted = map[uint32]tagInfo{
	0x00080016: {"UI"}, 0x00080018: {"UI"}, 0x00080020: {"DA"}, 0x00080030: {"TM"}, 0x00080050: {"SH"}, 0x00080060: {"CS"},
	0x00081030: {"LO"}, 0x0008103E: {"LO"}, 0x00100010: {"PN"}, 0x00100020: {"LO"}, 0x00100030: {"DA"}, 0x00100040: {"CS"},
	0x0020000D: {"UI"}, 0x0020000E: {"UI"}, 0x00200010: {"SH"}, 0x00200011: {"IS"}, 0x00200013: {"IS"},
	0x00280010: {"US"}, 0x00280011: {"US"}, 0x00280100: {"US"},
}

const lastWanted = 0x00280100

// maxInflated bounds how much of a deflated data set the header read expands.
const maxInflated = 4 << 20

type reader struct {
	b        []byte
	pos      int
	explicit bool
	big      bool
}

func (r *reader) order() binary.ByteOrder {
	if r.big {
		return binary.BigEndian
	}
	return binary.LittleEndian
}

var errEnd = errors.New("end of the header")

// element reads one element's tag, VR and length, leaving pos at its value.
func (r *reader) element() (uint32, string, uint32, error) {
	if r.pos+8 > len(r.b) {
		return 0, "", 0, errEnd
	}
	o := r.order()
	tag := uint32(o.Uint16(r.b[r.pos:]))<<16 | uint32(o.Uint16(r.b[r.pos+2:]))
	// Item and delimiter tags have no VR in any transfer syntax.
	if tag>>16 == 0xFFFE {
		n := o.Uint32(r.b[r.pos+4:])
		r.pos += 8
		return tag, "", n, nil
	}
	if !r.explicit {
		n := o.Uint32(r.b[r.pos+4:])
		r.pos += 8
		return tag, "", n, nil
	}
	vr := string(r.b[r.pos+4 : r.pos+6])
	switch vr {
	case "OB", "OD", "OF", "OL", "OV", "OW", "SQ", "UC", "UN", "UR", "UT", "SV", "UV":
		if r.pos+12 > len(r.b) {
			return 0, "", 0, errEnd
		}
		n := o.Uint32(r.b[r.pos+8:])
		r.pos += 12
		return tag, vr, n, nil
	}
	n := uint32(o.Uint16(r.b[r.pos+6:]))
	r.pos += 8
	return tag, vr, n, nil
}

const undefined = 0xFFFFFFFF

// skipSequence passes an undefined-length value: its items, to the matching
// sequence delimiter. Nested sequences are walked the same way.
func (r *reader) skipSequence(depth int) error {
	if depth > 32 {
		return errors.New("sequences nested too deep")
	}
	for {
		tag, _, n, err := r.element()
		if err != nil {
			return err
		}
		switch {
		case tag == 0xFFFEE0DD:
			return nil
		case tag == 0xFFFEE00D:
			continue // an item's end; the sequence goes on
		case tag == 0xFFFEE000 && n != undefined:
			r.pos += int(n)
		case tag == 0xFFFEE000:
			continue // the item's elements are read as this level's
		case n == undefined:
			if err := r.skipSequence(depth + 1); err != nil {
				return err
			}
		default:
			r.pos += int(n)
		}
		if r.pos > len(r.b) {
			return errEnd
		}
	}
}

func text(v []byte) string {
	return strings.TrimRight(string(v), " \x00")
}

// Parse reads a header from the start of a file. Only as much of the file
// as holds the wanted attributes is needed.
func Parse(data []byte) (Header, error) {
	if len(data) < 132 || string(data[128:132]) != "DICM" {
		return Header{}, ErrNotDICOM
	}
	h := Header{Tags: map[string]map[string]any{}}
	// File meta information is always explicit little endian.
	meta := &reader{b: data, pos: 132, explicit: true}
	syntax := ""
	for {
		start := meta.pos
		tag, _, n, err := meta.element()
		if err != nil {
			return h, ErrNotDICOM
		}
		if tag>>16 != 0x0002 {
			meta.pos = start
			break
		}
		if meta.pos+int(n) > len(data) {
			return h, ErrNotDICOM
		}
		if tag == 0x00020010 {
			syntax = text(data[meta.pos : meta.pos+int(n)])
		}
		meta.pos += int(n)
	}
	body := &reader{b: data, pos: meta.pos, explicit: syntax != "1.2.840.10008.1.2", big: syntax == "1.2.840.10008.1.2.2"}
	if syntax == "1.2.840.10008.1.2.1.99" {
		// Deflated explicit little endian: the data set is one raw deflate
		// stream. Only the start of the file was read, so the stream ends
		// early; what inflated is enough for the header.
		inflated, _ := io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(data[meta.pos:])), maxInflated))
		data = inflated
		body = &reader{b: data, explicit: true}
	}
	for {
		tag, _, n, err := body.element()
		if err != nil {
			break // the header may run past what was read; keep what was found
		}
		if tag > lastWanted {
			break
		}
		if n == undefined {
			if err := body.skipSequence(0); err != nil {
				break
			}
			continue
		}
		if body.pos+int(n) > len(data) {
			break
		}
		if info, ok := wanted[tag]; ok {
			h.set(tag, info.vr, data[body.pos:body.pos+int(n)], body.order())
		}
		body.pos += int(n)
	}
	if h.SOPInstance == "" || h.Study == "" || h.Series == "" {
		return h, fmt.Errorf("%w: no study, series or instance UID", ErrNotDICOM)
	}
	return h, nil
}

func (h *Header) set(tag uint32, vr string, v []byte, o binary.ByteOrder) {
	key := fmt.Sprintf("%08X", tag)
	attr := map[string]any{"vr": vr}
	switch vr {
	case "US":
		if len(v) >= 2 {
			attr["Value"] = []any{int(o.Uint16(v))}
		}
	case "IS":
		if n, err := strconv.Atoi(strings.TrimSpace(text(v))); err == nil {
			attr["Value"] = []any{n}
		}
	case "PN":
		if s := text(v); s != "" {
			attr["Value"] = []any{map[string]any{"Alphabetic": s}}
		}
	default:
		if s := text(v); s != "" {
			attr["Value"] = []any{s}
		}
	}
	h.Tags[key] = attr
	s := text(v)
	switch tag {
	case 0x00080016:
		h.SOPClass = s
	case 0x00080018:
		h.SOPInstance = s
	case 0x00080020:
		h.StudyDate = s
	case 0x00080060:
		h.Modality = s
	case 0x00100020:
		h.PatientID = s
	case 0x0020000D:
		h.Study = s
	case 0x0020000E:
		h.Series = s
	}
}
