package interfaces

import (
	"context"
	"errors"
	"time"
)

// Queue errors. Adapters wrap provider failures in these, so services can
// decide what to do without knowing the provider.
var (
	// ErrQueueThrottled: the provider asked callers to slow down. Retryable.
	ErrQueueThrottled = errors.New("queue throttled the request")
	// ErrQueueNotFound: the queue does not exist. Retrying cannot help
	// until it is created.
	ErrQueueNotFound = errors.New("queue not found")
	// ErrMessageTooLarge: the message is over the provider's size limit.
	ErrMessageTooLarge = errors.New("message too large for the queue")
	// ErrReceiptExpired: the message's visibility timeout passed, so it
	// may already be with another consumer.
	ErrReceiptExpired = errors.New("message receipt expired")
)

// OutgoingMessage is one message to publish.
type OutgoingMessage struct {
	Body []byte
	// GroupKey orders messages: a queue that supports it delivers messages
	// with the same key in publication order, none while an earlier one is
	// in flight.
	GroupKey string
	// DedupKey lets a queue that supports it drop a repeat of a message
	// sent again shortly after.
	DedupKey string
}

// QueueMessage is one received message.
type QueueMessage struct {
	ID      string
	Body    []byte
	Receipt string
	// ReceiveCount is how often the queue has delivered the message,
	// this delivery included.
	ReceiveCount int
	SentAt       time.Time
	// GroupKey is the message's ordering group, as published. A FIFO queue
	// can return several messages of one group in a single receive; they
	// must be handled one at a time, in the order received.
	GroupKey string
}

// QueueStats is a queue's approximate backlog.
type QueueStats struct {
	Waiting  int // visible, waiting for a consumer
	InFlight int // received and not yet acknowledged
	Delayed  int
	// DeadLetters are in the queue's own dead-letter queue, not yet
	// drained into the dead_letters table.
	DeadLetters int
}

// MessageQueue carries events from the outbox publisher to consumers.
// Delivery is at least once: a message can arrive twice, or late. Ordering
// and deduplication are provider features that may be missing, so
// consumers stay correct without them.
type MessageQueue interface {
	// Publish returns once the provider has accepted the message.
	Publish(ctx context.Context, queue string, msg OutgoingMessage) (messageID string, err error)
	// Receive waits up to wait for at most max messages and hides each one
	// from other consumers for visibility.
	Receive(ctx context.Context, queue string, max int, visibility, wait time.Duration) ([]QueueMessage, error)
	// Ack deletes a message that was processed.
	Ack(ctx context.Context, queue, receipt string) error
	// Retry makes the message visible again after delay.
	Retry(ctx context.Context, queue, receipt string, delay time.Duration) error
	// Extend hides the message for another visibility period while it is
	// still being processed.
	Extend(ctx context.Context, queue, receipt string, visibility time.Duration) error

	// ReceiveDeadLetters reads messages the queue itself moved to its
	// dead-letter queue after too many receives.
	ReceiveDeadLetters(ctx context.Context, queue string, max int, visibility time.Duration) ([]QueueMessage, error)
	// AckDeadLetter deletes a message from the queue's dead-letter queue.
	AckDeadLetter(ctx context.Context, queue, receipt string) error

	Stats(ctx context.Context, queue string) (QueueStats, error)
}
