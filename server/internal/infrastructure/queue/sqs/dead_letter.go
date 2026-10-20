package sqs

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
)

// ensure creates a queue and its dead-letter queue when missing, and
// (re)applies the redrive policy that connects them. Each call is
// idempotent.
func (q *Queue) ensure(ctx context.Context, queue string) error {
	fifo := map[string]string{string(types.QueueAttributeNameFifoQueue): "true"}

	deadName := q.deadLetterName(queue)
	deadOut, err := q.client.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName: aws.String(deadName),
		Attributes: map[string]string{
			string(types.QueueAttributeNameFifoQueue): "true",
			// The longest SQS allows, so nothing is lost before
			// reconciliation drains it.
			string(types.QueueAttributeNameMessageRetentionPeriod): strconv.Itoa(14 * 24 * 60 * 60),
		},
	})
	if err != nil {
		return classify("create queue "+deadName, err)
	}
	deadArn, err := q.client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       deadOut.QueueUrl,
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
	})
	if err != nil {
		return classify("read queue "+deadName, err)
	}

	name := q.queueName(queue)
	out, err := q.client.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(name), Attributes: fifo})
	if err != nil {
		return classify("create queue "+name, err)
	}
	policy := fmt.Sprintf(`{"deadLetterTargetArn":%q,"maxReceiveCount":"%d"}`,
		deadArn.Attributes[string(types.QueueAttributeNameQueueArn)], q.cfg.MaxReceiveCount)
	_, err = q.client.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
		QueueUrl:   out.QueueUrl,
		Attributes: map[string]string{string(types.QueueAttributeNameRedrivePolicy): policy},
	})
	if err != nil {
		return classify("set redrive policy of "+name, err)
	}

	q.mu.Lock()
	q.urls[deadName] = aws.ToString(deadOut.QueueUrl)
	q.urls[name] = aws.ToString(out.QueueUrl)
	q.mu.Unlock()
	return nil
}

// ReceiveDeadLetters reads messages SQS moved to the queue's dead-letter
// queue after too many receives.
func (q *Queue) ReceiveDeadLetters(ctx context.Context, queue string, limit int, visibility time.Duration) ([]interfaces.QueueMessage, error) {
	url, err := q.url(ctx, q.deadLetterName(queue))
	if err != nil {
		return nil, err
	}
	return q.receive(ctx, url, "receive dead letters of "+queue, limit, visibility, 0)
}

// AckDeadLetter deletes a message from the queue's dead-letter queue, once
// it is recorded in the dead_letters table.
func (q *Queue) AckDeadLetter(ctx context.Context, queue, receipt string) error {
	url, err := q.url(ctx, q.deadLetterName(queue))
	if err != nil {
		return err
	}
	return q.delete(ctx, url, "acknowledge dead letter of "+queue, receipt)
}
