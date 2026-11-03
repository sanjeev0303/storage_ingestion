package fastcdc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
)

func chunksOf(t *testing.T, r io.Reader) (out [][32]byte, sizes []int, all []byte) {
	t.Helper()
	c := New(r)
	for {
		b, err := c.Next()
		if errors.Is(err, io.EOF) {
			return out, sizes, all
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, sha256.Sum256(b))
		sizes = append(sizes, len(b))
		all = append(all, b...)
	}
}

func random(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func TestChunksRebuildTheFileWithinTheirBounds(t *testing.T) {
	data := random(1, 40<<20)
	// One byte at a time at the start of reads must change nothing.
	_, sizes, all := chunksOf(t, iotest.HalfReader(bytes.NewReader(data)))
	if !bytes.Equal(all, data) {
		t.Fatal("the chunks do not rebuild the file")
	}
	for i, n := range sizes {
		if n > MaxSize || (n < MinSize && i != len(sizes)-1) {
			t.Fatalf("chunk %d has %d bytes", i, n)
		}
	}
	mean := len(data) / len(sizes)
	if mean < AvgSize/2 || mean > AvgSize*2 {
		t.Fatalf("mean chunk %d over %d chunks", mean, len(sizes))
	}
}

func TestSmallAndEmptyFiles(t *testing.T) {
	if got, _, _ := chunksOf(t, bytes.NewReader(nil)); len(got) != 0 {
		t.Fatalf("empty: %d chunks", len(got))
	}
	if got, _, _ := chunksOf(t, bytes.NewReader(random(2, MinSize))); len(got) != 1 {
		t.Fatalf("at the minimum: %d chunks", len(got))
	}
}

// Without a boundary in sight, a chunk stops at the maximum.
func TestRepeatedBytesStopAtTheMaximum(t *testing.T) {
	_, sizes, _ := chunksOf(t, bytes.NewReader(make([]byte, 3*MaxSize)))
	for _, n := range sizes[:len(sizes)-1] {
		if n > MaxSize {
			t.Fatalf("sizes: %v", sizes)
		}
	}
}

func shared(a, b [][32]byte) int {
	seen := map[[32]byte]bool{}
	for _, h := range a {
		seen[h] = true
	}
	n := 0
	for _, h := range b {
		if seen[h] {
			n++
		}
	}
	return n
}

// The reason to cut by content: bytes inserted early leave later chunks alone.
func TestAnInsertionLeavesMostChunksAlone(t *testing.T) {
	data := random(3, 40<<20)
	before, _, _ := chunksOf(t, bytes.NewReader(data))
	edited := append(append(append([]byte{}, data[:5<<20]...), []byte("inserted")...), data[5<<20:]...)
	after, _, _ := chunksOf(t, bytes.NewReader(edited))
	if n := shared(before, after); n < len(after)-3 {
		t.Fatalf("%d of %d chunks kept", n, len(after))
	}
}

func BenchmarkChunk(b *testing.B) {
	data := random(4, 64<<20)
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		c := New(bytes.NewReader(data))
		for {
			if _, err := c.Next(); err != nil {
				break
			}
		}
	}
}

// The same vector is in sdk/python and sdk/go: a client that cuts anywhere else
// would find nothing to reuse.
const vector = "76aab42460290af8c3707a941931982e59207ffc27641b0681ceb9b96547b260"

func TestTheVectorTheSDKsAreHeldTo(t *testing.T) {
	var input bytes.Buffer
	for i := 0; input.Len() < 6<<20; i++ {
		sum := sha256.Sum256([]byte(strconv.Itoa(i)))
		input.Write(sum[:])
	}
	_, sizes, _ := chunksOf(t, bytes.NewReader(input.Bytes()[:6<<20]))
	parts := make([]string, len(sizes))
	for i, n := range sizes {
		parts[i] = strconv.Itoa(n)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, ",")))
	if got := hex.EncodeToString(sum[:]); got != vector {
		t.Fatalf("got %s for %v", got, sizes)
	}
}
