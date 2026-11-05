package processing

import (
	"bufio"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// inspectDICOM walks a DICOM Part 10 file's every element to the end: the
// file meta group, then the data set in its transfer syntax, through nested
// sequences and encapsulated pixel data. A transfer that stopped early cuts
// the pixel data short, and the header-only index (platform/dicom) never
// sees it. It also reports a file that still carries patient identifiers,
// or says its pixels have them burned in, unless the file says its identity
// was removed.

const (
	dcmItem          = 0xFFFEE000
	dcmItemEnd       = 0xFFFEE00D
	dcmSequenceEnd   = 0xFFFEE0DD
	dcmUndefined     = 0xFFFFFFFF
	dcmMaxDepth      = 32
	dcmIdentityKept  = 0x00120062 // Patient Identity Removed
	dcmBurnedIn      = 0x00280301 // Burned In Annotation
	dcmTransfer      = 0x00020010
	dcmPixelData     = 0x7FE00010
	dcmMaxValueBytes = 256 // identifying values longer than this are only noted as present
)

// dcmIdentifiers are the attributes that name or locate a patient (PS3.15
// Annex E's basic profile, the ones a data set most often keeps by mistake).
var dcmIdentifiers = map[uint32]string{
	0x00100010: "patient name",
	0x00100020: "patient ID",
	0x00100030: "birth date",
	0x00101000: "other patient IDs",
	0x00101001: "other patient names",
	0x00101040: "patient address",
	0x00102154: "patient telephone",
	0x00080090: "referring physician",
	0x00080080: "institution name",
}

type dcmWalk struct {
	br       *bufio.Reader
	read     int64
	size     int64 // -1 once the data set is deflated and its size unknown
	explicit bool
	big      bool
	found    map[string]bool
	removed  bool
	burnedIn bool
}

func (w *dcmWalk) full(p []byte) error {
	n, err := io.ReadFull(w.br, p)
	w.read += int64(n)
	return err
}

func (w *dcmWalk) skip(n int64) error {
	got, err := io.CopyN(io.Discard, w.br, n)
	w.read += got
	return err
}

func (w *dcmWalk) order() binary.ByteOrder {
	if w.big {
		return binary.BigEndian
	}
	return binary.LittleEndian
}

var errDcmEnd = errors.New("end of data set")

// element reads a tag, VR and length. A clean end of the file between
// elements is errDcmEnd; an end inside a header is a cut-short file.
func (w *dcmWalk) element() (uint32, string, uint32, error) {
	var h [8]byte
	n, err := io.ReadFull(w.br, h[:])
	w.read += int64(n)
	if n == 0 && errors.Is(err, io.EOF) {
		return 0, "", 0, errDcmEnd
	}
	if err != nil {
		return 0, "", 0, err
	}
	o := w.order()
	tag := uint32(o.Uint16(h[0:2]))<<16 | uint32(o.Uint16(h[2:4]))
	if tag>>16 == 0xFFFE || !w.explicit {
		return tag, "", o.Uint32(h[4:8]), nil
	}
	vr := string(h[4:6])
	switch vr {
	case "OB", "OD", "OF", "OL", "OV", "OW", "SQ", "UC", "UN", "UR", "UT", "SV", "UV":
		var l [4]byte
		if err := w.full(l[:]); err != nil {
			return 0, "", 0, err
		}
		return tag, vr, o.Uint32(l[:]), nil
	}
	return tag, vr, uint32(o.Uint16(h[6:8])), nil
}

// fits says whether a value of n bytes can still be in the file.
func (w *dcmWalk) fits(n uint32) bool {
	return w.size < 0 || int64(n) <= w.size-w.read
}

// value reads or skips one element's value. Sequences are walked.
func (w *dcmWalk) value(tag uint32, vr string, n uint32, depth int) error {
	if n == dcmUndefined {
		if tag == dcmPixelData {
			return w.fragments()
		}
		if vr == "UN" {
			// A UN of undefined length is a sequence encoded implicit VR
			// little endian, whatever the file's transfer syntax.
			explicit, big := w.explicit, w.big
			w.explicit, w.big = false, false
			err := w.items(depth + 1)
			w.explicit, w.big = explicit, big
			return err
		}
		if vr == "SQ" || vr == "" {
			return w.items(depth + 1)
		}
		return fmt.Errorf("element %08X has an undefined length", tag)
	}
	if !w.fits(n) {
		return fmt.Errorf("element %08X needs %d bytes but the file ends first: it was cut short", tag, n)
	}
	if vr == "SQ" {
		return w.definedItems(int64(n), depth+1)
	}
	if _, ok := dcmIdentifiers[tag]; ok || tag == dcmIdentityKept || tag == dcmBurnedIn {
		if n <= dcmMaxValueBytes {
			b := make([]byte, n)
			if err := w.full(b); err != nil {
				return err
			}
			w.note(tag, strings.TrimRight(string(b), " \x00"))
			return nil
		}
		w.note(tag, "long")
	}
	return w.skip(int64(n))
}

func (w *dcmWalk) note(tag uint32, v string) {
	switch {
	case tag == dcmIdentityKept:
		w.removed = strings.EqualFold(v, "YES")
	case tag == dcmBurnedIn:
		w.burnedIn = strings.EqualFold(v, "YES")
	case v != "":
		w.found[dcmIdentifiers[tag]] = true
	}
}

// items walks an undefined-length sequence (or encapsulated pixel data) to
// its delimiter. Each item may itself have a defined or undefined length.
func (w *dcmWalk) items(depth int) error {
	if depth > dcmMaxDepth {
		return errors.New("sequences are nested too deep")
	}
	for {
		tag, _, n, err := w.element()
		if err != nil {
			return dcmCut(err)
		}
		switch {
		case tag == dcmSequenceEnd:
			return nil
		case tag == dcmItem && n == dcmUndefined:
			if err := w.dataSet(-1, depth); err != nil {
				return err
			}
		case tag == dcmItem:
			if !w.fits(n) {
				return fmt.Errorf("an item needs %d bytes but the file ends first: it was cut short", n)
			}
			if err := w.dataSet(w.read+int64(n), depth); err != nil {
				return err
			}
		default:
			return fmt.Errorf("element %08X where a sequence item should be", tag)
		}
	}
}

// fragments walks encapsulated (compressed) pixel data: an offset table and
// one item per fragment, raw bytes, to the sequence delimiter.
func (w *dcmWalk) fragments() error {
	for count := 0; ; count++ {
		tag, _, n, err := w.element()
		if err != nil {
			return dcmCut(err)
		}
		if tag == dcmSequenceEnd {
			return nil
		}
		if tag != dcmItem || n == dcmUndefined {
			return fmt.Errorf("pixel data fragment %d is malformed", count)
		}
		if !w.fits(n) {
			return fmt.Errorf("pixel data fragment %d needs %d bytes but the file ends first: it was cut short", count, n)
		}
		if err := w.skip(int64(n)); err != nil {
			return dcmCut(err)
		}
	}
}

// definedItems walks a sequence of known length.
func (w *dcmWalk) definedItems(n int64, depth int) error {
	if depth > dcmMaxDepth {
		return errors.New("sequences are nested too deep")
	}
	end := w.read + n
	for w.read < end {
		tag, _, l, err := w.element()
		if err != nil {
			return dcmCut(err)
		}
		if tag != dcmItem {
			return fmt.Errorf("element %08X where a sequence item should be", tag)
		}
		itemEnd := int64(-1)
		if l != dcmUndefined {
			itemEnd = w.read + int64(l)
			if itemEnd > end {
				return errors.New("an item runs past the end of its sequence")
			}
		}
		if err := w.dataSet(itemEnd, depth); err != nil {
			return err
		}
	}
	if w.read != end {
		return errors.New("a sequence's items do not add up to its length")
	}
	return nil
}

// dataSet reads elements to end (a byte offset), to an item delimiter when
// end is -1 inside a sequence, or to the end of the file at the top.
func (w *dcmWalk) dataSet(end int64, depth int) error {
	for end < 0 || w.read < end {
		tag, vr, n, err := w.element()
		if errors.Is(err, errDcmEnd) && depth == 0 {
			return nil
		}
		if err != nil {
			return dcmCut(err)
		}
		if tag == dcmItemEnd {
			if depth == 0 || end >= 0 {
				return errors.New("an item delimiter outside an item")
			}
			return nil
		}
		if err := w.value(tag, vr, n, depth); err != nil {
			return dcmCut(err)
		}
	}
	if w.read != end {
		return errors.New("an item's elements do not add up to its length")
	}
	return nil
}

// dcmCut turns an end of file inside an element into a cut-short message.
func dcmCut(err error) error {
	if errors.Is(err, errDcmEnd) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return errors.New("the file ends inside an element: it was cut short")
	}
	return err
}

