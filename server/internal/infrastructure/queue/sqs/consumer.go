package sqs

import (
	"context"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
)

// Receive long-polls for up to wait (at most 20s) and returns at most
// limit messages (at most 10), each hidden from other consumers for
// visibility.
func (q *Queue) Receive(ctx context.Context, queue string, limit int, visibility, wait time.Duration) ([]interfaces.QueueMessage, error) {
	url, err := q.url(ctx, q.queueName(queue))
	if err != nil {
		q.recreate(ctx, queue, err)
		return nil, err
	}
	messages, err := q.receive(ctx, url, "receive from "+queue, limit, visibility, wait)
	q.recreate(ctx, queue, err)
	return messages, err
}

func (q *Queue) receive(ctx context.Context, url, operation string, limit int, visibility, wait time.Duration) ([]interfaces.QueueMessage, error) {
	out, err := q.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(url),
		MaxNumberOfMessages: int32(min(max(limit, 1), maxBatch)),
		VisibilityTimeout:   seconds(visibility, maxVisibility),
		WaitTimeSeconds:     seconds(wait, maxWaitSeconds),
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{
			types.MessageSystemAttributeNameApproximateReceiveCount,
			types.MessageSystemAttributeNameSentTimestamp,
			types.MessageSystemAttributeNameMessageGroupId,
		},
	})
	if err != nil {
		return nil, classify(operation, err)
	}

	messages := make([]interfaces.QueueMessage, 0, len(out.Messages))
	for _, m := range out.Messages {
		receives, _ := strconv.Atoi(m.Attributes[string(types.MessageSystemAttributeNameApproximateReceiveCount)])
		sentMillis, _ := strconv.ParseInt(m.Attributes[string(types.MessageSystemAttributeNameSentTimestamp)], 10, 64)
		messages = append(messages, interfaces.QueueMessage{
			ID:           aws.ToString(m.MessageId),
			Body:         []byte(aws.ToString(m.Body)),
			Receipt:      aws.ToString(m.ReceiptHandle),
			ReceiveCount: receives,
			SentAt:       time.UnixMilli(sentMillis).UTC(),
			GroupKey:     m.Attributes[string(types.MessageSystemAttributeNameMessageGroupId)],
		})
	}
	return messages, nil
}

// Ack deletes a processed message.
func (q *Queue) Ack(ctx context.Context, queue, receipt string) error {
	url, err := q.url(ctx, q.queueName(queue))
	if err != nil {
		return err
	}
	return q.delete(ctx, url, "acknowledge on "+queue, receipt)
}

func (q *Queue) delete(ctx context.Context, url, operation, receipt string) error {
	_, err := q.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(url),
		ReceiptHandle: aws.String(receipt),
	})
	if err != nil {
		return classifyReceipt(operation, err)
	}
	return nil
}

// Extend keeps a message hidden for another visibility period.
func (q *Queue) Extend(ctx context.Context, queue, receipt string, visibility time.Duration) error {
	return q.changeVisibility(ctx, queue, "extend visibility on "+queue, receipt, visibility)
}

// Stats reads SQS's approximate counts for the queue and its dead-letter
// queue.
func (q *Queue) Stats(ctx context.Context, queue string) (interfaces.QueueStats, error) {
	main, err := q.attributes(ctx, q.queueName(queue))
	if err != nil {
		return interfaces.QueueStats{}, err
	}
	dead, err := q.attributes(ctx, q.deadLetterName(queue))
	if err != nil {
		return interfaces.QueueStats{}, err
	}
	return interfaces.QueueStats{
		Waiting:     main[types.QueueAttributeNameApproximateNumberOfMessages],
		InFlight:    main[types.QueueAttributeNameApproximateNumberOfMessagesNotVisible],
		Delayed:     main[types.QueueAttributeNameApproximateNumberOfMessagesDelayed],
		DeadLetters: dead[types.QueueAttributeNameApproximateNumberOfMessages],
	}, nil
}

func (q *Queue) attributes(ctx context.Context, fullName string) (map[types.QueueAttributeName]int, error) {
	url, err := q.url(ctx, fullName)
	if err != nil {
		return nil, err
	}
	names := []types.QueueAttributeName{
		types.QueueAttributeNameApproximateNumberOfMessages,
		types.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
		types.QueueAttributeNameApproximateNumberOfMessagesDelayed,
	}
	out, err := q.client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(url),
		AttributeNames: names,
	})
	if err != nil {
		return nil, classify("read attributes of "+fullName, err)
	}
	counts := make(map[types.QueueAttributeName]int, len(names))
	for _, name := range names {
		counts[name], _ = strconv.Atoi(out.Attributes[string(name)])
	}
	return counts, nil
}

// seconds converts d to whole seconds within [0, limit], rounding up. A
// retry delay rounded down would make the message visible before the
// inbox's next_attempt_at, and the early delivery would only be refused
// and delayed again, using up a receive toward the queue's redrive limit.
func seconds(d time.Duration, limit int) int32 {
	if d <= 0 {
		return 0
	}
	s := int((d + time.Second - 1) / time.Second)
	return int32(min(s, limit))
}
