package model

import (
	"errors"
	"time"
)

// Event pipeline errors.
var (
	ErrEventNotFound      = errors.New("event not found")
	ErrDeadLetterNotFound = errors.New("dead letter not found")
	// ErrStaleEvent: the business state moved on; acting on the event now
	// would repeat or reverse a newer decision.
	ErrStaleEvent = errors.New("event is stale")
	// ErrNotReplayable: there is nothing durable to send again (a message
	// that could not be read, or an outbox row already compacted).
	ErrNotReplayable = errors.New("event cannot be replayed")
	// ErrDeadLetterClosed: the dead letter was already retried, replayed,
	// resolved or discarded.
	ErrDeadLetterClosed = errors.New("dead letter is not open")
)

// Outbox statuses.
const (
	OutboxPending   = "PENDING"
	OutboxPublished = "PUBLISHED"
	OutboxDead      = "DEAD"
)

// OutboxEvent is an event committed with the business change that caused
// it, waiting to be published.
type OutboxEvent struct {
	ID                string
	Type              string
	SchemaVersion     int
	AggregateType     string
	AggregateID       string
	AggregateSequence int64
	TenantID          string
	// Payload is the event's JSON payload. It is left out when an event is
	// printed as JSON.
	Payload       []byte `json:"-"`
	CorrelationID string
	CausationID   string
	// Destination is the logical queue the event is published to.
	Destination    string
	Status         string
	Attempts       int
	NextAttemptAt  time.Time
	LastAttemptAt  *time.Time
	LastError      string
	LeaseOwner     string
	LeaseExpiresAt *time.Time
	PublishedAt    *time.Time
	Republished    int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Inbox statuses.
const (
	InboxProcessing = "PROCESSING"
	InboxCompleted  = "COMPLETED"
	// InboxSkipped: done without acting (stale or no longer applicable).
	InboxSkipped = "SKIPPED"
	// InboxFailed: an attempt failed; the next delivery retries it.
	InboxFailed = "FAILED"
	InboxDead   = "DEAD"
)

// InboxRecord is one consumer's processing of one event.
type InboxRecord struct {
	Consumer          string
	EventID           string
	EventType         string
	SchemaVersion     int
	TenantID          string
	AggregateType     string
	AggregateID       string
	AggregateSequence int64
	CorrelationID     string
	Status            string
	Outcome           string
	Attempts          int
	LeaseOwner        string
	LeaseExpiresAt    *time.Time
	NextAttemptAt     *time.Time
	LastError         string
	FirstSeenAt       time.Time
	LastAttemptAt     *time.Time
	CompletedAt       *time.Time
	UpdatedAt         time.Time
}

// What a consumer's claim on an event found.
const (
	// ClaimAcquired: this consumer now processes the event.
	ClaimAcquired = "ACQUIRED"
	// ClaimDone: the event was already completed or skipped.
	ClaimDone = "DONE"
	// ClaimBusy: another worker holds a live lease on it.
	ClaimBusy = "BUSY"
	// ClaimDead: the event was dead-lettered.
	ClaimDead = "DEAD"
)

// Dead-letter sources.
const (
	SourcePublisher = "PUBLISHER"
	SourceConsumer  = "CONSUMER"
	SourceNativeDLQ = "NATIVE_DLQ"
)

// Why an event was dead-lettered.
const (
	FailurePermanent          = "PERMANENT"
	FailureExhausted          = "EXHAUSTED"
	FailureMalformed          = "MALFORMED"
	FailureUnknownType        = "UNKNOWN_TYPE"
	FailureUnsupportedVersion = "UNSUPPORTED_VERSION"
	// FailureRedrive: the queue moved the message to its dead-letter queue
	// after too many receives (a consumer crashing on it, for one).
	FailureRedrive = "REDRIVE"
	// FailureUnconsumed: published repeatedly without any consumer
	// finishing it.
	FailureUnconsumed = "UNCONSUMED"
)

// Dead-letter statuses.
const (
	DeadLetterOpen      = "OPEN"
	DeadLetterRetried   = "RETRIED"
	DeadLetterReplayed  = "REPLAYED"
	DeadLetterResolved  = "RESOLVED"
	DeadLetterDiscarded = "DISCARDED"
)

// DeadLetter is the operational record of an event that could not be
// published or consumed. It never holds the event's payload: that stays
// in the outbox row, referenced by EventID.
type DeadLetter struct {
	ID              string
	EventID         string
	Source          string
	Consumer        string
	EventType       string
	SchemaVersion   int
	TenantID        string
	AggregateType   string
	AggregateID     string
	CorrelationID   string
	FailureCategory string
	LastError       string
	Attempts        int
	EventCreatedAt  *time.Time
	FirstFailedAt   time.Time
	LastFailedAt    time.Time
	DeadLetteredAt  time.Time
	MessageDigest   string
	Status          string
	ResolutionNote  string
	ResolvedBy      string
	ResolvedAt      *time.Time
	ReplayEventID   string
	SourceChanged   string
	UpdatedAt       time.Time
}

// Discrepancy outcomes.
const (
	DiscrepancyRepaired = "REPAIRED"
	DiscrepancyReported = "REPORTED"
	DiscrepancyFailed   = "FAILED"
)

// EventDiscrepancy is something event reconciliation found, and what it
// did about it.
type EventDiscrepancy struct {
	ID            string
	Kind          string
	EventID       string
	Consumer      string
	TenantID      string
	AggregateType string
	AggregateID   string
	Detail        string
	Action        string
	Outcome       string
	CreatedAt     time.Time
}

// OutboxStats summarises the outbox for metrics.
type OutboxStats struct {
	Pending int
	// OldestPending is the age of the oldest unpublished event.
	OldestPending time.Duration
	// ExpiredLeases are pending events whose publisher stopped mid-way.
	ExpiredLeases int
	Dead          int
}

// DeadLetterStats summarises open dead letters for metrics.
type DeadLetterStats struct {
	Open   int
	Oldest time.Duration
}

// UploadFollowUp is a completed upload that has no asset yet, and the ID
// of its latest upload.completed event ("" when it has none).
type UploadFollowUp struct {
	TenantID string
	UploadID string
	EventID  string
}
