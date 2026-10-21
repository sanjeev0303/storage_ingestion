package s3

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

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
	options := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
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
	})

	return client, nil
}
