package s3

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Config contains the settings needed to connect to an s3-compatible service
type Config struct {
	Region          string
	Endpoint        string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	ForcePathStyle  bool
}

// LoadConfig reads the storage settings from the environment.
func LoadConfig() (Config, error) {
	cfg := Config{
		Region:          os.Getenv("S3_REGION"),
		Endpoint:        os.Getenv("S3_ENDPOINT"),
		AccessKeyID:     os.Getenv("S3_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("S3_SECRET_ACCESS_KEY"),
		SessionToken:    os.Getenv("S3_SESSION_TOKEN"),
		ForcePathStyle:  strings.EqualFold(os.Getenv("S3_FORCE_PATH_STYLE"), "true"),
	}

	if cfg.Region == "" {
		return Config{}, fmt.Errorf("S3_REGION is required")
	}

	if cfg.Endpoint != "" {
		parsed, err := url.Parse(cfg.Endpoint)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return Config{}, fmt.Errorf("S3_ENDPOINT must be a valid absolute URL")
		}

		if parsed.Scheme != "https" &&
			!(parsed.Scheme == "http" &&
				(parsed.Hostname() == "localhost" ||
					parsed.Hostname() == "127.0.0.1")) {
			return Config{}, fmt.Errorf(
				"S3_ENDPOINT must use HTTPS except for local development",
			)
		}
	}

	// If explicit credentials are supplied, require both key and secret.
	if (cfg.AccessKeyID == "") != (cfg.SecretAccessKey == "") {
		return Config{}, fmt.Errorf("S3_ACCESS_KEY_ID and S3_SECRET_ACCESS_KEY must be set together")
	}

	return cfg, nil
}

// NewClient creates an S3 client. With no explicit credentials,
// AWS SDK uses its default credential provider chain.
func NewClient(
	ctx context.Context,
	cfg Config,
) (*s3.Client, error) {
	// Every processing job reads up to rangeread.Parallel ranges at once
	// and several jobs run side by side; the SDK's default keeps 10 idle
	// connections per host, so the rest were closed after each request and
	// opened again (a TCP handshake per range).
	httpClient := awshttp.NewBuildableClient().WithTransportOptions(func(t *http.Transport) {
		t.MaxIdleConns = 256
		t.MaxIdleConnsPerHost = 128
	})
	options := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithHTTPClient(httpClient),
	}

	if cfg.AccessKeyID != "" {
		options = append(options, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(
				cfg.AccessKeyID,
				cfg.SecretAccessKey,
				cfg.SessionToken,
			),
		))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("load AWS SDK configuration: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = &cfg.Endpoint
		}
		o.UsePathStyle = cfg.ForcePathStyle
		// The platform checks every object's SHA-256 itself, end to end; the
		// SDK's own CRC over every byte sent and received adds CPU and
		// nothing more, and is kept only where an operation requires it.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})

	return client, nil
}
