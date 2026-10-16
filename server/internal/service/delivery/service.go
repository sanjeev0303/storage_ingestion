package delivery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

var (
	// ErrNotFound covers missing assets and assets the caller may not see,
	// so the answer never reveals another tenant's or owner's asset.
	ErrNotFound = errors.New("asset not found")
	// ErrNotAvailable: the caller's own asset exists but is not deliverable
	// (still processing, or rejected).
	ErrNotAvailable        = errors.New("asset is not available for delivery")
	ErrStorageUnavailable  = errors.New("storage is temporarily unavailable")
	ErrAuditUnavailable    = errors.New("delivery could not be recorded")
	ErrRangeNotSatisfiable = errors.New("requested range not satisfiable")
)

// HardMaxURLTTL bounds every download URL whatever the configuration says.
const HardMaxURLTTL = time.Hour

const maxFilenameRunes = 150

// CDNConfig enables signed CDN delivery. There is no unsigned CDN mode:
// private assets are never cached for anyone without a valid signature.
type CDNConfig struct {
	BaseURL string // https://cdn.example.com
	Signer  interfaces.URLSigner
}

type Config struct {
	// Bucket and Prefix are where promoted assets live (promotion's
	// destination). Clients never choose either.
	Bucket     string
	Prefix     string
	DefaultTTL time.Duration
	MaxTTL     time.Duration
	// CDN is nil for direct private-storage delivery.
	CDN *CDNConfig
}

// Service delivers AVAILABLE assets to their owner. Every call reads the
// asset's current state from the database, so a deletion or rejection
// takes effect on the next request.
type Service struct {
	assets  interfaces.AssetRepository
	storage interfaces.ObjectDelivery
	audit   interfaces.DeliveryAuditRepository
	cfg     Config
	log     *slog.Logger
	now     func() time.Time
	newID   func() string
}

func NewService(
	assets interfaces.AssetRepository,
	storage interfaces.ObjectDelivery,
	audit interfaces.DeliveryAuditRepository,
	cfg Config,
	log *slog.Logger,
) (*Service, error) {
	if assets == nil || storage == nil || audit == nil || log == nil {
		return nil, errors.New("delivery dependencies are required")
	}
	cfg.Prefix = strings.Trim(cfg.Prefix, "/")
	if cfg.Bucket == "" || cfg.Prefix == "" || cfg.Prefix == "quarantine" {
		return nil, errors.New("delivery bucket and a non-quarantine prefix are required")
	}
	if cfg.DefaultTTL <= 0 || cfg.MaxTTL < cfg.DefaultTTL || cfg.MaxTTL > HardMaxURLTTL {
		return nil, fmt.Errorf("download URL TTLs must satisfy 0 < default <= max <= %s", HardMaxURLTTL)
	}
	if cfg.CDN != nil {
		parsed, err := url.Parse(cfg.CDN.BaseURL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" {
			return nil, errors.New("CDN base URL must be an https URL without a query")
		}
		if cfg.CDN.Signer == nil {
			return nil, errors.New("CDN delivery requires URL signing; unsigned CDN delivery is not supported")
		}
	}

	return &Service{
		assets: assets, storage: storage, audit: audit, cfg: cfg, log: log,
		now:   func() time.Time { return time.Now().UTC() },
		newID: newEventID,
	}, nil
}

