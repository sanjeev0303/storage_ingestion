package storageconn

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/registry"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/netguard"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

// ErrTestFailed: new credentials did not pass the connection test, so
// they were not stored.
var ErrTestFailed = errors.New("the storage connection test failed")

// Sanitized test outcomes. A provider's own message never leaves the
// service: it may name accounts, keys or internal hosts.
const (
	CategoryAccessDenied       = "ACCESS_DENIED"
	CategoryNotFound           = "NOT_FOUND"
	CategoryThrottled          = "THROTTLED"
	CategoryNetwork            = "NETWORK"
	CategoryEndpointRejected   = "ENDPOINT_REJECTED"
	CategoryTLS                = "TLS"
	CategoryInvalidCredentials = "INVALID_CREDENTIALS"
	CategoryUnsupported        = "UNSUPPORTED"
	CategoryProviderError      = "PROVIDER_ERROR"
)

var categoryMessages = map[string]string{
	CategoryAccessDenied:       "the credentials are not allowed to do this; check the permissions in docs/storage-connections.md",
	CategoryNotFound:           "the bucket or container does not exist, or the credentials cannot see it",
	CategoryThrottled:          "the provider is throttling requests; try again later",
	CategoryNetwork:            "the endpoint could not be reached in time",
	CategoryEndpointRejected:   "the endpoint is not allowed by the platform's network policy",
	CategoryTLS:                "the endpoint's TLS certificate is not trusted",
	CategoryInvalidCredentials: "the credentials are malformed or could not be used",
	CategoryUnsupported:        "the provider does not support an operation the platform needs",
	CategoryProviderError:      "the provider returned an unexpected error",
}

// Check is one step of a connection test.
type Check struct {
	Name     string
	OK       bool
	Category string
}

// TestResult is a connection test's sanitized outcome.
type TestResult struct {
	OK           bool
	Category     string
	Message      string
	Checks       []Check
	Capabilities *model.ProviderCapabilities
	TestedAt     time.Time
}

// Test checks a connection with its stored credentials: the endpoint
// against the network policy, the bucket and its capabilities, listing
// under the ready prefix, reading the metadata of one object it already
// holds, if any, and, only when probe is asked for, writing, reading and
// deleting one small object under the ready prefix. Nothing else is
// created or deleted. A passing test activates a pending connection.
func (s *Service) Test(ctx context.Context, p model.Principal, connectionID string, probe bool) (model.StorageConnection, TestResult, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermStorageManage, "storage_connection.test", "storage_connection", connectionID); err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	if err := s.enabled(); err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	c, err := s.ownConnection(ctx, p, connectionID)
	if err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	if c.Status == model.ConnectionDisabled {
		return model.StorageConnection{}, TestResult{}, fmt.Errorf("%w: enable it to test it", model.ErrConnectionNotActive)
	}
	return s.test(ctx, p, c, probe)
}

func (s *Service) test(ctx context.Context, p model.Principal, c model.StorageConnection, probe bool) (model.StorageConnection, TestResult, error) {
	result, err := s.check(ctx, c, s.stored(c), probe)
	if err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	return s.recordTest(ctx, p, c, result, probe)
}

// recordTest stores a test's outcome and audits it.
func (s *Service) recordTest(ctx context.Context, p model.Principal, c model.StorageConnection, result TestResult, probe bool) (model.StorageConnection, TestResult, error) {
	stored, err := s.deps.Connections.RecordTest(ctx, p.TenantID, c.ID, interfaces.ConnectionTestRecord{
		OK: result.OK, ErrorCategory: result.Category, Capabilities: result.Capabilities, At: result.TestedAt,
	})
	if err != nil {
		return model.StorageConnection{}, result, err
	}
	s.deps.Clients.Invalidate(c.ID)
	s.record(ctx, p, "storage_connection.test", stored, map[string]any{"passed": result.OK, "category": result.Category, "probe": probe})
	return stored, result, nil
}

// stored builds clients for c's settings from its stored credentials.
func (s *Service) stored(c model.StorageConnection) func(context.Context) (interfaces.StorageProvider, func(), error) {
	return func(ctx context.Context) (interfaces.StorageProvider, func(), error) {
		return s.deps.Clients.BuildStored(ctx, c)
	}
}

// runTest tests secrets that are not stored yet (a rotation).
func (s *Service) runTest(ctx context.Context, c model.StorageConnection, secrets model.StorageSecrets, probe bool) (TestResult, error) {
	build := func(ctx context.Context) (interfaces.StorageProvider, func(), error) {
		return s.deps.Clients.Build(ctx, c, secrets)
	}
	return s.check(ctx, c, build, probe)
}

