package main

import (
	"bytes"
	"crypto/sha256"
	"io"
	"testing"
)

func TestContentIsTheSameWhateverTheReadSize(t *testing.T) {
	f := fileSpec{index: 7, size: 300_001}
	whole, err := io.ReadAll(newContent(42, f))
	if err != nil || int64(len(whole)) != f.size {
		t.Fatalf("read %d bytes: %v", len(whole), err)
	}
	// Read in odd pieces, as an HTTP transport or a part limit would.
	var pieces bytes.Buffer
	r := newContent(42, f)
	buf := make([]byte, 13)
	for {
		n, err := r.Read(buf[:1+pieces.Len()%13])
		pieces.Write(buf[:n])
		if err == io.EOF {
			break
		}
	}
	if !bytes.Equal(whole, pieces.Bytes()) {
		t.Fatal("the bytes depend on the read sizes")
	}

	specs := []fileSpec{f}
	hashAll(specs, 42, 2)
	if specs[0].sum != sha256.Sum256(whole) {
		t.Fatal("hashAll does not hash the generated content")
	}
	if other, _ := io.ReadAll(newContent(43, f)); bytes.Equal(other, whole) {
		t.Fatal("another seed must give other bytes")
	}
}

func TestPlanFollowsTheSizeClasses(t *testing.T) {
	classes, err := parseClasses("small=80:1KiB-4KiB, large=20:6MiB-8MiB")
	if err != nil {
		t.Fatal(err)
	}
	specs := plan(10_000, classes, 1)
	large := 0
	for _, s := range specs {
		switch s.class {
		case "small":
			if s.size < 1024 || s.size > 4096 {
				t.Fatalf("small file of %d bytes", s.size)
			}
		case "large":
			large++
			if s.size < 6<<20 || s.size > 8<<20 {
				t.Fatalf("large file of %d bytes", s.size)
			}
		}
	}
	if large < 1800 || large > 2200 {
		t.Fatalf("%d large files of 10000, want about 20%%", large)
	}
	again := plan(10_000, classes, 1)
	for i := range specs {
		if specs[i] != again[i] {
			t.Fatal("the same seed must give the same plan")
		}
	}
	for _, bad := range []string{"small", "small=x:1-2", "small=10:5-1", "small=0:1-2"} {
		if _, err := parseClasses(bad); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
}