// AssetInfo is the metadata an owner may see. It never includes the
// bucket or object key.
type AssetInfo struct {
	ID              string
	FileName        string
	ContentType     string
	SizeBytes       int64
	ChecksumSHA256  string
	Status          model.AssetStatus
	RejectionReason string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// URLRequest is a download-URL request. TTL 0 means the default.
type URLRequest struct {
	TTL      time.Duration
	Filename string
}

type DownloadURL struct {
	URL       string
	ExpiresAt time.Time
	Via       string // "storage" or "cdn"
}

// Download is an authorized stream of all or part of an asset.
type Download struct {
	Body               io.ReadCloser
	ContentType        string
	ContentDisposition string
	TotalSize          int64
	First, Last        int64
	Partial            bool
}

// Metadata returns the caller's own asset, in any state but DELETED.
func (s *Service) Metadata(ctx context.Context, principal model.Principal, assetID string) (AssetInfo, error) {
	asset, err := s.owned(ctx, principal, assetID)
	if err != nil {
		return AssetInfo{}, err
	}
	return AssetInfo{
		ID: asset.ID, FileName: asset.FileName, ContentType: asset.ContentType, SizeBytes: asset.SizeBytes,
		ChecksumSHA256: asset.ChecksumSHA256, Status: asset.Status, RejectionReason: asset.RejectionReason,
		CreatedAt: asset.CreatedAt, UpdatedAt: asset.UpdatedAt,
	}, nil
}

// CreateDownloadURL issues a short-lived URL for an AVAILABLE asset. The
// URL is a bearer credential: anyone holding it can download until it
// expires, so authorization happens here, at issue time. The URL is
// returned only after the decision is durably audited, and never stored.
func (s *Service) CreateDownloadURL(ctx context.Context, principal model.Principal, assetID string, req URLRequest) (DownloadURL, error) {
	asset, err := s.deliverable(ctx, principal, assetID, model.DeliveryOpDownloadURL)
	if err != nil {
		return DownloadURL{}, err
	}

	ttl := req.TTL
	if ttl <= 0 {
		ttl = s.cfg.DefaultTTL
	}
	if ttl > s.cfg.MaxTTL {
		ttl = s.cfg.MaxTTL
	}
	expiresAt := s.now().Add(ttl)

	var result DownloadURL
	if s.cfg.CDN != nil {
		signed, err := s.cfg.CDN.Signer.SignURL(s.cdnURL(asset.ObjectKey), expiresAt)
		if err != nil {
			s.fail(ctx, principal, asset.ID, model.DeliveryOpDownloadURL, "cdn_signing_failed", err)
			return DownloadURL{}, ErrStorageUnavailable
		}
		result = DownloadURL{URL: signed, ExpiresAt: expiresAt, Via: "cdn"}
	} else {
		presigned, err := s.storage.PresignGet(ctx, s.cfg.Bucket, asset.ObjectKey, ttl, interfaces.DownloadOptions{
			ContentDisposition: attachment(req.Filename, asset.FileName),
			ContentType:        asset.ContentType,
		})
		if err != nil {
			s.fail(ctx, principal, asset.ID, model.DeliveryOpDownloadURL, "presign_failed", err)
			return DownloadURL{}, ErrStorageUnavailable
		}
		result = DownloadURL{URL: presigned, ExpiresAt: expiresAt, Via: "storage"}
	}

	if err := s.record(ctx, principal, asset.ID, model.DeliveryOpDownloadURL, model.DeliveryAllowed, ""); err != nil {
		return DownloadURL{}, ErrAuditUnavailable
	}
	return result, nil
}

// Open authorizes a proxied download and opens the requested byte range.
// Authorization runs on every request, unlike a presigned URL.
func (s *Service) Open(ctx context.Context, principal model.Principal, assetID string, rangeHeader string, filename string) (Download, error) {
	asset, err := s.deliverable(ctx, principal, assetID, model.DeliveryOpProxy)
	if err != nil {
		return Download{}, err
	}

	first, last, partial, err := parseRange(rangeHeader, asset.SizeBytes)
	if err != nil {
		return Download{TotalSize: asset.SizeBytes}, err
	}

	if err := s.record(ctx, principal, asset.ID, model.DeliveryOpProxy, model.DeliveryAllowed, ""); err != nil {
		return Download{}, ErrAuditUnavailable
	}

	requestLast := last
	if !partial {
		requestLast = -1
	}
	body, err := s.storage.OpenRange(ctx, s.cfg.Bucket, asset.ObjectKey, first, requestLast)
	if err != nil {
		if ctx.Err() != nil {
			return Download{}, ctx.Err()
		}
		s.log.Error("open asset for delivery", "asset_id", asset.ID, "tenant_id", principal.TenantID, "error", retry.Sanitize(err))
		return Download{}, ErrStorageUnavailable
	}

	return Download{
		Body:               body,
		ContentType:        asset.ContentType,
		ContentDisposition: attachment(filename, asset.FileName),
		TotalSize:          asset.SizeBytes,
		First:              first,
		Last:               last,
		Partial:            partial,
	}, nil
}

// owned loads the caller's own asset. Anything the caller may not see is
// ErrNotFound, exactly like a missing asset.
func (s *Service) owned(ctx context.Context, principal model.Principal, assetID string) (model.Asset, error) {
	if !principal.Valid() {
		return model.Asset{}, model.ErrUnauthenticated
	}
	if strings.TrimSpace(assetID) == "" {
		return model.Asset{}, ErrNotFound
	}

	asset, err := s.assets.GetByID(ctx, principal.TenantID, assetID)
	if errors.Is(err, model.ErrAssetNotFound) {
		return model.Asset{}, ErrNotFound
	}
	if err != nil {
		return model.Asset{}, fmt.Errorf("load asset: %w", err)
	}
	// Sharing does not exist yet: only the owner may see an asset.
	if asset.OwnerID != principal.UserID || asset.Status == model.AssetStatusDeleted {
		return model.Asset{}, ErrNotFound
	}
	return *asset, nil
}

// deliverable is owned plus the delivery rules: AVAILABLE, and stored at
// the promoted key derived from its tenant and ID.
func (s *Service) deliverable(ctx context.Context, principal model.Principal, assetID string, operation string) (model.Asset, error) {
	asset, err := s.owned(ctx, principal, assetID)
	switch {
	case errors.Is(err, model.ErrUnauthenticated):
		return model.Asset{}, err
	case errors.Is(err, ErrNotFound):
		s.deny(ctx, principal, assetID, operation, "not_found")
		return model.Asset{}, err
	case err != nil:
		return model.Asset{}, err
	}

	if asset.Status != model.AssetStatusAvailable {
		s.deny(ctx, principal, assetID, operation, "not_available")
		return model.Asset{}, ErrNotAvailable
	}
	if asset.ObjectKey != s.promotedKey(asset) {
		s.log.Error("available asset is not at its promoted key; refusing delivery",
			"asset_id", asset.ID, "tenant_id", asset.TenantID)
		s.deny(ctx, principal, assetID, operation, "key_mismatch")
		return model.Asset{}, ErrNotAvailable
	}
	return asset, nil
}

func (s *Service) promotedKey(asset model.Asset) string {
	return fmt.Sprintf("%s/%s/%s", s.cfg.Prefix, asset.TenantID, asset.ID)
}

// cdnURL is the CDN URL for an object key, each segment escaped.
func (s *Service) cdnURL(objectKey string) string {
	segments := strings.Split(objectKey, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.TrimRight(s.cfg.CDN.BaseURL, "/") + "/" + strings.Join(segments, "/")
}

func (s *Service) record(ctx context.Context, principal model.Principal, assetID, operation, outcome, reason string) error {
	err := s.audit.Record(ctx, model.DeliveryAuditEvent{
		ID: s.newID(), TenantID: principal.TenantID, PrincipalID: principal.UserID, AssetID: assetID,
		Operation: operation, Outcome: outcome, Reason: reason, CreatedAt: s.now(),
	})
	if err != nil {
		s.log.Error("record delivery audit event", "operation", operation, "outcome", outcome, "error", retry.Sanitize(err))
	}
	return err
}

// deny audits a refusal. It is best effort: refusing still refuses.
func (s *Service) deny(ctx context.Context, principal model.Principal, assetID, operation, reason string) {
	s.log.Info("delivery denied", "asset_id", assetID, "tenant_id", principal.TenantID, "operation", operation, "reason", reason)
	_ = s.record(ctx, principal, assetID, operation, model.DeliveryDenied, reason)
}

// fail logs a provider failure without its raw text reaching the client.
func (s *Service) fail(ctx context.Context, principal model.Principal, assetID, operation, reason string, err error) {
	s.log.Error("delivery failed", "asset_id", assetID, "tenant_id", principal.TenantID,
		"operation", operation, "reason", reason, "error", retry.Sanitize(err))
	_ = s.record(ctx, principal, assetID, operation, model.DeliveryFailed, reason)
}

// SafeFilename keeps a download filename harmless: no path, no control or
// quoting characters, no leading dots, bounded length.
func SafeFilename(name string) string {
	name = name[strings.LastIndexAny(name, `/\`)+1:]

	var b strings.Builder
	for _, r := range name {
		if unicode.IsControl(r) || strings.ContainsRune(`"\;:*?<>|`, r) {
			continue
		}
		b.WriteRune(r)
	}

	cleaned := strings.TrimSpace(strings.TrimLeft(b.String(), ". "))
	if runes := []rune(cleaned); len(runes) > maxFilenameRunes {
		cleaned = string(runes[:maxFilenameRunes])
	}
	if cleaned == "" {
		return "download"
	}
	return cleaned
}

// attachment is always a download, never inline rendering, so stored
// content cannot run as a page on the storage or CDN origin. Non-ASCII
// names are encoded per RFC 2231 by the standard library.
func attachment(requested, stored string) string {
	name := requested
	if strings.TrimSpace(name) == "" {
		name = stored
	}
	return mime.FormatMediaType("attachment", map[string]string{"filename": SafeFilename(name)})
}

func newEventID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