// check runs a connection test. Its error is the platform's own failure
// (the database); the provider's failures are in the result.
func (s *Service) check(ctx context.Context, c model.StorageConnection, build func(context.Context) (interfaces.StorageProvider, func(), error), probe bool) (TestResult, error) {
	held, err := s.deps.Connections.HeldObject(ctx, c.TenantID, c.ID)
	if err != nil {
		return TestResult{}, err
	}
	timeout := s.cfg.TestTimeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result := TestResult{TestedAt: s.now()}
	pass := func(name string) { result.Checks = append(result.Checks, Check{Name: name, OK: true}) }
	fail := func(name string, err error) TestResult {
		category := Categorize(err)
		s.deps.Log.Info("storage connection test failed", "connection_id", c.ID, "check", name, "category", category)
		result.Checks = append(result.Checks, Check{Name: name, Category: category})
		result.Category, result.Message = category, categoryMessages[category]
		return result
	}

	// The address is checked now, not only when the connection was saved:
	// a name that has since been pointed somewhere private is refused.
	endpoint, err := endpointOf(c, s.policy)
	if err != nil {
		return fail("endpoint", err), nil
	}
	if host := hostOf(endpoint); host != "" {
		if err := s.policy.Resolve(ctx, host); err != nil {
			return fail("endpoint", err), nil
		}
	}
	pass("endpoint")

	client, closeClient, err := build(ctx)
	if err != nil {
		return fail("credentials", err), nil
	}
	defer closeClient()

	caps, err := client.ProviderCapabilities(ctx, c.Bucket)
	if err != nil {
		return fail("bucket", err), nil
	}
	pass("bucket")

	if _, err := client.ListObjectsPage(ctx, c.Bucket, c.ReadyPrefix+"/", "", 1); err != nil {
		return fail("list", err), nil
	}
	pass("list")

	// A bucket of the same name behind another endpoint or account is not
	// the same bucket: what the connection already holds must be there.
	if held != "" {
		if _, err := client.HeadObject(ctx, c.Bucket, held); err != nil {
			return fail("existing_object", err), nil
		}
		pass("existing_object")
	}

	if probe {
		if err := s.probe(ctx, c, client, &caps); err != nil {
			return fail("write_probe", err), nil
		}
		pass("write_probe")
	}

	result.OK = true
	result.Capabilities = &caps
	return result, nil
}

// probe writes, reads back and deletes one small object of its own under
// the ready prefix. Where the write returns a version ID, versioning is
// on; deleting the key removes that version too, so nothing is left.
func (s *Service) probe(ctx context.Context, c model.StorageConnection, client interfaces.StorageProvider, caps *model.ProviderCapabilities) error {
	body := []byte("object storage platform connection test\n")
	key := c.ReadyPrefix + "/" + c.TenantID + "/.connection-test/" + s.newID()
	meta, err := client.WriteObject(ctx, c.Bucket, key, bytes.NewReader(body), int64(len(body)), "text/plain")
	if err != nil {
		return err
	}
	defer func() {
		_ = client.DeleteObject(context.WithoutCancel(ctx), c.Bucket, key)
	}()
	reader, err := client.OpenObject(ctx, c.Bucket, key)
	if err != nil {
		return err
	}
	read, err := io.ReadAll(io.LimitReader(reader, int64(len(body))+1))
	reader.Close()
	if err != nil {
		return err
	}
	if !bytes.Equal(read, body) {
		return errors.New("the probe object read back differently")
	}
	if meta.VersionID != "" {
		caps.ProviderVersioning, caps.VersioningStatus = true, "Enabled"
	}
	return nil
}

func endpointOf(c model.StorageConnection, policy netguard.Policy) (string, error) {
	return registry.Endpoint(c, policy)
}

func hostOf(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// Categorize maps a test failure to a sanitized category.
func Categorize(err error) string {
	var certErr *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var invalidCert x509.CertificateInvalidError
	var dnsErr *net.DNSError
	var opErr *net.OpError
	switch {
	case errors.Is(err, model.ErrEndpointRejected), errors.Is(err, netguard.ErrRejected):
		return CategoryEndpointRejected
	case errors.As(err, &certErr), errors.As(err, &unknownAuthority), errors.As(err, &hostnameErr), errors.As(err, &invalidCert):
		return CategoryTLS
	case errors.Is(err, interfaces.ErrStorageAccessDenied):
		return CategoryAccessDenied
	case errors.Is(err, interfaces.ErrObjectNotFound):
		return CategoryNotFound
	case errors.Is(err, interfaces.ErrStorageThrottled):
		return CategoryThrottled
	case errors.Is(err, model.ErrInvalidConnection), errors.Is(err, model.ErrCredentialMissing),
		errors.Is(err, interfaces.ErrStorageCredentialsRejected):
		return CategoryInvalidCredentials
	case errors.Is(err, model.ErrUnsupportedProvider):
		return CategoryUnsupported
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &dnsErr), errors.As(err, &opErr):
		return CategoryNetwork
	}
	return CategoryProviderError
}
