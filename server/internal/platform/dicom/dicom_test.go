package dicom

import (
	"encoding/binary"
	"errors"
	"testing"
)

func el(tag uint32, vr, value string) []byte {
	if len(value)%2 == 1 {
		value += " "
	}
	out := binary.LittleEndian.AppendUint16(nil, uint16(tag>>16))
	out = binary.LittleEndian.AppendUint16(out, uint16(tag))
	out = append(out, vr...)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(value)))
	return append(out, value...)
}

func implicit(tag uint32, value string) []byte {
	if len(value)%2 == 1 {
		value += " "
	}
	out := binary.LittleEndian.AppendUint16(nil, uint16(tag>>16))
	out = binary.LittleEndian.AppendUint16(out, uint16(tag))
	out = binary.LittleEndian.AppendUint32(out, uint32(len(value)))
	return append(out, value...)
}

// File builds a small instance; the integration tests use the same shape.
func file(syntax string, body ...[]byte) []byte {
	out := append(make([]byte, 128), "DICM"...)
	out = append(out, el(0x00020010, "UI", syntax)...)
	for _, b := range body {
		out = append(out, b...)
	}
	return out
}

func undefinedSequence() []byte {
	// (0008,1115) SQ, undefined length, one undefined-length item holding a
	// tag that must not be read as the instance's.
	out := binary.LittleEndian.AppendUint16(nil, 0x0008)
	out = binary.LittleEndian.AppendUint16(out, 0x1115)
	out = append(out, "SQ\x00\x00"...)
	out = binary.LittleEndian.AppendUint32(out, 0xFFFFFFFF)
	out = append(out, 0xFE, 0xFF, 0x00, 0xE0, 0xFF, 0xFF, 0xFF, 0xFF)
	out = append(out, el(0x0020000E, "UI", "nested.series")...)
	out = append(out, 0xFE, 0xFF, 0x0D, 0xE0, 0, 0, 0, 0)
	return append(out, 0xFE, 0xFF, 0xDD, 0xE0, 0, 0, 0, 0)
}

func TestParseExplicit(t *testing.T) {
	data := file("1.2.840.10008.1.2.1",
		el(0x00080016, "UI", "1.2.840.10008.5.1.4.1.1.2"), el(0x00080018, "UI", "1.2.3.4"), el(0x00080020, "DA", "20261030"),
		el(0x00080060, "CS", "CT"), undefinedSequence(), el(0x00100010, "PN", "Doe^Jane"), el(0x00100020, "LO", "P-1"),
		el(0x0020000D, "UI", "1.2.3"), el(0x0020000E, "UI", "1.2.3.9"), el(0x00200013, "IS", "7"),
		// Past the last wanted attribute: pixel data is never reached.
		el(0x7FE00010, "OB", "pixels"))
	h, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if h.SOPInstance != "1.2.3.4" || h.Study != "1.2.3" || h.Series != "1.2.3.9" || h.Modality != "CT" || h.PatientID != "P-1" {
		t.Fatalf("%+v", h)
	}
	if v := h.Tags["00200013"]["Value"].([]any)[0]; v != 7 {
		t.Fatalf("instance number %v", v)
	}
	if v := h.Tags["00100010"]["Value"].([]any)[0].(map[string]any)["Alphabetic"]; v != "Doe^Jane" {
		t.Fatalf("patient name %v", v)
	}
}

func TestParseImplicit(t *testing.T) {
	h, err := Parse(file("1.2.840.10008.1.2", implicit(0x00080018, "9.9"), implicit(0x0020000D, "9"), implicit(0x0020000E, "9.1")))
	if err != nil || h.SOPInstance != "9.9" {
		t.Fatalf("%+v %v", h, err)
	}
}

func TestNotDICOM(t *testing.T) {
	if _, err := Parse([]byte("hello")); !errors.Is(err, ErrNotDICOM) {
		t.Fatal(err)
	}
	if _, err := Parse(file("1.2.840.10008.1.2.1", el(0x00080060, "CS", "CT"))); !errors.Is(err, ErrNotDICOM) {
		t.Fatalf("no UIDs: %v", err)
	}
}
