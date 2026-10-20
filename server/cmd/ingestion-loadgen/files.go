package main

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"io"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
)

// sizeClass is one band of the file size distribution.
type sizeClass struct {
	name     string
	percent  int
	min, max int64
}

// fileSpec is one synthetic file. Its bytes are never stored: they are
// generated again from the seed whenever they are needed.
type fileSpec struct {
	index int // the manifest line is index+1
	class string
	path  string
	size  int64
	sum   [32]byte
}

// parseClasses reads "small=85:1KiB-256KiB,medium=13:1MiB-8MiB,...".
func parseClasses(s string) ([]sizeClass, error) {
	var classes []sizeClass
	for _, part := range strings.Split(s, ",") {
		name, rest, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return nil, fmt.Errorf("size class %q: want name=percent:min-max", part)
		}
		percent, bounds, ok := strings.Cut(rest, ":")
		if !ok {
			return nil, fmt.Errorf("size class %q: want name=percent:min-max", part)
		}
		low, high, ok := strings.Cut(bounds, "-")
		if !ok {
			return nil, fmt.Errorf("size class %q: want a min-max range", part)
		}
		c := sizeClass{name: name}
		var err error
		if c.percent, err = strconv.Atoi(percent); err != nil || c.percent < 0 {
			return nil, fmt.Errorf("size class %q: bad percentage", part)
		}
		if c.min, err = parseSize(low); err != nil {
			return nil, err
		}
		if c.max, err = parseSize(high); err != nil {
			return nil, err
		}
		if c.min <= 0 || c.max < c.min {
			return nil, fmt.Errorf("size class %q: min must be positive and not above max", part)
		}
		classes = append(classes, c)
	}
	total := 0
	for _, c := range classes {
		total += c.percent
	}
	if total == 0 {
		return nil, fmt.Errorf("the size classes' percentages add up to 0")
	}
	return classes, nil
}

// parseSize reads a byte count such as 512, 64KiB, 8MiB or 1GiB.
func parseSize(s string) (int64, error) {
	units := []struct {
		suffix string
		factor int64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"B", 1}}
	s = strings.TrimSpace(s)
	for _, u := range units {
		if number, ok := strings.CutSuffix(s, u.suffix); ok {
			n, err := strconv.ParseInt(number, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("size %q: %w", s, err)
			}
			return n * u.factor, nil
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", s, err)
	}
	return n, nil
}

// plan picks every file's class and size from the seed: the same seed
// always gives the same dataset.
func plan(n int, classes []sizeClass, seed uint64) []fileSpec {
	total := 0
	for _, c := range classes {
		total += c.percent
	}
	rng := rand.New(rand.NewPCG(seed, 0))
	specs := make([]fileSpec, n)
	for i := range specs {
		pick := rng.IntN(total)
		class := classes[len(classes)-1]
		for _, c := range classes {
			if pick < c.percent {
				class = c
				break
			}
			pick -= c.percent
		}
		size := class.min + rng.Int64N(class.max-class.min+1)
		specs[i] = fileSpec{
			index: i, class: class.name, size: size,
			path: fmt.Sprintf("loadgen/%s/%04d/%07d.txt", class.name, i/1000, i),
		}
	}
	return specs
}

const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 ."

// content streams a file's bytes: readable text, so content checks see a
// text file, generated from the run seed and the file's index. The bytes
// do not depend on how the reads are sized, so hashing and uploading see
// the same file.
type content struct {
	rng       *rand.Rand
	remaining int64
	column    int
	// bits holds random bytes not used yet; left counts them.
	bits uint64
	left int
}

func newContent(seed uint64, f fileSpec) *content {
	return &content{rng: rand.New(rand.NewPCG(seed, uint64(f.index)+1)), remaining: f.size}
}

func (c *content) Read(p []byte) (int, error) {
	if c.remaining == 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), c.remaining)
	for i := range p[:n] {
		if c.column == 79 {
			p[i] = '\n'
			c.column = 0
			continue
		}
		if c.left == 0 {
			c.bits, c.left = c.rng.Uint64(), 8
		}
		p[i] = alphabet[c.bits&63]
		c.bits >>= 8
		c.left--
		c.column++
	}
	c.remaining -= n
	return int(n), nil
}

// hashAll computes every file's SHA-256 with workers goroutines.
func hashAll(specs []fileSpec, seed uint64, workers int) {
	next := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 256<<10)
			for i := range next {
				h := sha256.New()
				_, _ = io.CopyBuffer(h, newContent(seed, specs[i]), buf)
				copy(specs[i].sum[:], h.Sum(nil))
			}
		}()
	}
	for i := range specs {
		next <- i
	}
	close(next)
	wg.Wait()
}

// fakeSums gives each file a checksum without generating it, for runs that
// never upload.
func fakeSums(specs []fileSpec, seed uint64) {
	for i := range specs {
		specs[i].sum = sha256.Sum256([]byte(fmt.Sprintf("%d/%d", seed, i)))
	}
}

// writeManifest writes the JSON Lines manifest of specs.
func writeManifest(w io.Writer, specs []fileSpec) error {
	buf := bufio.NewWriterSize(w, 256<<10)
	for _, f := range specs {
		if _, err := fmt.Fprintf(buf, `{"path":%q,"size":%d,"content_type":"text/plain","checksum":"sha256:%x"}`+"\n",
			f.path, f.size, f.sum); err != nil {
			return err
		}
	}
	return buf.Flush()
}

// countingWriter counts what is written to it.
type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// manifestSize is the manifest's length and SHA-256, computed without
// holding it.
func manifestSize(specs []fileSpec) (int64, string) {
	counter := &countingWriter{}
	h := sha256.New()
	_ = writeManifest(io.MultiWriter(counter, h), specs)
	return counter.n, fmt.Sprintf("%x", h.Sum(nil))
}

// manifestBody streams the manifest through a pipe.
func manifestBody(specs []fileSpec) io.ReadCloser {
	r, w := io.Pipe()
	go func() { w.CloseWithError(writeManifest(w, specs)) }()
	return r
}
