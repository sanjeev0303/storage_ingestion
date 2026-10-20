package sqs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsretry "github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/smithy-go"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

// Retry makes a message visible again after delay: SQS redelivers it then.
// On a FIFO queue the message's group waits for it, which keeps order.
func (q *Queue) Retry(ctx context.Context, queue, receipt string, delay time.Duration) error {
	return q.changeVisibility(ctx, queue, "retry on "+queue, receipt, delay)
}

func (q *Queue) changeVisibility(ctx context.Context, queue, operation, receipt string, visibility time.Duration) error {
	url, err := q.url(ctx, q.queueName(queue))
	if err != nil {
		return err
	}
	_, err = q.client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl:          aws.String(url),
		ReceiptHandle:     aws.String(receipt),
		VisibilityTimeout: seconds(visibility, maxVisibility),
	})
	if err != nil {
		return classifyReceipt(operation, err)
	}
	return nil
}

// classify maps an SQS failure to the queue errors services understand.
// Only the provider's error code is kept, never its message. A missing
// queue is retried within the caller's budget: it is a deployment problem
// that someone fixes, not a property of the event.
func classify(operation string, err error) error {
	var missing *types.QueueDoesNotExist
	if errors.As(err, &missing) {
		return fmt.Errorf("%s: %w", operation, interfaces.ErrQueueNotFound)
	}

	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		// Network failures and timeouts: retryable.
		return fmt.Errorf("%s: %w", operation, err)
	}
	code := apiErr.ErrorCode()
	switch {
	case code == "AWS.SimpleQueueService.NonExistentQueue":
		return fmt.Errorf("%s: %w", operation, interfaces.ErrQueueNotFound)
	case isThrottle(code):
		return fmt.Errorf("%s: %s: %w", operation, code, interfaces.ErrQueueThrottled)
	case permanentCodes[code]:
		return retry.Permanent(fmt.Errorf("%s: %s", operation, code))
	}
	return fmt.Errorf("%s: provider error %s", operation, code)
}

// classifyReceipt is classify for calls that name a received message: a
// rejected receipt means its visibility timeout passed.
func classifyReceipt(operation string, err error) error {
	var invalid *types.ReceiptHandleIsInvalid
	var notInFlight *types.MessageNotInflight
	if errors.As(err, &invalid) || errors.As(err, &notInFlight) {
		return fmt.Errorf("%s: %w", operation, interfaces.ErrReceiptExpired)
	}
	// SQS answers a visibility change for a message that is no longer in
	// flight with InvalidParameterValue.
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "InvalidParameterValue" {
		return fmt.Errorf("%s: %w", operation, interfaces.ErrReceiptExpired)
	}
	return classify(operation, err)
}

func isThrottle(code string) bool {
	if _, ok := awsretry.DefaultThrottleErrorCodes[code]; ok {
		return true
	}
	// KmsThrottled: the queue's encryption key is rate limited.
	// OverLimit: too many messages in flight.
	return code == "KmsThrottled" || code == "OverLimit"
}

// permanentCodes are requests retrying cannot fix: the message itself, or
// the call, is invalid.
//
// Credential, permission and encryption-key failures are not here. Like a
// missing queue they are a deployment problem that someone fixes, not a
// property of the event, so they stay retryable within the caller's budget.
// Dead-lettering on the first try would turn one bad IAM change or key
// rotation into every event of the outage, each needing an operator retry.
var permanentCodes = map[string]bool{
	"InvalidParameterValue":  true,
	"InvalidMessageContents": true,
	"MissingParameter":       true,
	"InvalidAddress":         true,
	"UnsupportedOperation":   true,
}
