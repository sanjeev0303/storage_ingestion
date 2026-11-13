package middleware

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// readAll answers with how much of the body the handler read, and keeps
// working for hold afterwards.
func readAll(hold time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			http.Error(w, "body read failed", http.StatusRequestTimeout)
			return
		}
		select {
		case <-time.After(hold):
		case <-r.Context().Done():
			return // cut off: the client sees no answer
		}
		fmt.Fprintf(w, "%d", n)
	})
}

func TestBodyDeadlineCutsOffATricklingBody(t *testing.T) {
	srv := httptest.NewServer(BodyDeadline(300*time.Millisecond, 1<<20, readAll(0)))
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\n")
	// One byte at a time, far below 1 MiB/s: the deadline runs out.
	started := time.Now()
	for i := 0; i < 50; i++ {
		if _, err := conn.Write([]byte("a")); err != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	answer, _ := io.ReadAll(conn)
	if !strings.Contains(string(answer), "408") && len(answer) != 0 {
		t.Fatalf("a trickling body was read to the end: %q", answer)
	}
	if time.Since(started) > 4*time.Second {
		t.Fatal("the slow body was not cut off")
	}
}

func TestBodyDeadlineLetsAMovingBodyAndALongHandlerFinish(t *testing.T) {
	// A body that keeps pace may take longer than the grace in total, and
	// the handler may keep working after reading it.
	srv := httptest.NewServer(BodyDeadline(200*time.Millisecond, 64<<10, readAll(500*time.Millisecond)))
	defer srv.Close()

	pr, pw := io.Pipe()
	go func() {
		chunk := bytes.Repeat([]byte("b"), 32<<10)
		for i := 0; i < 8; i++ { // 256 KiB over ~0.8 s: 320 KiB/s
			_, _ = pw.Write(chunk)
			time.Sleep(100 * time.Millisecond)
		}
		_ = pw.Close()
	}()
	req, _ := http.NewRequest(http.MethodPost, srv.URL, pr)
	req.ContentLength = 8 * 32 << 10
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != fmt.Sprint(8*32<<10) {
		t.Fatalf("a body that kept moving: %d %q", resp.StatusCode, body)
	}

	// A small body read at once, then work past the grace: the deadline
	// was lifted at the end of the body, so the request is not cancelled.
	resp, err = http.Post(srv.URL, "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "5" {
		t.Fatalf("a handler working after its body: %d %q", resp.StatusCode, body)
	}

	// Requests without a body are left alone.
	resp, err = http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("no body: %d", resp.StatusCode)
	}
}
