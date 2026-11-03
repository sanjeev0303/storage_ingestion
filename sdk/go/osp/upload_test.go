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
