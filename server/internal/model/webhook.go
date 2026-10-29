package model

import (
	"encoding/json"
	"errors"
	"time"
)

// Customer webhooks (docs/webhooks.md).

var (
	ErrWebhookNotFound = errors.New("webhook endpoint not found")
	ErrDeliveryNotFound = errors.New("webhook delivery not found")
)

// WebhookEventTypes are the events an endpoint can subscribe to. The
// database triggers of migration 000021 write all but webhook.test, which
// the API sends on request.
var WebhookEventTypes = []string{
	"object.available",
	"object.rejected",
	"object.erased",
	"ingestion.finished",
	"dataset.snapshot_created",
	"dataset.validation_finished",
	"webhook.test",
}

type WebhookEndpoint struct {
	ID          string
	TenantID    string
	URL         string
	Description string
	EventTypes  []string
	Enabled     bool
	// The sealed signing secret.
	SecretKeyVersion int
	SecretNonce      []byte
	SecretCipher     []byte
	CreatedBy        string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type DeliveryStatus string

const (
	HookPending   DeliveryStatus = "PENDING"
	HookDelivered DeliveryStatus = "DELIVERED"
	HookFailed    DeliveryStatus = "FAILED"
)

type WebhookDelivery struct {
	ID             string
	EventID        string
	EndpointID     string
	TenantID       string
	EventType      string
	Payload        json.RawMessage
	Status         DeliveryStatus
	Attempts       int
	NextAttemptAt  time.Time
	LastStatusCode int
	LastError      string
	LastAttemptAt  *time.Time
	DeliveredAt    *time.Time
	CreatedAt      time.Time
}

// DueDelivery is a claimed delivery with the endpoint it goes to.
type DueDelivery struct {
	WebhookDelivery
	Endpoint WebhookEndpoint
}
