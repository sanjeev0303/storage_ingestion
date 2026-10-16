package clamav

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
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

func TestScanVerdicts(t *testing.T) {
	content := strings.Repeat("payload", 30_000) // several 64 KiB chunks

	cases := []struct {
		name      string
		reply     string
		infected  bool
		signature string
		wantErr   bool
	}{
		{"clean", "stream: OK", false, "", false},
		{"infected", "stream: Eicar-Signature FOUND", true, "Eicar-Signature", false},
		{"size limit", "INSTREAM size limit exceeded. ERROR", false, "", true},
		{"no answer", "", false, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			address, got := fakeClamd(t, c.reply)
			verdict, err := New(address, 5*time.Second).Scan(context.Background(), strings.NewReader(content))

			if received := <-got; string(received) != content {
				t.Fatalf("clamd received %d bytes, want %d", len(received), len(content))
			}
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got verdict %+v", verdict)
				}
				return
			}
			if err != nil || verdict.Infected != c.infected || verdict.Signature != c.signature {
				t.Fatalf("got %+v, %v", verdict, err)
			}
		})
	}
}

func TestScanFailsWhenClamdIsUnreachable(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	address := ln.Addr().String()
	ln.Close()

	if _, err := New(address, time.Second).Scan(context.Background(), strings.NewReader("x")); err == nil {
		t.Fatal("an unreachable scanner must be an error, never a clean result")
	}
}

func TestScanStopsOnCancellation(t *testing.T) {
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
	_, err = New(ln.Addr().String(), time.Minute).Scan(ctx, strings.NewReader("x"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the deadline, got %v", err)
	}
}
