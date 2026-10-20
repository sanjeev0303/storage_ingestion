package registry

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/azure"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/gcs"
	s3storage "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/s3"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/netguard"
)

// Provider domains: a known provider's endpoint must be one of its own.
var providerDomains = map[model.StorageProviderType]string{
	model.ProviderAWSS3:     "amazonaws.com",
	model.ProviderR2:        "r2.cloudflarestorage.com",
	model.ProviderGCS:       "storage.googleapis.com",
	model.ProviderAzureBlob: "blob.core.windows.net",
}

// Endpoint returns the URL the platform calls for a connection ("" means
// the provider SDK's own default), after checking it: https, no
// credentials or path, a known provider's own domain, and for a generic
// S3-compatible service any public host. A local emulator (http, private
// address) passes only under the development policy. Addresses are
// checked again on every dial, so a name that later resolves somewhere
// private is still refused.
func Endpoint(c model.StorageConnection, policy netguard.Policy) (string, error) {
	raw := c.Endpoint
	switch {
	case raw == "" && c.Provider == model.ProviderR2:
		raw = "https://" + c.AccountID + ".r2.cloudflarestorage.com"
	case raw == "" && c.Provider == model.ProviderAzureBlob:
		raw = "https://" + c.AccountID + ".blob.core.windows.net"
	case raw == "" && c.Provider == model.ProviderS3Compatible:
		return "", fmt.Errorf("%w: S3_COMPATIBLE needs an endpoint", model.ErrEndpointRejected)
	case raw == "":
		return "", nil
	}
	u, err := policy.ValidateURL(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %v", model.ErrEndpointRejected, err)
	}
	domain, known := providerDomains[c.Provider]
	if known && !netguard.HostIn(u.Hostname(), domain) && !(policy.AllowPrivate && isLocal(u)) {
		return "", fmt.Errorf("%w: %s endpoints must be under %s", model.ErrEndpointRejected, c.Provider, domain)
	}
	return u.String(), nil
}

func isLocal(u *url.URL) bool {
	if u.Hostname() == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(u.Hostname())
	return err == nil && (ip.IsLoopback() || ip.IsPrivate())
}

// Builder builds real provider clients. Every client shares one
// network-guarded HTTP client, which holds no credentials.
type Builder struct {
	policy    netguard.Policy
	http      *http.Client
	partBytes int64

	awsOnce sync.Once
	aws     *aws.Config
	awsErr  error
}

// NewBuilder builds clients under the network policy.
func NewBuilder(policy netguard.Policy, transferPartBytes int64) *Builder {
	return &Builder{policy: policy, http: policy.Client(), partBytes: transferPartBytes}
}

// Build is a BuildFunc.
func (b *Builder) Build(ctx context.Context, c model.StorageConnection, secrets model.StorageSecrets) (interfaces.StorageProvider, func(), error) {
	if err := secrets.ValidateFor(c.AuthMethod); err != nil {
		return nil, nil, err
	}
	endpoint, err := Endpoint(c, b.policy)
	if err != nil {
		return nil, nil, err
	}
	switch c.Provider {
	case model.ProviderAWSS3, model.ProviderS3Compatible, model.ProviderR2:
		cfg := s3storage.ConnectionConfig{
			Region: c.Region, Endpoint: endpoint, ForcePathStyle: c.ForcePathStyle, HTTPClient: b.http,
			AccessKeyID: secrets.AccessKeyID, SecretAccessKey: secrets.SecretAccessKey, SessionToken: secrets.SessionToken,
		}
		switch c.Provider {
		case model.ProviderR2:
			// R2 signs with the region "auto"; it has no AWS regions. Its
			// certificate covers <account>.r2.cloudflarestorage.com, not a
			// bucket subdomain of it, so requests are path style.
			cfg.Region, cfg.ForcePathStyle = "auto", true
		case model.ProviderS3Compatible:
			if cfg.Region == "" {
				cfg.Region = "us-east-1"
			}
		case model.ProviderAWSS3:
			cfg.StrictChecksums = true
		}
		if c.AuthMethod == model.AuthAssumeRole {
			base, err := b.platformAWS(ctx)
			if err != nil {
				return nil, nil, err
			}
			cfg.PlatformAWS, cfg.RoleARN, cfg.ExternalID = base, c.RoleARN, c.ExternalID
			cfg.SessionName = "object-storage-" + c.ID
		}
		client, err := s3storage.NewConnectionClient(ctx, cfg)
		if err != nil {
			return nil, nil, err
		}
		storage := s3storage.NewObjectStorage(client)
		storage.SetTransferPartBytes(b.partBytes)
		return storage, func() {}, nil
	case model.ProviderGCS:
		storage, err := gcs.New(ctx, gcs.Config{
			ServiceAccountJSON: []byte(secrets.ServiceAccountJSON), HTTPClient: b.http, Endpoint: endpoint, PartBytes: int(b.partBytes),
		})
		if err != nil {
			return nil, nil, err
		}
		return storage, func() { _ = storage.Close() }, nil
	case model.ProviderAzureBlob:
		if endpoint != "" && !netguard.HostIn(hostOf(endpoint), providerDomains[model.ProviderAzureBlob]) {
			// An emulator (Azurite) names the account in the path.
			endpoint += "/" + c.AccountID
		}
		storage, err := azure.New(azure.Config{
			AccountName: c.AccountID, AccountKey: secrets.AccountKey, SASToken: secrets.SASToken,
			Endpoint: endpoint, HTTPClient: b.http, PartBytes: b.partBytes,
		})
		if err != nil {
			return nil, nil, err
		}
		return storage, func() {}, nil
	}
	return nil, nil, fmt.Errorf("%w: %s", model.ErrUnsupportedProvider, c.Provider)
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// platformAWS loads the platform's own AWS identity once: the base that
// assumes customers' roles. It comes from the default chain (the
// workload's role, or AWS_* settings), never from a customer.
func (b *Builder) platformAWS(ctx context.Context) (*aws.Config, error) {
	b.awsOnce.Do(func() {
		cfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			b.awsErr = fmt.Errorf("%w: the platform has no AWS identity to assume roles with", model.ErrUnsupportedProvider)
			return
		}
		if cfg.Region == "" {
			cfg.Region = "us-east-1"
		}
		b.aws = &cfg
	})
	return b.aws, b.awsErr
}
