// Package sqs is the Amazon SQS adapter for interfaces.MessageQueue. It
// also runs against ElasticMQ for local development.
//
// Every logical queue is a FIFO queue with a FIFO dead-letter queue:
// messages with the same group key are delivered one at a time in order,
// a repeat of a deduplication ID within five minutes is dropped, and a
// message received more than MaxReceiveCount times is moved to the
// dead-letter queue by SQS itself.
package sqs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
)

// SQS limits.
const (
	maxMessageBytes = 256 * 1024
	maxBatch        = 10
	maxWaitSeconds  = 20
	maxVisibility   = 12 * 60 * 60 // seconds
)

// requestTimeout bounds every SQS request, a 20-second long poll included,
// so an endpoint that stops answering cannot hold a worker indefinitely.
const requestTimeout = 30 * time.Second

type Config struct {
	Region          string
	Endpoint        string // empty: AWS
	AccessKeyID     string
	SecretAccessKey string
	// Prefix starts every queue name: queue "processing" is
	// Prefix+"processing.fifo", its dead-letter queue
	// Prefix+"processing-dlq.fifo".
	Prefix string
	// MaxReceiveCount is how often SQS delivers a message before moving it
	// to the dead-letter queue. It is a backstop for consumers that crash
	// before recording anything; the consumer's own retry budget is lower.
	MaxReceiveCount int
	// CreateQueues creates missing queues at startup, for local
	// development. In AWS the queues are provisioned separately.
	CreateQueues bool
}

// Queue sends and receives events through SQS.
type Queue struct {
	client *sqs.Client
	cfg    Config

	mu   sync.Mutex
	urls map[string]string // queue name -> URL
}

var _ interfaces.MessageQueue = (*Queue)(nil)

// New connects to SQS and checks, or with CreateQueues creates, the given
// logical queues and their dead-letter queues.
func New(ctx context.Context, cfg Config, queues ...string) (*Queue, error) {
	if cfg.Region == "" || cfg.MaxReceiveCount <= 0 {
		return nil, fmt.Errorf("SQS needs a region and a positive maximum receive count")
	}

	options := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithHTTPClient(awshttp.NewBuildableClient().WithTimeout(requestTimeout)),
	}
	if cfg.AccessKeyID != "" {
		options = append(options, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("load AWS SDK configuration: %w", err)
	}
	client := sqs.NewFromConfig(awsCfg, func(o *sqs.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
	})

	q := &Queue{client: client, cfg: cfg, urls: make(map[string]string)}
	for _, name := range queues {
		if cfg.CreateQueues {
			err = q.ensure(ctx, name)
		} else {
			_, err = q.url(ctx, q.deadLetterName(name))
			if err == nil {
				_, err = q.url(ctx, q.queueName(name))
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return q, nil
}

func (q *Queue) queueName(queue string) string {
	return q.cfg.Prefix + queue + ".fifo"
}

func (q *Queue) deadLetterName(queue string) string {
	return q.cfg.Prefix + queue + "-dlq.fifo"
}

// recreate brings back a queue that vanished, as an in-memory ElasticMQ's
// do when it restarts, so the caller's next attempt finds it. Only with
// CreateQueues: in AWS a missing queue stays an error for operators.
func (q *Queue) recreate(ctx context.Context, queue string, err error) {
	if q.cfg.CreateQueues && errors.Is(err, interfaces.ErrQueueNotFound) {
		_ = q.ensure(ctx, queue)
	}
}

// url resolves a full queue name to its URL, once.
func (q *Queue) url(ctx context.Context, fullName string) (string, error) {
	q.mu.Lock()
	url, ok := q.urls[fullName]
	q.mu.Unlock()
	if ok {
		return url, nil
	}

	out, err := q.client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(fullName)})
	if err != nil {
		return "", classify("find queue "+fullName, err)
	}
	url = aws.ToString(out.QueueUrl)

	q.mu.Lock()
	q.urls[fullName] = url
	q.mu.Unlock()
	return url, nil
}