func inspectDICOM(r io.Reader, size int64, in *inspection) error {
	bad := func(err error, detail string) error {
		if passthrough(err) {
			return err
		}
		in.find("dicom", "format.invalid_dicom", detail)
		return nil
	}
	w := &dcmWalk{br: bufio.NewReaderSize(r, 64<<10), size: size, explicit: true, found: map[string]bool{}}
	var preamble [132]byte
	if err := w.full(preamble[:]); err != nil || string(preamble[128:132]) != "DICM" {
		return bad(err, "the DICM prefix is missing")
	}
	// The file meta group is always explicit little endian.
	syntax := ""
	for {
		head, err := w.br.Peek(2)
		if err != nil {
			return bad(err, "the file has no data set after its meta information")
		}
		if binary.LittleEndian.Uint16(head) != 0x0002 {
			break
		}
		tag, _, n, err := w.element()
		if err != nil {
			return bad(dcmCut(err), "the file meta information is cut short")
		}
		if !w.fits(n) || n > 1<<16 {
			return bad(nil, "a file meta element runs past the end of the file")
		}
		b := make([]byte, n)
		if err := w.full(b); err != nil {
			return bad(err, "the file meta information is cut short")
		}
		if tag == dcmTransfer {
			syntax = strings.TrimRight(string(b), " \x00")
		}
	}
	switch syntax {
	case "1.2.840.10008.1.2":
		w.explicit = false
	case "1.2.840.10008.1.2.2":
		w.big = true
	case "1.2.840.10008.1.2.1.99":
		// Deflated: the data set after the meta group is one raw deflate stream.
		w.br = bufio.NewReaderSize(flate.NewReader(w.br), 64<<10)
		w.size = -1
	}
	if err := w.dataSet(-1, 0); err != nil {
		return bad(err, err.Error())
	}
	if w.burnedIn {
		in.find("dicom", "phi.burned_in_annotation", "the image says patient information is burned into its pixels (Burned In Annotation is YES)")
	}
	if len(w.found) > 0 && !w.removed {
		var names []string
		for _, name := range []string{"patient name", "patient ID", "birth date", "other patient IDs", "other patient names", "patient address", "patient telephone", "referring physician", "institution name"} {
			if w.found[name] {
				names = append(names, name)
			}
		}
		in.find("dicom", "phi.dicom_identifiers", "the file carries "+strings.Join(names, ", ")+" and does not say its identity was removed")
	}
	return nil
}
