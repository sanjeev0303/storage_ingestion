package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/secretbox"
)

// Queue is the delivery side of the store.
type Queue interface {
	Claim(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]model.DueDelivery, error)
	Record(ctx context.Context, id, owner string, now time.Time, ok bool, statusCode int, lastError string, next *time.Time) error
}

// retryDelays: after the nth failed attempt, wait retryDelays[n-1]. After
// the last one the delivery is FAILED (about a day and a half in all) and
// waits for a redelivery from the API.
var retryDelays = []time.Duration{
	10 * time.Second, time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour,
}

const (
	sendTimeout = 10 * time.Second
	lease       = time.Minute
	batchSize   = 64
)

// Sender delivers due deliveries. Its HTTP client must be the network
// guard's (netguard.Policy.Client): every dial is checked against private,
// loopback and metadata addresses, and redirects are not followed.
type Sender struct {
	queue       Queue
	sealer      secretbox.Sealer
	client      *http.Client
	owner       string
	concurrency int
	log         *slog.Logger
	now         func() time.Time
}

func NewSender(queue Queue, sealer secretbox.Sealer, client *http.Client, owner string, concurrency int, log *slog.Logger) *Sender {
	return &Sender{queue: queue, sealer: sealer, client: client, owner: owner, concurrency: max(concurrency, 1), log: log,
		now: func() time.Time { return time.Now().UTC() }}
}

// RunOnce sends one batch of due deliveries and returns how many it tried.
func (s *Sender) RunOnce(ctx context.Context) (int, error) {
	due, err := s.queue.Claim(ctx, s.owner, s.now(), lease, batchSize)
	if err != nil {
		return 0, err
	}
	var wg sync.WaitGroup
	slots := make(chan struct{}, s.concurrency)
	for _, d := range due {
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			s.deliver(ctx, d)
		}()
	}
	wg.Wait()
	return len(due), nil
}

// Run sends deliveries until ctx ends: at once when woken, otherwise
// every interval, and again straight away while batches come back full.
func (s *Sender) Run(ctx context.Context, interval time.Duration, wake <-chan struct{}) {
	for ctx.Err() == nil {
		n, err := s.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			s.log.Error("webhook delivery batch failed", "error", retry.Sanitize(err))
		}
		if n == batchSize {
			continue
		}
		select {
		case <-ctx.Done():
		case <-wake:
		case <-time.After(interval):
		}
	}
}

func (s *Sender) deliver(ctx context.Context, d model.DueDelivery) {
	code, err := s.send(ctx, d)
	if ctx.Err() != nil {
		return // shutting down: the lease runs out and it is sent again
	}
	now := s.now()
	var next *time.Time
	message := ""
	if err != nil {
		message = retry.Sanitize(err)
		if d.Attempts < len(retryDelays) {
			at := now.Add(retryDelays[d.Attempts])
			next = &at
		}
		s.log.Warn("webhook delivery failed", "delivery_id", d.ID, "endpoint_id", d.EndpointID,
			"attempt", d.Attempts+1, "status", code, "error", message)
	}
	if err := s.queue.Record(ctx, d.ID, s.owner, now, err == nil, code, message, next); err != nil {
		s.log.Error("record webhook delivery", "delivery_id", d.ID, "error", retry.Sanitize(err))
	}
}

// Body is what an endpoint receives.
type Body struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	CreatedAt time.Time       `json:"created_at"`
	Data      json.RawMessage `json:"data"`
}

func (s *Sender) send(ctx context.Context, d model.DueDelivery) (int, error) {
	if s.sealer == nil {
		return 0, ErrNotConfigured
	}
	e := d.Endpoint
	secret, err := s.sealer.Open(secretbox.Sealed{KeyVersion: e.SecretKeyVersion, Nonce: e.SecretNonce, Ciphertext: e.SecretCipher},
		associatedData(&e))
	if err != nil {
		return 0, fmt.Errorf("open the signing secret: %w", err)
	}
	body, err := json.Marshal(Body{ID: d.EventID, Type: d.EventType, CreatedAt: d.CreatedAt, Data: d.Payload})
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	timestamp := s.now().Unix()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "object-storage-webhooks/1")
	req.Header.Set("webhook-id", d.EventID)
	req.Header.Set("webhook-timestamp", strconv.FormatInt(timestamp, 10))
	req.Header.Set("webhook-signature", Sign(secret, d.EventID, timestamp, body))
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("the endpoint answered HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

// Sign is the Standard Webhooks signature: HMAC-SHA256 over
// "id.timestamp.body" with the secret's bytes, as "v1,<base64>".
func Sign(secret []byte, id string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(id + "." + strconv.FormatInt(timestamp, 10) + "."))
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
