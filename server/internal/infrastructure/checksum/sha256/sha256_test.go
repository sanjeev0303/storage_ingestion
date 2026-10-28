package sha256

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestSumMatchesKnownDigest(t *testing.T) {
	got, err := Sum(context.Background(), strings.NewReader("abc"))
	if err != nil {
		t.Fatal(err)
	}
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

// endless never ends, so only cancellation can stop Sum.
type endless struct{ cancel context.CancelFunc }

func (e endless) Read(p []byte) (int, error) {
	e.cancel()
	return len(p), nil
}

func TestSumStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := Sum(ctx, endless{cancel}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestSumReportsReadErrors(t *testing.T) {
	broken := io.MultiReader(strings.NewReader("abc"), iotestErrReader{})
	if _, err := Sum(context.Background(), broken); err == nil {
		t.Fatal("expected the read error")
	}
}

type iotestErrReader struct{}

func (iotestErrReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// writerToOnly hides everything but WriteTo, as the range reader offers.
type writerToOnly struct{ r *bytes.Reader }

func (w writerToOnly) Read(p []byte) (int, error)           { return w.r.Read(p) }
func (w writerToOnly) WriteTo(dst io.Writer) (int64, error) { return w.r.WriteTo(dst) }

func TestSumOfAWriterToSourceMatchesAPlainRead(t *testing.T) {
	data := bytes.Repeat([]byte("object bytes "), 100_000)
	plain, err := Sum(context.Background(), io.LimitReader(bytes.NewReader(data), int64(len(data))))
	if err != nil {
		t.Fatal(err)
	}
	direct, err := Sum(context.Background(), writerToOnly{bytes.NewReader(data)})
	if err != nil || direct != plain {
		t.Fatalf("WriteTo path gave %q (err %v), plain read gave %q", direct, err, plain)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Sum(ctx, writerToOnly{bytes.NewReader(data)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled hash returned %v, want context.Canceled", err)
	}
}
