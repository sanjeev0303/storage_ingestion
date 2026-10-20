package sqs

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

// Publish sends one message. The group key becomes the FIFO message group
// and the dedup key the deduplication ID, so a publisher that sends the
// same message again after a crash is deduplicated within five minutes.
func (q *Queue) Publish(ctx context.Context, queue string, msg interfaces.OutgoingMessage) (string, error) {
	operation := "publish to " + queue
	if len(msg.Body) > maxMessageBytes {
		return "", retry.Permanent(fmt.Errorf("%s: %w", operation, interfaces.ErrMessageTooLarge))
	}
	if msg.GroupKey == "" || msg.DedupKey == "" {
		return "", retry.Permanent(fmt.Errorf("%s: %w", operation, errors.New("a FIFO queue needs a group key and a dedup key")))
	}

	url, err := q.url(ctx, q.queueName(queue))
	if err != nil {
		q.recreate(ctx, queue, err)
		return "", err
	}
	out, err := q.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(url),
		MessageBody:            aws.String(string(msg.Body)),
		MessageGroupId:         aws.String(msg.GroupKey),
		MessageDeduplicationId: aws.String(msg.DedupKey),
	})
	if err != nil {
		err = classify(operation, err)
		q.recreate(ctx, queue, err)
		return "", err
	}
	return aws.ToString(out.MessageId), nil
}
