package compaction

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"sync"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Source is one entry to pack: the bytes Open returns must be exactly Size
// bytes with SHA-256 SHA256.
type Source struct {
	AssetID string
	Size    int64
	SHA256  string
	Open    func(ctx context.Context) (io.ReadCloser, error)
}

// ErrSourceMismatch: a source's bytes are not the ones recorded for it. It
// is a data-integrity failure, never retried as if transient.
var ErrSourceMismatch = errors.New("source does not match its recorded size or checksum")

// SourceError is a failure reading one source.
type SourceError struct {
	Index   int
	AssetID string
	Err     error
}

func (e *SourceError) Error() string {
	return fmt.Sprintf("source %d (asset %s): %v", e.Index, e.AssetID, e.Err)
}

func (e *SourceError) Unwrap() error { return e.Err }

// Layout is where a packed entry landed.
type Layout struct {
	Offset int64
	Length int64
}

// Stream is a pack's bytes, read from its sources in order. Up to
// concurrency sources are fetched ahead of the reader into memory, each
// checked against its size and checksum, so memory stays near
// concurrency × the largest source whatever the pack's size, and a fetch
// waits while the reader is behind. A source that fails, or does not
// match, ends the stream with its error: the upload reading it is aborted.
type Stream struct {
	sources []Source
	results []chan fetched
	slots   chan struct{}
	cancel  context.CancelFunc

	current []byte
	next    int
	offset  int64
	sum     hash.Hash
	layout  []Layout
	err     error
	once    sync.Once
}

type fetched struct {
	data []byte
	err  error
}

// NewStream starts fetching sources, at most concurrency at a time.
func NewStream(ctx context.Context, sources []Source, concurrency int) *Stream {
	ctx, cancel := context.WithCancel(ctx)
	s := &Stream{
		sources: sources,
		results: make([]chan fetched, len(sources)),
		slots:   make(chan struct{}, max(concurrency, 1)),
		cancel:  cancel,
		sum:     sha256.New(),
		layout:  make([]Layout, 0, len(sources)),
	}
	for i := range sources {
		s.results[i] = make(chan fetched, 1)
	}
	go func() {
		for i, src := range sources {
			select {
			case s.slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			go func(i int, src Source) {
				data, err := fetch(ctx, src)
				if err != nil {
					err = &SourceError{Index: i, AssetID: src.AssetID, Err: err}
				}
				s.results[i] <- fetched{data: data, err: err}
			}(i, src)
		}
	}()
	return s
}

func fetch(ctx context.Context, src Source) ([]byte, error) {
	body, err := src.Open(ctx)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, src.Size+1))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != src.Size || hex.EncodeToString(sum[:]) != src.SHA256 {
		return nil, ErrSourceMismatch
	}
	return data, nil
}

func (s *Stream) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	for len(s.current) == 0 {
		if s.next == len(s.sources) {
			s.err = io.EOF
			return 0, io.EOF
		}
		r := <-s.results[s.next]
		<-s.slots // the fetched bytes are ours now: the next fetch may start
		if r.err != nil {
			s.err = r.err
			s.Close()
			return 0, r.err
		}
		s.layout = append(s.layout, Layout{Offset: s.offset, Length: int64(len(r.data))})
		s.offset += int64(len(r.data))
		s.current = r.data
		s.next++
	}
	n := copy(p, s.current)
	s.sum.Write(p[:n])
	s.current = s.current[n:]
	return n, nil
}

// Err is the error the stream ended with, if a source failed: a writer
// reading the stream may report only its own failure.
func (s *Stream) Err() error {
	if s.err == nil || errors.Is(s.err, io.EOF) {
		return nil
	}
	return s.err
}

// Close stops fetching. Safe to call more than once.
func (s *Stream) Close() error {
	s.once.Do(s.cancel)
	return nil
}

// Result is the pack's layout and SHA-256, once every source was read (a
// reader that knows the size may stop before io.EOF); an error otherwise.
func (s *Stream) Result() ([]Layout, string, error) {
	finished := s.next == len(s.sources) && len(s.current) == 0
	if !finished || (s.err != nil && !errors.Is(s.err, io.EOF)) {
		return nil, "", errors.New("the pack stream was not read to its end")
	}
	return s.layout, hex.EncodeToString(s.sum.Sum(nil)), nil
}

// TotalSize is the pack size of sources.
func TotalSize(sources []Source) int64 {
	var n int64
	for _, s := range sources {
		n += s.Size
	}
	return n
}

// VerifyPack reads a stored pack and checks it against its manifest:
// every entry's bytes against its checksum, the whole against the pack's
// checksum, and the size exactly.
func VerifyPack(r io.Reader, m Manifest) error {
	whole := sha256.New()
	for _, e := range m.Entries {
		entry := sha256.New()
		n, err := io.Copy(io.MultiWriter(entry, whole), io.LimitReader(r, e.Length))
		if err != nil {
			return err
		}
		if n != e.Length || hex.EncodeToString(entry.Sum(nil)) != e.SHA256 {
			return fmt.Errorf("%w: entry %d (asset %s) does not match", model.ErrPackCorrupt, e.Ordinal, e.AssetID)
		}
	}
	if extra, _ := io.Copy(io.Discard, io.LimitReader(r, 1)); extra != 0 {
		return fmt.Errorf("%w: the pack is longer than its entries", model.ErrPackCorrupt)
	}
	if hex.EncodeToString(whole.Sum(nil)) != m.PackSHA256 {
		return fmt.Errorf("%w: the pack's checksum does not match", model.ErrPackCorrupt)
	}
	return nil
}

// VerifiedEntry wraps a read of one whole entry: it ends with an error,
// not io.EOF, if the bytes are not exactly size bytes with SHA-256 sum.
func VerifiedEntry(body io.ReadCloser, size int64, sum string) io.ReadCloser {
	return &verified{body: body, left: size, want: sum, hash: sha256.New()}
}

type verified struct {
	body io.ReadCloser
	left int64
	want string
	hash hash.Hash
}

func (v *verified) Read(p []byte) (int, error) {
	if int64(len(p)) > v.left+1 {
		p = p[:v.left+1] // one byte more than promised shows a long entry
	}
	n, err := v.body.Read(p)
	v.hash.Write(p[:n])
	v.left -= int64(n)
	if v.left < 0 {
		return n, fmt.Errorf("%w: the entry is longer than recorded", model.ErrPackCorrupt)
	}
	if errors.Is(err, io.EOF) {
		if v.left != 0 {
			return n, io.ErrUnexpectedEOF
		}
		if hex.EncodeToString(v.hash.Sum(nil)) != v.want {
			return n, fmt.Errorf("%w: the entry's checksum does not match", model.ErrPackCorrupt)
		}
	}
	return n, err
}

func (v *verified) Close() error { return v.body.Close() }
