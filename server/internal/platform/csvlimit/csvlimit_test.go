package csvlimit

import (
	"encoding/csv"
	"errors"
	"strings"
	"testing"
)

func readAll(input string, max int) ([][]string, error) {
	cr := csv.NewReader(Reader(strings.NewReader(input), max))
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	return cr.ReadAll()
}

func TestOrdinaryCSVPassesThrough(t *testing.T) {
	rows, err := readAll("a,b\n\"x, \"\"quoted\"\"\",2\n\"multi\nline\",3\n", 64)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[1][0] != `x, "quoted"` || rows[2][0] != "multi\nline" {
		t.Fatalf("rows changed: %q", rows)
	}
}

func TestLongLineIsRefused(t *testing.T) {
	_, err := readAll("a,b\n"+strings.Repeat("x", 100), 64)
	if !errors.Is(err, ErrRecordTooLong) {
		t.Fatalf("err = %v, want ErrRecordTooLong", err)
	}
}

func TestUnclosedQuoteIsRefusedAcrossShortLines(t *testing.T) {
	// Every physical line is short, but the quote never closes, so it is one record.
	_, err := readAll("a,b\n\""+strings.Repeat("x\n", 100), 64)
	if !errors.Is(err, ErrRecordTooLong) {
		t.Fatalf("err = %v, want ErrRecordTooLong", err)
	}
}

func TestEachRecordGetsTheFullLimit(t *testing.T) {
	line := strings.Repeat("y", 60) + "\n"
	if _, err := readAll(strings.Repeat(line, 1000), 64); err != nil {
		t.Fatalf("many records under the limit: %v", err)
	}
}
