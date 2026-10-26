package metrics

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func get(t *testing.T, url string) (int, string, error) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), nil
}

func TestServeAnswersLivenessReadinessAndMetricsUntilStopped(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()

	r := New()
	r.Add("served_test_total", 3)
	r.Publish("serve_test")
	var dbDown atomic.Bool
	ready := func(context.Context) error {
		if dbDown.Load() {
			return errors.New("connection refused by 10.0.0.5")
		}
		return nil
	}
	stop := Serve(addr, ready, slog.New(slog.NewTextHandler(io.Discard, nil)))
	base := "http://" + addr
	for range 50 { // the listener starts in the background
		if _, _, err := get(t, base+"/health"); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if code, _, err := get(t, base+"/health"); err != nil || code != http.StatusOK {
		t.Fatalf("liveness: %d %v", code, err)
	}
	if code, _, err := get(t, base+"/ready"); err != nil || code != http.StatusOK {
		t.Fatalf("readiness: %d %v", code, err)
	}
	dbDown.Store(true)
	code, body, err := get(t, base+"/ready")
	if err != nil || code != http.StatusServiceUnavailable || strings.Contains(body, "10.0.0.5") {
		t.Fatalf("not ready, without the cause: %d %q %v", code, body, err)
	}
	if code, _, _ := get(t, base+"/health"); code != http.StatusOK {
		t.Fatal("a database outage is not a liveness failure")
	}
	if code, body, err := get(t, base+"/debug/vars"); err != nil || code != http.StatusOK || !strings.Contains(body, "served_test_total") {
		t.Fatalf("metrics: %d %v", code, err)
	}

	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not return")
	}
	if _, _, err := get(t, base+"/health"); err == nil {
		t.Fatal("the listener still answers after stop")
	}
}
