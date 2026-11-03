package osp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The same vector is in internal/platform/fastcdc and sdk/python: a client that
// cuts anywhere else would find nothing to reuse.
const chunkVector = "76aab42460290af8c3707a941931982e59207ffc27641b0681ceb9b96547b260"

func chunkVectorInput() []byte {
	var out bytes.Buffer
	for i := 0; out.Len() < 6<<20; i++ {
		sum := sha256.Sum256([]byte(strconv.Itoa(i)))
		out.Write(sum[:])
	}
	return out.Bytes()[:6<<20]
}

func TestCutsWhereTheServerDoes(t *testing.T) {
	lengths := []string{}
	err := eachChunk(bytes.NewReader(chunkVectorInput()), func(_ int64, data []byte) error {
		lengths = append(lengths, strconv.Itoa(len(data)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(strings.Join(lengths, ",")))
	if got := hex.EncodeToString(sum[:]); got != chunkVector {
		t.Fatalf("got %s for %v", got, lengths)
	}
}

func TestDeltaUploadSendsOnlyWhatIsMissing(t *testing.T) {
	data := chunkVectorInput()
	var sent int
	var manifest struct {
		Chunks []deltaChunk `json:"chunks"`
		SHA256 string       `json:"sha256"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/delta-uploads":
			_ = json.NewDecoder(r.Body).Decode(&manifest)
			// Everything but the first chunk is held already.
			fmt.Fprint(w, `{"id":"d1","missing":[0],"reused_bytes":5,"sent_bytes":6}`)
		case r.Method == http.MethodPut:
			raw, _ := io.ReadAll(r.Body)
			sum := sha256.Sum256(raw)
			if hex.EncodeToString(sum[:]) != manifest.Chunks[0].SHA256 || !strings.HasSuffix(r.URL.Path, "/chunks/0") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			sent += len(raw)
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/complete"):
			fmt.Fprint(w, `{}`)
		default:
			fmt.Fprint(w, `{"upload":{"state":"DONE","upload_id":"u1"}}`)
		}
	}))
	defer srv.Close()
	file := filepath.Join(t.TempDir(), "build.bin")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := New(srv.URL, "token").DeltaUpload(context.Background(), file, DeltaOptions{DatasetID: "d", BaseVersionID: "v", Path: "build.bin"})
	if err != nil {
		t.Fatal(err)
	}
	whole := sha256.Sum256(data)
	if got.UploadID != "u1" || got.SHA256 != hex.EncodeToString(whole[:]) || manifest.SHA256 != got.SHA256 || sent != manifest.Chunks[0].Length {
		t.Fatalf("%+v sent %d", got, sent)
	}
}
