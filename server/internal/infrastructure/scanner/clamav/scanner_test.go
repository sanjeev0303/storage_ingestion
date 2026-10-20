package clamav

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// fakeClamd decodes the INSTREAM framing exactly as clamd does and answers
// with reply. It reports the bytes it received on got.
func fakeClamd(t *testing.T, reply string) (string, <-chan []byte) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	got := make(chan []byte, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)

		command, err := r.ReadString(0)
		if err != nil || command != "zINSTREAM\x00" {
			got <- nil
			return
		}
		var received bytes.Buffer
		size := make([]byte, 4)
		for {
			if _, err := io.ReadFull(r, size); err != nil {
				got <- nil
				return
			}
			n := binary.BigEndian.Uint32(size)
			if n == 0 {
				break
			}
			if _, err := io.CopyN(&received, r, int64(n)); err != nil {
				got <- nil
				return
			}
		}
		got <- received.Bytes()
		conn.Write([]byte(reply + "\x00"))
	}()
	return ln.Addr().String(), got
}

func TestScanOutcomes(t *testing.T) {
	content := strings.Repeat("payload", 30_000) // several 64 KiB chunks

	cases := []struct {
		name      string
		reply     string
		outcome   string
		signature string
	}{
		{"clean", "stream: OK", model.ScanClean, ""},
		{"infected", "stream: Eicar-Signature FOUND", model.ScanInfected, "Eicar-Signature"},
		{"size limit", "INSTREAM size limit exceeded. ERROR", model.ScanUnsupported, ""},
		{"scanner error", "stream: Can't allocate memory ERROR", model.ScanError, ""},
		{"no answer", "", model.ScanError, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			address, got := fakeClamd(t, c.reply)
			result := New(address, 5*time.Second).Scan(context.Background(), strings.NewReader(content))

			if received := <-got; string(received) != content {
				t.Fatalf("clamd received %d bytes, want %d", len(received), len(content))
			}
			if result.Outcome != c.outcome || result.Signature != c.signature {
				t.Fatalf("got %+v, want %s %q", result, c.outcome, c.signature)
			}
		})
	}
}

func TestScanIsAnErrorWhenClamdIsUnreachable(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	address := ln.Addr().String()
	ln.Close()

	result := New(address, time.Second).Scan(context.Background(), strings.NewReader("x"))
	if result.Outcome != model.ScanError {
		t.Fatalf("an unreachable scanner must be ERROR, never clean; got %+v", result)
	}
}

func TestScanTimesOut(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			defer conn.Close()
			io.Copy(io.Discard, conn) // reads forever, never answers
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result := New(ln.Addr().String(), time.Minute).Scan(ctx, strings.NewReader("x"))
	if result.Outcome != model.ScanTimeout {
		t.Fatalf("expected TIMEOUT, got %+v", result)
	}
}

func TestPing(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		command, _ := bufio.NewReader(conn).ReadString(0)
		if command == "zPING\x00" {
			conn.Write([]byte("PONG\x00"))
		}
	}()

	if err := New(ln.Addr().String(), time.Second).Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}

	address := ln.Addr().String()
	ln.Close()
	if err := New(address, time.Second).Ping(context.Background()); err == nil {
		t.Fatal("ping to a closed port must fail")
	}
}
