package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

var _ interfaces.StorageProvider = (*ObjectStorage)(nil)

// ConnectionConfig is one customer connection's S3 settings: AWS S3,
// Cloudflare R2 or another S3-compatible service. Nothing is read from
// the environment: a tenant's client never picks up the platform's own
// AWS settings by accident.
type ConnectionConfig struct {
	Region         string
	Endpoint       string
	ForcePathStyle bool
	// Static credentials (ACCESS_KEY), or a role the platform's own
	// identity assumes (ASSUME_ROLE), with the connection's external ID.
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	RoleARN         string
	ExternalID      string
	SessionName     string
	// PlatformAWS is the platform's own AWS configuration: the base
	// identity that assumes a customer's role.
	PlatformAWS *aws.Config
	// HTTPClient is the network-guarded client every request goes through.
	HTTPClient *http.Client
	// StrictChecksums leaves the SDK's default request checksums on; S3-
	// compatible services that do not support them get them only when an
	// operation requires one.
	StrictChecksums bool
}

// NewConnectionClient builds the S3 client of one connection.
func NewConnectionClient(ctx context.Context, cfg ConnectionConfig) (*s3.Client, error) {
	if cfg.HTTPClient == nil {
		return nil, errors.New("a connection client needs the guarded HTTP client")
	}
	awsCfg := aws.Config{Region: cfg.Region, HTTPClient: cfg.HTTPClient, RetryMaxAttempts: 3}
	switch {
	case cfg.AccessKeyID != "":
		awsCfg.Credentials = aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(
			cfg.AccessKeyID, cfg.SecretAccessKey, cfg.SessionToken))
	case cfg.RoleARN != "":
		if cfg.PlatformAWS == nil {
			return nil, errors.New("assuming a role needs the platform's own AWS identity")
		}
		base := cfg.PlatformAWS.Copy()
		base.HTTPClient = cfg.HTTPClient
		provider := stscreds.NewAssumeRoleProvider(sts.NewFromConfig(base), cfg.RoleARN, func(o *stscreds.AssumeRoleOptions) {
			o.ExternalID = aws.String(cfg.ExternalID)
			o.RoleSessionName = cfg.SessionName
		})
		awsCfg.Credentials = aws.NewCredentialsCache(provider)
	default:
		return nil, fmt.Errorf("%w: the connection has no credentials", model.ErrCredentialMissing)
	}
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.ForcePathStyle
		if !cfg.StrictChecksums {
			o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
			o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		}
	}), nil
}

// defaultTransferPart is the buffer of a streaming write when none is set.
const defaultTransferPart = 16 << 20

// SetTransferPartBytes sets the most of one object a streaming write
// holds in memory.
func (s *ObjectStorage) SetTransferPartBytes(n int64) { s.partBytes = n }

// WriteObject stores size bytes from body. Up to one part it is a single
// PUT; beyond, a multipart upload of fixed-size parts, read one at a time,
// aborted if anything fails. Memory is one part, whatever the size.
func (s *ObjectStorage) WriteObject(ctx context.Context, bucket, objectKey string, body io.Reader, size int64, contentType string) (interfaces.ObjectMetadata, error) {
	if bucket == "" || objectKey == "" || size < 0 {
		return interfaces.ObjectMetadata{}, errors.New("bucket, object key and size are required")
	}
	partSize := s.partBytes
	if partSize <= 0 {
		partSize = defaultTransferPart
	}
	body = io.LimitReader(body, size)
	if size <= partSize {
		buf := make([]byte, size)
		if _, err := io.ReadFull(body, buf); err != nil {
			return interfaces.ObjectMetadata{}, fmt.Errorf("read source: %w", err)
		}
		_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(bucket), Key: aws.String(objectKey), Body: bytes.NewReader(buf),
			ContentLength: aws.Int64(size), ContentType: aws.String(contentType),
		})
		if err != nil {
			return interfaces.ObjectMetadata{}, classify("put object", err)
		}
		return s.HeadObject(ctx, bucket, objectKey)
	}

	uploadID, err := s.BeginMultipart(ctx, bucket, objectKey, contentType)
	if err != nil {
		return interfaces.ObjectMetadata{}, err
	}
	abort := func(cause error) (interfaces.ObjectMetadata, error) {
		_ = s.AbortMultipart(context.WithoutCancel(ctx), bucket, objectKey, uploadID)
		return interfaces.ObjectMetadata{}, cause
	}
	buf := make([]byte, partSize)
	var parts []types.CompletedPart
	for number := int32(1); ; number++ {
		n, err := io.ReadFull(body, buf)
		if n == 0 && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
			break
		}
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			return abort(fmt.Errorf("read source: %w", err))
		}
		out, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String(bucket), Key: aws.String(objectKey), UploadId: aws.String(uploadID),
			PartNumber: aws.Int32(number), Body: bytes.NewReader(buf[:n]), ContentLength: aws.Int64(int64(n)),
		})
		if err != nil {
			return abort(classify("upload part", err))
		}
		parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(number), ETag: out.ETag})
		if int64(n) < partSize {
			break
		}
	}
	_, err = s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(objectKey), UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		return abort(classify("complete multipart upload", err))
	}
	return s.HeadObject(ctx, bucket, objectKey)
}

// ProviderCapabilities reports what an S3-family bucket does. Everything
// but versioning is the S3 API itself; versioning is asked of the bucket,
// and a provider without it (R2, some others) answers NotImplemented,
// which is reported as unsupported rather than as an error. A missing
// bucket or refused credentials are errors.
func (s *ObjectStorage) ProviderCapabilities(ctx context.Context, bucket string) (model.ProviderCapabilities, error) {
	caps := model.ProviderCapabilities{
		PresignedUpload: true, MaxSingleUploadBytes: 5 << 30,
		Multipart: true, MultipartKind: "S3", MaxParts: 10000, MinPartBytes: 5 << 20, PartValidator: "CONTENT",
		ListIncompleteUploads: true, ServerSideCopy: true, RangeReads: true, ConditionalReads: true,
		PresignedDownload: true, ListObjects: true, ChecksumAlgorithms: []string{"MD5"},
	}
	status, err := s.VersioningStatus(ctx, bucket)
	switch {
	case err == nil:
		caps.VersioningStatus = status
		caps.ProviderVersioning = status == "Enabled"
		if caps.ProviderVersioning {
			caps.VersionIDKind = "S3_VERSION_ID"
		}
	case strings.Contains(err.Error(), "NotImplemented"):
		caps.VersioningStatus = "UNSUPPORTED"
	case errors.Is(err, interfaces.ErrStorageAccessDenied):
		// Reading the setting needs its own permission; without it the
		// status is unknown, which is not a failure.
		caps.VersioningStatus = "UNKNOWN"
	default:
		return model.ProviderCapabilities{}, err
	}
	return caps, nil
}
