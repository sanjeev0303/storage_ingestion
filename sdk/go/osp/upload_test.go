package osp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Parts go several at once, and the upload still completes with every part in order.
func TestPartsAreSentSideBySide(t *testing.T) {
	const partSize = 1 << 20
	data := bytes.Repeat([]byte("0123456789abcdef"), 9*partSize/16+100)
	var mu sync.Mutex
	parts := map[int][]byte{}
	inFlight, most := 0, 0
	var completed []struct {
		PartNumber int    `json:"part_number"`
		ETag       string `json:"etag"`
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/uploads/multipart":
			fmt.Fprint(w, `{"upload_id":"u1"}`)
		case strings.HasSuffix(r.URL.Path, "/parts/urls"):
			var in struct {
				PartNumbers []int `json:"part_numbers"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			var out []map[string]any
			for _, n := range in.PartNumbers {
				out = append(out, map[string]any{"part_number": n, "url": srv.URL + "/put/" + strconv.Itoa(n)})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"parts": out})
		case r.Method == http.MethodPut:
			mu.Lock()
			inFlight++
			most = max(most, inFlight)
			mu.Unlock()
			raw, _ := io.ReadAll(r.Body)
			time.Sleep(30 * time.Millisecond)
			n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/put/"))
			mu.Lock()
			inFlight--
			parts[n] = raw
			mu.Unlock()
			w.Header().Set("ETag", `"e`+strconv.Itoa(n)+`"`)
		case strings.HasSuffix(r.URL.Path, "/parts/confirm"):
			fmt.Fprint(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/complete"):
			var in struct {
				Parts []struct {
					PartNumber int    `json:"part_number"`
					ETag       string `json:"etag"`
				} `json:"parts"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			completed = in.Parts
			fmt.Fprint(w, `{}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	file := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := New(srv.URL, "token").UploadFile(context.Background(), file, UploadOptions{Dataset: "d", PartSize: partSize}); err != nil {
		t.Fatal(err)
	}
	var joined []byte
	for i, p := range completed {
		if p.PartNumber != i+1 || p.ETag != `"e`+strconv.Itoa(i+1)+`"` {
			t.Fatalf("completed %+v", completed)
		}
		joined = append(joined, parts[p.PartNumber]...)
	}
	if len(completed) != 10 || !bytes.Equal(joined, data) {
		t.Fatalf("%d parts, %d of %d bytes match", len(completed), len(joined), len(data))
	}
	if most < 2 || most > partsAtOnce {
		t.Fatalf("%d parts in flight at most, want 2 to %d", most, partsAtOnce)
	}
}

// flakyStorage is an API and storage fake whose PUTs are answered by
// storage(n, path, w), n counting from 1; it returns false to answer 200.
func flakyStorage(t *testing.T, storage func(n int, path string, w http.ResponseWriter) bool) (*httptest.Server, *[]string) {
	var mu sync.Mutex
	var log []string
	puts := 0
	version := 0
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		log = append(log, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.Method == http.MethodPut:
			_, _ = io.Copy(io.Discard, r.Body)
			mu.Lock()
			puts++
			n := puts
			mu.Unlock()
			if storage(n, r.URL.Path, w) {
				return
			}
			w.Header().Set("ETag", `"e"`)
		case r.URL.Path == "/api/v1/uploads":
			mu.Lock()
			version++
			fmt.Fprintf(w, `{"upload_id":"s1","upload_url":"%s/put/single-v%d"}`, srv.URL, version)
			mu.Unlock()
		case r.URL.Path == "/api/v1/uploads/multipart":
			fmt.Fprint(w, `{"upload_id":"u1"}`)
		case strings.HasSuffix(r.URL.Path, "/parts/urls"):
			var in struct {
				PartNumbers []int `json:"part_numbers"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			mu.Lock()
			version++
			var out []map[string]any
			for _, n := range in.PartNumbers {
				out = append(out, map[string]any{"part_number": n, "url": fmt.Sprintf("%s/put/%d-v%d", srv.URL, n, version)})
			}
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"parts": out})
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &log
}

func putsIn(log []string) []string {
	var out []string
	for _, line := range log {
		if strings.HasPrefix(line, "PUT ") {
			out = append(out, strings.TrimPrefix(line, "PUT /put/"))
		}
	}
	return out
}

func writeFile(t *testing.T, size int) string {
	file := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(file, bytes.Repeat([]byte("x"), size), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestStalledPutIsDroppedAndSentAgain(t *testing.T) {
	srv, log := flakyStorage(t, func(n int, _ string, w http.ResponseWriter) bool {
		if n == 1 {
			time.Sleep(time.Second)
		}
		return false
	})
	_, err := New(srv.URL, "t").UploadFile(context.Background(), writeFile(t, 100), UploadOptions{StallTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if puts := putsIn(*log); len(puts) != 2 {
		t.Fatalf("puts %v, want the stalled one and one more", puts)
	}
}

func TestExpiredPartURLIsReplaced(t *testing.T) {
	srv, log := flakyStorage(t, func(n int, _ string, w http.ResponseWriter) bool {
		if n == 1 {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, "<Message>Request has expired</Message>")
			return true
		}
		return false
	})
	_, err := New(srv.URL, "t").UploadFile(context.Background(), writeFile(t, 3<<20), UploadOptions{PartSize: 1 << 20, Attempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	// Parts go side by side, and the log order need not match the order the
	// refusal was counted in, so look for the one part that was sent again.
	puts := putsIn(*log)
	var resent []string
	for _, put := range puts {
		if strings.HasSuffix(put, "-v2") {
			resent = append(resent, put)
		}
	}
	if len(puts) != 4 || len(resent) != 1 || !slices.Contains(puts, strings.TrimSuffix(resent[0], "v2")+"v1") {
		t.Fatalf("puts %v, want the refused part once more on a fresh link", puts)
	}
}

func TestRefusalIsNotRetriedAndRetryAfterIsRespected(t *testing.T) {
	srv, log := flakyStorage(t, func(int, string, http.ResponseWriter) bool { return false })
	srv.Config.Handler = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut {
				*log = append(*log, "PUT "+r.URL.Path)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			next.ServeHTTP(w, r)
		})
	}(srv.Config.Handler)
	if _, err := New(srv.URL, "t").UploadFile(context.Background(), writeFile(t, 100), UploadOptions{}); err == nil {
		t.Fatal("a 400 from storage must fail the upload")
	}
	if puts := putsIn(*log); len(puts) != 1 {
		t.Fatalf("puts %v, want one", puts)
	}

	busy, busyLog := flakyStorage(t, func(n int, _ string, w http.ResponseWriter) bool {
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		return false
	})
	start := time.Now()
	if _, err := New(busy.URL, "t").UploadFile(context.Background(), writeFile(t, 100), UploadOptions{}); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited < time.Second {
		t.Fatalf("waited %v, want the second Retry-After asked for", waited)
	}
	if puts := putsIn(*busyLog); len(puts) != 2 {
		t.Fatalf("puts %v", puts)
	}
}

func TestBackoffIsFullJitterUpToThirtySeconds(t *testing.T) {
	for attempt := 0; attempt < 20; attempt++ {
		limit := min(30*time.Second, time.Second<<attempt)
		for i := 0; i < 50; i++ {
			if d := backoff(attempt); d < 0 || d > limit {
				t.Fatalf("attempt %d: %v outside 0 to %v", attempt, d, limit)
			}
		}
	}
}

func TestProgressFileLetsARerunContinue(t *testing.T) {
	broken := true
	srv, log := flakyStorage(t, func(_ int, path string, w http.ResponseWriter) bool {
		if broken && strings.HasPrefix(path, "/put/3-") {
			// Late, so the other parts are confirmed and saved first.
			time.Sleep(300 * time.Millisecond)
			w.WriteHeader(http.StatusBadRequest)
			return true
		}
		return false
	})
	file := writeFile(t, 3<<20+10)
	state := filepath.Join(t.TempDir(), "progress.json")
	progress, err := OpenProgressFile(state)
	if err != nil {
		t.Fatal(err)
	}
	opts := UploadOptions{PartSize: 1 << 20, Store: progress, Resume: progress.Resume()}
	if _, err := New(srv.URL, "t").UploadFile(context.Background(), file, opts); err == nil {
		t.Fatal("part 3 was refused; the first run must fail")
	}

	broken = false
	*log = nil
	again, err := OpenProgressFile(state)
	if err != nil {
		t.Fatal(err)
	}
	saved := again.Resume()
	if saved == nil || saved.UploadID != "u1" || len(saved.Parts) != 3 || saved.Parts[3] != "" {
		t.Fatalf("saved progress %+v, want every part but 3", saved)
	}
	opts = UploadOptions{PartSize: 1 << 20, Store: again, Resume: saved}
	if _, err := New(srv.URL, "t").UploadFile(context.Background(), file, opts); err != nil {
		t.Fatal(err)
	}
	if puts := putsIn(*log); len(puts) != 1 || !strings.HasPrefix(puts[0], "3-") {
		t.Fatalf("puts %v, want only part 3", puts)
	}
	if err := again.Remove(); err != nil {
		t.Fatal(err)
	}
}

func TestProgressForAnotherFileIsNotResumed(t *testing.T) {
	srv, log := flakyStorage(t, func(int, string, http.ResponseWriter) bool { return false })
	file := writeFile(t, 2<<20)
	state := filepath.Join(t.TempDir(), "progress.json")
	// Saved for a file of the same size whose bytes have since changed.
	saved := fmt.Sprintf(`{"upload_id":"old","part_size":%d,"parts":{"1":"\"e\""},"sha256":"%s","size":%d}`, 1<<20, strings.Repeat("b", 64), 2<<20)
	if err := os.WriteFile(state, []byte(saved), 0o600); err != nil {
		t.Fatal(err)
	}
	progress, err := OpenProgressFile(state)
	if err != nil {
		t.Fatal(err)
	}
	opts := UploadOptions{PartSize: 1 << 20, Store: progress, Resume: progress.Resume()}
	result, err := New(srv.URL, "t").UploadFile(context.Background(), file, opts)
	if err != nil {
		t.Fatal(err)
	}
	if result.UploadID != "u1" || slices.ContainsFunc(*log, func(line string) bool { return strings.Contains(line, "/uploads/old") }) {
		t.Fatalf("upload %s, requests %v: want a new upload and nothing sent to the old one", result.UploadID, *log)
	}
	if again := progress.Resume(); again == nil || again.SHA256 != result.SHA256 || again.Size != 2<<20 {
		t.Fatalf("saved progress %+v, want it to name this file", again)
	}
}

func TestEndedSessionStartsAgainOnceUnderANewKey(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/uploads/multipart":
			var in struct {
				IdempotencyKey string `json:"idempotency_key"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			mu.Lock()
			keys = append(keys, in.IdempotencyKey)
			first := len(keys) == 1
			mu.Unlock()
			// The first key belongs to a session that has since expired.
			if first {
				fmt.Fprint(w, `{"upload_id":"old"}`)
			} else {
				fmt.Fprint(w, `{"upload_id":"new"}`)
			}
		case strings.HasPrefix(r.URL.Path, "/api/v1/uploads/old/"):
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error":"multipart upload is not active"}`)
		case strings.HasSuffix(r.URL.Path, "/parts/urls"):
			var in struct {
				PartNumbers []int `json:"part_numbers"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			var out []map[string]any
			for _, n := range in.PartNumbers {
				out = append(out, map[string]any{"part_number": n, "url": srv.URL + "/put/" + strconv.Itoa(n)})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"parts": out})
		case r.Method == http.MethodPut:
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("ETag", `"e"`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)

	progress, err := OpenProgressFile(filepath.Join(t.TempDir(), "progress.json"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := New(srv.URL, "t").UploadFile(context.Background(), writeFile(t, 2<<20), UploadOptions{PartSize: 1 << 20, Store: progress})
	if err != nil {
		t.Fatal(err)
	}
	if result.UploadID != "new" || len(keys) != 2 || keys[0] == keys[1] {
		t.Fatalf("upload %s with keys %v, want a second start under a new key", result.UploadID, keys)
	}
	if saved := progress.Resume(); saved == nil || saved.UploadID != "new" {
		t.Fatalf("saved progress %+v, want the new upload", saved)
	}
}

func TestStorageAccessDeniedIsARefusal(t *testing.T) {
	srv, log := flakyStorage(t, func(_ int, _ string, w http.ResponseWriter) bool {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>")
		return true
	})
	if _, err := New(srv.URL, "t").UploadFile(context.Background(), writeFile(t, 100), UploadOptions{}); err == nil {
		t.Fatal("a refused PUT must fail the upload")
	}
	if puts := putsIn(*log); len(puts) != 1 {
		t.Fatalf("puts %v, want one: a refusal is not an expired link", puts)
	}
}

// A replayed start whose session has ended answers 409 session_ended: the
// single PUT starts once more under a new key, as the parts do.
func TestEndedSingleSessionStartsAgainUnderANewKey(t *testing.T) {
	var mu sync.Mutex
	var keys, log []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		log = append(log, r.Method+" "+r.URL.Path)
		switch {
		case r.URL.Path == "/api/v1/uploads":
			var in struct {
				IdempotencyKey string `json:"idempotency_key"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			keys = append(keys, in.IdempotencyKey)
			if len(keys) == 1 {
				w.WriteHeader(http.StatusConflict)
				fmt.Fprint(w, `{"code":"session_ended","error":"upload session is not active: it has ended; start again with a new idempotency key"}`)
				return
			}
			fmt.Fprintf(w, `{"upload_id":"new","status":"PENDING","upload_url":"%s/put/new"}`, srv.URL)
		case r.Method == http.MethodPut:
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("ETag", `"e"`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)

	result, err := New(srv.URL, "t").UploadFile(context.Background(), writeFile(t, 100), UploadOptions{Dataset: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if result.UploadID != "new" || len(keys) != 2 || keys[0] == keys[1] || !slices.Contains(log, "POST /api/v1/uploads/new/complete") {
		t.Fatalf("upload %s with keys %v, requests %v: want a second start under a new key", result.UploadID, keys, log)
	}
}

// A replayed start of an upload that already completed answers its status and
// no URL: there is nothing left to send.
func TestACompletedSingleUploadIsNotSentAgain(t *testing.T) {
	var mu sync.Mutex
	var log []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		log = append(log, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/api/v1/uploads" {
			fmt.Fprint(w, `{"upload_id":"done","status":"COMPLETED","upload_url":""}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"unexpected"}`)
	}))
	t.Cleanup(srv.Close)

	result, err := New(srv.URL, "t").UploadFile(context.Background(), writeFile(t, 100), UploadOptions{Dataset: "d"})
	if err != nil {
		t.Fatalf("%v; requests %v", err, log)
	}
	if result.UploadID != "done" || len(log) != 1 {
		t.Fatalf("upload %s, requests %v: want the start alone", result.UploadID, log)
	}
}

// Progress read back before any part was saved has no parts map at all (the
// agent's journal leaves an empty one out): it continues all the same.
func TestProgressWithNoPartsYetContinues(t *testing.T) {
	srv, _ := flakyStorage(t, func(int, string, http.ResponseWriter) bool { return false })
	file := writeFile(t, 2<<20)
	sum, size, err := SHA256File(file)
	if err != nil {
		t.Fatal(err)
	}
	resume := &Progress{UploadID: "u1", PartSize: 1 << 20, SHA256: sum, Size: size}
	if _, err := New(srv.URL, "t").UploadFile(context.Background(), file, UploadOptions{PartSize: 1 << 20, Resume: resume}); err != nil {
		t.Fatal(err)
	}
}
