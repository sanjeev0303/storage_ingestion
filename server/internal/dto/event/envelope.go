// Package event defines the events the platform publishes through the
// transactional outbox: one versioned envelope, a catalog of the known
// event types, and a typed payload for each.
//
// Compatibility rules: a payload change that old consumers can read
// (a new optional field) keeps the schema version, since unknown JSON
// fields are ignored. Removing, renaming or retyping a field, or changing
// its meaning, needs a new version; consumers learn the new version first,
// and producers switch to it only after every consumer is deployed.
package event

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Why a message cannot be handled. Each is permanent: delivering the same
// bytes again cannot change the answer.
var (
	ErrMalformed          = errors.New("malformed event")
	ErrUnknownType        = errors.New("unknown event type")
	ErrUnsupportedVersion = errors.New("unsupported event schema version")
)

// Queues events are published to. Each queue has one consumer, named
// after the queue.
const (
	QueueProcessing = "processing"
	QueueLifecycle  = "lifecycle"
)

// Aggregates: the records whose events must be handled in order.
const (
	AggregateUpload    = "upload"
	AggregateAsset     = "asset"
	AggregateLegalHold = "legal_hold"
)

// Envelope is the message every event travels in. Payloads carry
// identifiers only: never file content, signed URLs or credentials.
type Envelope struct {
	ID                string          `json:"id"`
	Type              string          `json:"type"`
	SchemaVersion     int             `json:"schema_version"`
	OccurredAt        time.Time       `json:"occurred_at"`
	AggregateType     string          `json:"aggregate_type"`
	AggregateID       string          `json:"aggregate_id"`
	AggregateSequence int64           `json:"aggregate_sequence"`
	TenantID          string          `json:"tenant_id,omitempty"`
	CorrelationID     string          `json:"correlation_id,omitempty"`
	CausationID       string          `json:"causation_id,omitempty"`
	Payload           json.RawMessage `json:"payload"`
}

type payload interface{ validate() error }

// spec is what the catalog knows about one event type.
type spec struct {
	queue string
	// current is the version producers write; versions are the ones
	// consumers can read.
	current    int
	versions   []int
	newPayload func() payload
}

var catalog = map[string]spec{
	TypeUploadCompleted:   {QueueProcessing, 1, []int{1}, func() payload { return &UploadCompleted{} }},
	TypeDeletionScheduled: {QueueLifecycle, 1, []int{1}, func() payload { return &DeletionScheduled{} }},
	TypeHoldReleased:      {QueueLifecycle, 1, []int{1}, func() payload { return &HoldReleased{} }},
	TypeVersionReplaced:   {QueueLifecycle, 1, []int{1}, func() payload { return &VersionReplaced{} }},
	TypeManifestStaged:    {QueueIngestion, 1, []int{1}, func() payload { return &ManifestStaged{} }},
	TypeUploadProcessed:   {QueueIngestion, 1, []int{1}, func() payload { return &UploadProcessed{} }},
	TypeImportQueued:      {QueueDatasets, 1, []int{1}, func() payload { return &ImportQueued{} }},
	TypeValidationQueued:  {QueueDatasets, 1, []int{1}, func() payload { return &ValidationQueued{} }},
}

// QueueOf returns the queue an event type is published to.
func QueueOf(eventType string) (string, bool) {
	s, ok := catalog[eventType]
	return s.queue, ok
}

// Decode parses and validates a message body. When validation fails the
// envelope still holds what could be read, so the failure can be recorded
// against the event's ID.
func Decode(body []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return Envelope{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return env, env.Validate()
}

// Validate checks the required fields, that the type and schema version
// are known, and that the payload is complete.
func (e Envelope) Validate() error {
	if e.ID == "" || e.Type == "" || e.AggregateType == "" || e.AggregateID == "" || e.TenantID == "" {
		return fmt.Errorf("%w: id, type, aggregate and tenant are required", ErrMalformed)
	}
	if e.SchemaVersion <= 0 || e.AggregateSequence <= 0 || e.OccurredAt.IsZero() {
		return fmt.Errorf("%w: schema_version, aggregate_sequence and occurred_at are required", ErrMalformed)
	}
	s, ok := catalog[e.Type]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownType, e.Type)
	}
	if !slices.Contains(s.versions, e.SchemaVersion) {
		return fmt.Errorf("%w: %s version %d", ErrUnsupportedVersion, e.Type, e.SchemaVersion)
	}
	return DecodePayload(e, s.newPayload())
}

// DecodePayload reads the envelope's payload into target, one of this
// package's payload types, and checks it.
func DecodePayload(e Envelope, target payload) error {
	if err := json.Unmarshal(e.Payload, target); err != nil {
		return fmt.Errorf("%w: payload: %v", ErrMalformed, err)
	}
	if err := target.validate(); err != nil {
		return fmt.Errorf("%w: payload: %v", ErrMalformed, err)
	}
	return nil
}

// FromOutbox is the envelope the publisher sends for an outbox row.
func FromOutbox(e model.OutboxEvent) Envelope {
	return Envelope{
		ID: e.ID, Type: e.Type, SchemaVersion: e.SchemaVersion, OccurredAt: e.CreatedAt,
		AggregateType: e.AggregateType, AggregateID: e.AggregateID, AggregateSequence: e.AggregateSequence,
		TenantID: e.TenantID, CorrelationID: e.CorrelationID, CausationID: e.CausationID,
		Payload: json.RawMessage(e.Payload),
	}
}

// newEvent builds the outbox row for a catalog event. The aggregate
// sequence is assigned when the row is written.
func newEvent(eventType, tenantID, aggregateType, aggregateID string, body payload, at time.Time, correlationID string) model.OutboxEvent {
	s := catalog[eventType]
	// The payloads are plain structs of strings: encoding cannot fail.
	raw, _ := json.Marshal(body)
	return model.OutboxEvent{
		ID: NewID(), Type: eventType, SchemaVersion: s.current,
		AggregateType: aggregateType, AggregateID: aggregateID, TenantID: tenantID,
		Payload: raw, CorrelationID: correlationID, Destination: s.queue,
		Status: model.OutboxPending, NextAttemptAt: at, CreatedAt: at, UpdatedAt: at,
	}
}

// Replay copies an event under a new identity, for an operator who means
// a new logical operation. The copy names the original as its cause.
func Replay(original model.OutboxEvent, at time.Time) model.OutboxEvent {
	return model.OutboxEvent{
		ID: NewID(), Type: original.Type, SchemaVersion: original.SchemaVersion,
		AggregateType: original.AggregateType, AggregateID: original.AggregateID, TenantID: original.TenantID,
		Payload: original.Payload, CorrelationID: original.CorrelationID, CausationID: original.ID,
		Destination: original.Destination, Status: model.OutboxPending, NextAttemptAt: at, CreatedAt: at, UpdatedAt: at,
	}
}

// NewID returns a random event ID.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
