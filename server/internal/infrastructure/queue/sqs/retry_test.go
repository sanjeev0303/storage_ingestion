package sqs

import (
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/smithy-go"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

func apiError(code string) error {
	return &smithy.GenericAPIError{Code: code, Message: "details from the provider, https://sqs.example/?X-Amz-Signature=abc"}
}

func TestClassifyThrottlingIsRetryable(t *testing.T) {
	for _, code := range []string{"RequestThrottled", "ThrottlingException", "KmsThrottled", "OverLimit"} {
		err := classify("publish to q", apiError(code))
		if !errors.Is(err, interfaces.ErrQueueThrottled) || retry.IsPermanent(err) {
			t.Errorf("%s: want a retryable throttle, got %v", code, err)
		}
	}
}

func TestClassifyPermanentFailures(t *testing.T) {
	// A missing queue is fixed by whoever deploys it, so it is retried
	// within the caller's budget rather than failing the event at once.
	missing := classify("find queue q", &types.QueueDoesNotExist{})
	if !errors.Is(missing, interfaces.ErrQueueNotFound) || retry.IsPermanent(missing) {
		t.Fatalf("a missing queue is a retryable ErrQueueNotFound, got %v", missing)
	}
	for _, code := range []string{"InvalidMessageContents", "InvalidParameterValue", "MissingParameter"} {
		if err := classify("publish to q", apiError(code)); !retry.IsPermanent(err) {
			t.Errorf("%s: want permanent, got %v", code, err)
		}
	}
}

func TestClassifyCredentialAndKeyFailuresAreRetried(t *testing.T) {
	// A bad IAM change or key rotation is fixed by a deployment; failing
	// every event of the outage at once would dead-letter all of them.
	for _, code := range []string{"AccessDenied", "InvalidClientTokenId", "SignatureDoesNotMatch", "KmsAccessDenied", "KmsDisabled"} {
		if err := classify("publish to q", apiError(code)); retry.IsPermanent(err) {
			t.Errorf("%s: want retryable, got permanent %v", code, err)
		}
	}
}

func TestClassifyNetworkFailuresAreRetryable(t *testing.T) {
	err := classify("publish to q", &net.OpError{Op: "dial", Err: errors.New("connection refused")})
	if retry.IsPermanent(err) || errors.Is(err, interfaces.ErrQueueThrottled) {
		t.Fatalf("a network error is a plain retryable failure, got %v", err)
	}
	if err := classify("publish to q", apiError("InternalError")); retry.IsPermanent(err) {
		t.Fatalf("an unknown provider error is retried, got %v", err)
	}
}

func TestClassifyKeepsOnlyTheCode(t *testing.T) {
	err := classify("publish to q", apiError("InternalError"))
	if strings.Contains(err.Error(), "Signature") || !strings.Contains(err.Error(), "InternalError") {
		t.Fatalf("only the provider's code may be kept, got %v", err)
	}
}

func TestClassifyReceiptExpiry(t *testing.T) {
	for _, err := range []error{&types.ReceiptHandleIsInvalid{}, &types.MessageNotInflight{}, apiError("InvalidParameterValue")} {
		if got := classifyReceipt("acknowledge on q", err); !errors.Is(got, interfaces.ErrReceiptExpired) {
			t.Errorf("want ErrReceiptExpired, got %v", got)
		}
	}
}
