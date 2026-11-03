package osp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

// DeltaOptions says what a delta upload is relative to (docs/chunks.md).
type DeltaOptions struct {
	DatasetID     string
	BaseVersionID string
	Path          string
	ContentType   string
	// Poll is how often to ask whether the file has been built; 2s by default.
	Poll time.Duration
}

// DeltaResult is how a delta upload went.
type DeltaResult struct {
	ID          string
	UploadID    string
	SHA256      string
	SizeBytes   int64
	ReusedBytes int64
	SentBytes   int64
}

const deltaSendsAtOnce = 8

type deltaChunk struct {
	Offset int64  `json:"offset"`
	Length int    `json:"length"`
	SHA256 string `json:"sha256"`
}

// DeltaUpload sends a file as what it does not share with a snapshot the caller
// can read, and waits until the server has built it. The file then goes through
// the same checks as any upload.
func (c *Client) DeltaUpload(ctx context.Context, file string, opts DeltaOptions) (DeltaResult, error) {
	f, err := os.Open(file)
	if err != nil {
		return DeltaResult{}, err
	}
	defer f.Close()
	chunks := []deltaChunk{}
	whole := sha256.New()
	var size int64
	err = eachChunk(f, func(offset int64, data []byte) error {
		sum := sha256.Sum256(data)
		whole.Write(data)
		chunks = append(chunks, deltaChunk{Offset: offset, Length: len(data), SHA256: hex.EncodeToString(sum[:])})
		size += int64(len(data))
		return nil
	})
	if err != nil {
		return DeltaResult{}, err
	}
	contentType := opts.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	out := DeltaResult{SHA256: hex.EncodeToString(whole.Sum(nil)), SizeBytes: size}
	var created struct {
		ID          string `json:"id"`
		Missing     []int  `json:"missing"`
		ReusedBytes int64  `json:"reused_bytes"`
		SentBytes   int64  `json:"sent_bytes"`
	}
	err = c.Do(ctx, http.MethodPost, "/api/v1/delta-uploads", map[string]any{"dataset_id": opts.DatasetID, "base_version_id": opts.BaseVersionID,
		"path": opts.Path, "size_bytes": size, "sha256": out.SHA256, "content_type": contentType, "chunks": chunks}, &created)
	if err != nil {
		return out, err
	}
	out.ID, out.ReusedBytes, out.SentBytes = created.ID, created.ReusedBytes, created.SentBytes
	// Several at once: a chunk is about a megabyte, and a round trip each adds up.
	send, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var once sync.Once
	var sendErr error
	slots := make(chan struct{}, deltaSendsAtOnce)
	for _, ordinal := range created.Missing {
		select {
		case slots <- struct{}{}:
		case <-send.Done():
		}
		if send.Err() != nil {
			break
		}
		wg.Add(1)
		go func(ordinal int) {
			defer wg.Done()
			defer func() { <-slots }()
			chunk := chunks[ordinal]
			data := make([]byte, chunk.Length)
			_, err := f.ReadAt(data, chunk.Offset)
			if err == nil {
				err = c.putDeltaChunk(send, created.ID, ordinal, data)
			}
			if err != nil {
				once.Do(func() { sendErr = err; cancel() })
			}
		}(ordinal)
	}
	wg.Wait()
	if sendErr != nil {
		return out, sendErr
	}
	if err := c.Do(ctx, http.MethodPost, "/api/v1/delta-uploads/"+created.ID+"/complete", map[string]any{}, nil); err != nil {
		return out, err
	}
	poll := opts.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	for {
		var status struct {
			Upload struct {
				State    string `json:"state"`
				UploadID string `json:"upload_id"`
				Error    string `json:"error"`
			} `json:"upload"`
		}
		if err := c.Do(ctx, http.MethodGet, "/api/v1/delta-uploads/"+created.ID, nil, &status); err != nil {
			return out, err
		}
		switch status.Upload.State {
		case "DONE":
			out.UploadID = status.Upload.UploadID
			return out, nil
		case "FAILED", "EXPIRED":
			return out, fmt.Errorf("delta upload %s: %s", status.Upload.State, status.Upload.Error)
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (c *Client) putDeltaChunk(ctx context.Context, id string, ordinal int, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, fmt.Sprintf("%s/api/v1/delta-uploads/%s/chunks/%d", c.BaseURL, id, ordinal), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return &APIError{Status: resp.StatusCode, Message: "the chunk was refused"}
	}
	return nil
}
