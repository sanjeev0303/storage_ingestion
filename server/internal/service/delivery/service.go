package delivery

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

var (
	// ErrNotFound covers missing assets and assets the caller may not see,
	// so the answer never reveals another tenant's or owner's asset.
	ErrNotFound = errors.New("asset not found")
	// ErrNotAvailable: the caller's own asset exists but is not deliverable
	// (still processing, or rejected).
	ErrNotAvailable        = errors.New("asset is not available for delivery")
	ErrStorageUnavailable  = errors.New("storage is temporarily unavailable")
	ErrStorageBusy         = errors.New("storage is throttling requests")
	ErrAuditUnavailable    = errors.New("delivery could not be recorded")
	ErrRangeNotSatisfiable = errors.New("requested range not satisfiable")
	// ErrPreconditionFailed: the client's If-Match does not match.
	ErrPreconditionFailed = errors.New("precondition failed")
	// ErrVersionMismatch: the stored object is no longer the one the asset
	// was published as. Nothing is served rather than other bytes.
	ErrVersionMismatch = errors.New("stored object does not match the published version")
	// ErrTokenInvalid covers unknown, expired and revoked download links
	// alike.
	ErrTokenInvalid   = errors.New("download link is invalid or has expired")
	ErrInvalidRequest = errors.New("invalid delivery request")
)

// HardMaxURLTTL bounds every download URL whatever the configuration says.
const HardMaxURLTTL = time.Hour

const maxFilenameRunes = 150

// URL modes for CreateDownloadURL.
const (
	// ModeStorage: a presigned storage URL (or a signed CDN URL). Not
	// revocable once issued; it works until it expires.
	ModeStorage = "storage"
	// ModeToken: an API link that is authorized again on every use and can
	// be revoked at once.
	ModeToken = "token"
)

// TokenPathPrefix is where token links are served.
const TokenPathPrefix = "/api/v1/downloads/"

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
	// URLMode is the mode used when a request names none.
	URLMode string
	// CDN is nil for direct private-storage delivery.
	CDN *CDNConfig
	// Metrics is optional.
	Metrics *metrics.Registry
}

// Service delivers AVAILABLE assets to their owner. Every call reads the
// asset's current state from the database, so a deletion or rejection
// takes effect on the next request.
type Service struct {
	assets  interfaces.AssetRepository
	storage interfaces.ObjectDelivery
	audit   interfaces.DeliveryAuditRepository
	tokens  interfaces.DownloadTokenRepository
	cfg     Config
	log     *slog.Logger
	now     func() time.Time
	newID   func() string
	// access rebuilds a token link issuer's current principal, so a link
	// stops working when its issuer loses access. security records signed
	// URL issuance in the security audit log.
	access   func(ctx context.Context, tenantID, userID string) (model.Principal, error)
	security interfaces.AuditRecorder
	// router reaches assets published on tenants' storage connections;
	// nil serves only the platform's bucket.
	router interfaces.StorageRouter
}

// Route lets delivery serve assets published on tenants' storage
// connections. An asset is always read through its own connection, never
// the organization's current default.
func (s *Service) Route(router interfaces.StorageRouter) *Service {
	s.router = router
	return s
}

// readyStorage is where an asset's published bytes are.
type readyStorage struct {
	storage interfaces.ObjectDelivery
	bucket  string
	prefix  string
	// platform: the platform's bucket, the only one behind the CDN.
	platform bool
	// presign: the connection can sign a download URL itself.
	presign bool
}

func (s *Service) ready(ctx context.Context, asset model.Asset) (readyStorage, error) {
	if model.IsPlatformConnection(asset.StorageConnectionID) {
		return readyStorage{storage: s.storage, bucket: s.cfg.Bucket, prefix: s.cfg.Prefix, platform: true, presign: true}, nil
	}
	if s.router == nil {
		return readyStorage{}, model.ErrStorageNotConfigured
	}
	loc, err := s.router.Locate(ctx, asset.TenantID, asset.StorageConnectionID)
	if err != nil {
		return readyStorage{}, err
	}
	return readyStorage{storage: loc.Storage, bucket: loc.ReadyBucket, prefix: loc.ReadyPrefix,
		presign: loc.Capabilities.PresignedDownload}, nil
}

// Guard makes token links check their issuer's current access, and
// records issued URLs in the security audit log. The API sets both.
func (s *Service) Guard(access func(ctx context.Context, tenantID, userID string) (model.Principal, error), security interfaces.AuditRecorder) *Service {
	s.access, s.security = access, security
	return s
}

// NewService builds the delivery service. tokens may be nil, which turns
// token links off.
func NewService(
	assets interfaces.AssetRepository,
	storage interfaces.ObjectDelivery,
	audit interfaces.DeliveryAuditRepository,
	tokens interfaces.DownloadTokenRepository,
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
	switch cfg.URLMode {
	case "":
		cfg.URLMode = ModeStorage
	case ModeStorage:
	case ModeToken:
		if tokens == nil {
			return nil, errors.New("token download links need a token repository")
		}
	default:
		return nil, fmt.Errorf("unknown download URL mode %q", cfg.URLMode)
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
		assets: assets, storage: storage, audit: audit, tokens: tokens, cfg: cfg, log: log,
		now:   func() time.Time { return time.Now().UTC() },
		newID: newEventID,
	}, nil
}

// AssetInfo is the metadata an owner may see. It never includes the
// bucket, the object key or provider validators.
type AssetInfo struct {
	ID              string
	FileName        string
	ContentType     string
	SizeBytes       int64
	ChecksumSHA256  string
	Dataset         string
	Status          model.AssetStatus
	RejectionReason string
	// Deliverable reports whether a download would be served now.
	Deliverable bool
	// ETag is the download validator (set when Deliverable).
	ETag        string
	PublishedAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// URLRequest is a download-URL request. TTL 0 means the default; Mode ""
// means the configured mode.
type URLRequest struct {
	TTL       time.Duration
	Filename  string
	Mode      string
	RequestID string
}

type DownloadURL struct {
	URL       string
	ExpiresAt time.Time
	Via       string // "storage", "cdn" or "token"
	// Revocable reports whether RevokeLinks can cancel this URL.
	Revocable bool
}

// DownloadRequest carries the HTTP request fields delivery decides on.
// All of them are untrusted.
type DownloadRequest struct {
	Range             string
	IfRange           string
	IfMatch           string
	IfNoneMatch       string
	IfModifiedSince   string
	IfUnmodifiedSince string
	Filename          string
	// HeadOnly answers with metadata only; no bytes are read.
	HeadOnly  bool
	RequestID string
}

// Download is an authorized stream of all or part of an asset. Body is nil
// for HEAD requests and for NotModified answers.
type Download struct {
	Body               io.ReadCloser
	ContentType        string
	ContentDisposition string
	ETag               string
	LastModified       time.Time
	TotalSize          int64
	First, Last        int64
	Partial            bool
	NotModified        bool

	principal model.Principal
	assetID   string
	operation string
	requestID string
}

// Length is how many body bytes the response carries.
func (d Download) Length() int64 {
	if d.TotalSize == 0 {
		return 0
	}
	return d.Last - d.First + 1
}

// Metadata returns one of the caller's organization's assets, in any
// state but DELETED.
func (s *Service) Metadata(ctx context.Context, principal model.Principal, assetID string) (AssetInfo, error) {
	asset, err := s.owned(ctx, principal, assetID, model.PermAssetsRead)
	if err != nil {
		return AssetInfo{}, err
	}
	info := AssetInfo{
		ID: asset.ID, FileName: asset.FileName, ContentType: SafeContentType(asset.ContentType),
		SizeBytes: asset.SizeBytes, ChecksumSHA256: asset.ChecksumSHA256, Dataset: asset.Dataset, Status: asset.Status,
		RejectionReason: asset.RejectionReason, PublishedAt: asset.PublishedAt,
		CreatedAt: asset.CreatedAt, UpdatedAt: asset.UpdatedAt,
	}
	r, err := s.ready(ctx, asset)
	if err == nil && asset.Status == model.AssetStatusAvailable && asset.ObjectKey == promotedKey(r.prefix, asset) {
		info.Deliverable = true
		info.ETag = entityTag(asset)
	}
	return info, nil
}

// CreateDownloadURL issues a short-lived URL for an AVAILABLE asset.
// Storage and CDN URLs are bearer credentials that work until they expire;
// token URLs are checked on every use and can be revoked. The URL is
// returned only after the decision is durably audited, and never stored.
func (s *Service) CreateDownloadURL(ctx context.Context, principal model.Principal, assetID string, req URLRequest) (DownloadURL, error) {
	mode := req.Mode
	if mode == "" {
		mode = s.cfg.URLMode
	}
	if mode != ModeStorage && mode != ModeToken {
		return DownloadURL{}, fmt.Errorf("%w: mode must be storage or token", ErrInvalidRequest)
	}
	if mode == ModeToken && s.tokens == nil {
		return DownloadURL{}, fmt.Errorf("%w: token links are not enabled", ErrInvalidRequest)
	}

	asset, r, err := s.deliverable(ctx, principal, assetID, model.DeliveryOpDownloadURL, req.RequestID)
	if err != nil {
		return DownloadURL{}, err
	}
	if mode == ModeStorage && !r.presign {
		// The connection cannot sign a URL (an Azure SAS-token connection):
		// a token link streams through the API instead.
		if s.tokens == nil {
			return DownloadURL{}, fmt.Errorf("%w: this asset's storage cannot sign download URLs", ErrInvalidRequest)
		}
		mode = ModeToken
	}

	ttl := req.TTL
	if ttl <= 0 {
		ttl = s.cfg.DefaultTTL
	}
	if ttl > s.cfg.MaxTTL {
		ttl = s.cfg.MaxTTL
	}
	expiresAt := s.now().Add(ttl)
	fail := func(reason string, err error) (DownloadURL, error) {
		s.fail(ctx, principal, asset.ID, model.DeliveryOpDownloadURL, req.RequestID, reason, err)
		return DownloadURL{}, ErrStorageUnavailable
	}

	var result DownloadURL
	switch {
	case mode == ModeToken:
		raw, err := s.issueToken(ctx, principal, asset, req.Filename, expiresAt)
		if err != nil {
			return fail("token_issue_failed", err)
		}
		result = DownloadURL{URL: TokenPathPrefix + raw, ExpiresAt: expiresAt, Via: ModeToken, Revocable: true}
	case s.cfg.CDN != nil && r.platform:
		signed, err := s.cfg.CDN.Signer.SignURL(s.cdnURL(asset.ObjectKey), expiresAt)
		if err != nil {
			return fail("cdn_signing_failed", err)
		}
		result = DownloadURL{URL: signed, ExpiresAt: expiresAt, Via: "cdn"}
	default:
		presigned, err := r.storage.PresignGet(ctx, r.bucket, asset.ObjectKey, ttl, interfaces.DownloadOptions{
			ContentDisposition: attachment(req.Filename, asset.FileName),
			ContentType:        SafeContentType(asset.ContentType),
			VersionID:          asset.StorageVersionID,
		})
		if err != nil {
			return fail("presign_failed", err)
		}
		result = DownloadURL{URL: presigned, ExpiresAt: expiresAt, Via: ModeStorage}
	}

	event := s.event(principal, asset.ID, model.DeliveryOpDownloadURL, model.DeliveryAllowed, req.RequestID)
	event.Reason = "via_" + result.Via
	if err := s.record(ctx, event); err != nil {
		return DownloadURL{}, ErrAuditUnavailable
	}
	s.cfg.Metrics.Add("urls_issued_"+result.Via, 1)
	if s.security != nil {
		// Never the URL itself: it is a bearer credential.
		e := audit.From(ctx, principal, "asset.download_url", "asset", asset.ID)
		e.Details = map[string]any{"via": result.Via, "expires_at": result.ExpiresAt.UTC().Format(time.RFC3339)}
		s.security.Record(ctx, e)
	}
	return result, nil
}

// Open authorizes a proxied download and opens the requested byte range.
// Authorization runs on every request, unlike a presigned URL.
func (s *Service) Open(ctx context.Context, principal model.Principal, assetID string, req DownloadRequest) (Download, error) {
	asset, r, err := s.deliverable(ctx, principal, assetID, model.DeliveryOpProxy, req.RequestID)
	if err != nil {
		return Download{}, err
	}
	return s.open(ctx, principal, asset, r, model.DeliveryOpProxy, req)
}

// OpenToken serves a token link. The token's issuer must still be allowed
// to download the asset: deletion, rejection and revocation all take
// effect on the next request.
func (s *Service) OpenToken(ctx context.Context, rawToken string, req DownloadRequest) (Download, error) {
	if s.tokens == nil || !wellFormedToken(rawToken) {
		s.cfg.Metrics.Add("tokens_invalid", 1)
		return Download{}, ErrTokenInvalid
	}
	token, err := s.tokens.GetByHash(ctx, hashToken(rawToken))
	if errors.Is(err, model.ErrDownloadTokenNotFound) {
		s.cfg.Metrics.Add("tokens_invalid", 1)
		return Download{}, ErrTokenInvalid
	}
	if err != nil {
		return Download{}, fmt.Errorf("load download token: %w", err)
	}

	principal := model.Principal{TenantID: token.TenantID, UserID: token.PrincipalID, Permissions: model.PermAssetsDownload}
	if s.access != nil {
		// The link carries its issuer's authority, checked as it is now: a
		// removed member's or a revoked role's links stop working.
		current, err := s.access(ctx, token.TenantID, token.PrincipalID)
		if err != nil || !current.Can(model.PermAssetsDownload) {
			s.cfg.Metrics.Add("tokens_invalid", 1)
			s.deny(ctx, principal, token.AssetID, model.DeliveryOpToken, req.RequestID, "issuer_not_allowed")
			return Download{}, ErrTokenInvalid
		}
		principal = current
	}
	switch {
	case token.RevokedAt != nil:
		s.cfg.Metrics.Add("tokens_invalid", 1)
		s.deny(ctx, principal, token.AssetID, model.DeliveryOpToken, req.RequestID, "token_revoked")
		return Download{}, ErrTokenInvalid
	case !s.now().Before(token.ExpiresAt):
		s.cfg.Metrics.Add("tokens_invalid", 1)
		s.deny(ctx, principal, token.AssetID, model.DeliveryOpToken, req.RequestID, "token_expired")
		return Download{}, ErrTokenInvalid
	}

	asset, r, err := s.deliverable(ctx, principal, token.AssetID, model.DeliveryOpToken, req.RequestID)
	if err != nil {
		return Download{}, err
	}
	if req.Filename == "" {
		req.Filename = token.Filename
	}
	return s.open(ctx, principal, asset, r, model.DeliveryOpToken, req)
}

// RevokeLinks revokes every token link the caller's asset has. Presigned
// storage URLs and CDN URLs cannot be revoked; they expire.
func (s *Service) RevokeLinks(ctx context.Context, principal model.Principal, assetID, requestID string) (int, error) {
	if s.tokens == nil {
		return 0, fmt.Errorf("%w: token links are not enabled", ErrInvalidRequest)
	}
	asset, err := s.owned(ctx, principal, assetID, model.PermAssetsDelete)
	if err != nil {
		return 0, err
	}
	revoked, err := s.tokens.RevokeForAsset(ctx, asset.TenantID, asset.ID, s.now())
	if err != nil {
		return 0, fmt.Errorf("revoke download links: %w", err)
	}
	event := s.event(principal, asset.ID, model.DeliveryOpTokenRevoke, model.DeliveryAllowed, requestID)
	event.Reason = "revoked_" + strconv.Itoa(revoked)
	_ = s.record(ctx, event)
	s.cfg.Metrics.Add("tokens_revoked", int64(revoked))
	return revoked, nil
}

// TransferFailed records a stream that broke after its headers were sent.
// The request's context is usually cancelled by then, so the audit write
// gets its own short deadline.
func (s *Service) TransferFailed(ctx context.Context, d Download, bytesSent int64, reason string) {
	s.cfg.Metrics.Add("downloads_interrupted", 1)
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	event := s.event(d.principal, d.assetID, d.operation, model.DeliveryFailed, d.requestID)
	event.Reason = reason
	event.ByteRange = d.byteRange()
	event.BytesSent = bytesSent
	_ = s.record(auditCtx, event)
}

// TransferDone counts a completed stream.
func (s *Service) TransferDone(d Download, bytesSent int64) {
	s.cfg.Metrics.Add("downloads_completed", 1)
	s.cfg.Metrics.Add("bytes_sent", bytesSent)
}

// open applies the conditional headers, validates the range against the
// published size, pins the read to the published object version, audits
// the decision and only then opens the stream.
func (s *Service) open(ctx context.Context, principal model.Principal, asset model.Asset, r readyStorage, operation string, req DownloadRequest) (Download, error) {
	d := Download{
		ContentType:        SafeContentType(asset.ContentType),
		ContentDisposition: attachment(req.Filename, asset.FileName),
		ETag:               entityTag(asset),
		LastModified:       lastModified(asset),
		TotalSize:          asset.SizeBytes,
		principal:          principal,
		assetID:            asset.ID,
		operation:          operation,
		requestID:          req.RequestID,
	}

	// RFC 9110 section 13.2.2 order. A date condition is only used when
	// its ETag counterpart is absent, and an unparsable date is ignored.
	switch {
	case req.IfMatch != "":
		if !matchesETag(req.IfMatch, d.ETag, false) {
			return d, ErrPreconditionFailed
		}
	case req.IfUnmodifiedSince != "":
		if at, err := parseHTTPTime(req.IfUnmodifiedSince); err == nil && d.LastModified.After(at) {
			return d, ErrPreconditionFailed
		}
	}

	notModified := false
	switch {
	case req.IfNoneMatch != "":
		notModified = matchesETag(req.IfNoneMatch, d.ETag, true)
	case req.IfModifiedSince != "":
		at, err := parseHTTPTime(req.IfModifiedSince)
		notModified = err == nil && !d.LastModified.After(at)
	}
	if notModified {
		d.NotModified = true
		s.cfg.Metrics.Add("not_modified", 1)
		return d, nil
	}
	rangeHeader := req.Range
	if rangeHeader != "" && req.IfRange != "" && !ifRangeMatches(req.IfRange, d.ETag, d.LastModified) {
		// The client's copy is a different version: send the whole object.
		rangeHeader = ""
	}

	first, last, partial, err := parseRange(rangeHeader, asset.SizeBytes)
	if err != nil {
		s.cfg.Metrics.Add("range_not_satisfiable", 1)
		return d, err
	}
	d.First, d.Last, d.Partial = first, last, partial

	cond, err := s.readCondition(ctx, principal, asset, r, operation, req)
	if err != nil {
		return d, err
	}
	if req.HeadOnly {
		return d, nil
	}

	event := s.event(principal, asset.ID, operation, model.DeliveryAllowed, req.RequestID)
	event.ByteRange = d.byteRange()
	if err := s.record(ctx, event); err != nil {
		return Download{}, ErrAuditUnavailable
	}

	requestLast := last
	if !partial {
		requestLast = -1
	}
	body, err := r.storage.OpenRange(ctx, r.bucket, asset.ObjectKey, first, requestLast, cond)
	if err != nil {
		return Download{}, s.storageError(ctx, principal, asset, operation, req.RequestID, err)
	}

	d.Body = body
	if partial {
		s.cfg.Metrics.Add("downloads_partial", 1)
	} else {
		s.cfg.Metrics.Add("downloads_full", 1)
	}
	return d, nil
}

// readCondition pins reads to the stored object the asset was published
// as. Assets published before validators were recorded are checked
// against a fresh HEAD (size must match), and that ETag is used for the
// read, so one response never mixes versions. A HEAD request also checks
// the stored object, so it answers like the GET would.
func (s *Service) readCondition(ctx context.Context, principal model.Principal, asset model.Asset, r readyStorage, operation string, req DownloadRequest) (interfaces.ReadCondition, error) {
	cond := interfaces.ReadCondition{IfMatch: asset.StorageETag, VersionID: asset.StorageVersionID}
	if cond.IfMatch != "" && !req.HeadOnly {
		return cond, nil
	}

	stored, err := r.storage.HeadObject(ctx, r.bucket, asset.ObjectKey)
	if err != nil {
		return cond, s.storageError(ctx, principal, asset, operation, req.RequestID, err)
	}
	if stored.SizeBytes != asset.SizeBytes || (cond.IfMatch != "" && stored.ETag != cond.IfMatch) {
		return cond, s.storageError(ctx, principal, asset, operation, req.RequestID, interfaces.ErrPreconditionFailed)
	}
	if cond.IfMatch == "" {
		cond.IfMatch = stored.ETag
	}
	return cond, nil
}

// storageError maps a provider failure to a client-safe error. A missing
// object is checked against the asset's current state, so a concurrent
// deletion answers like a deleted asset.
func (s *Service) storageError(ctx context.Context, principal model.Principal, asset model.Asset, operation, requestID string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	switch {
	case errors.Is(err, interfaces.ErrObjectNotFound):
		current, getErr := s.assets.GetByID(ctx, asset.TenantID, asset.ID)
		if errors.Is(getErr, model.ErrAssetNotFound) || (getErr == nil && current.Status == model.AssetStatusDeleted) {
			s.deny(ctx, principal, asset.ID, operation, requestID, "deleted")
			return ErrNotFound
		}
		if getErr == nil && current.Status != model.AssetStatusAvailable {
			s.deny(ctx, principal, asset.ID, operation, requestID, "not_available")
			return ErrNotAvailable
		}
		s.fail(ctx, principal, asset.ID, operation, requestID, "object_missing", err)
		return ErrStorageUnavailable
	case errors.Is(err, interfaces.ErrPreconditionFailed), errors.Is(err, interfaces.ErrInvalidRange):
		s.cfg.Metrics.Add("version_mismatch", 1)
		s.fail(ctx, principal, asset.ID, operation, requestID, "version_mismatch", err)
		return ErrVersionMismatch
	case errors.Is(err, interfaces.ErrStorageThrottled):
		s.cfg.Metrics.Add("storage_throttled", 1)
		s.fail(ctx, principal, asset.ID, operation, requestID, "throttled", err)
		return ErrStorageBusy
	default:
		s.cfg.Metrics.Add("storage_errors", 1)
		s.fail(ctx, principal, asset.ID, operation, requestID, "storage_error", err)
		return ErrStorageUnavailable
	}
}

// owned loads an asset of the caller's organization, for a caller with
// perm. Anything the caller may not see is ErrNotFound, exactly like a
// missing asset: another tenant's asset is never distinguishable.
func (s *Service) owned(ctx context.Context, principal model.Principal, assetID string, perm model.Permission) (model.Asset, error) {
	if !principal.Valid() {
		return model.Asset{}, model.ErrUnauthenticated
	}
	if err := principal.Require(perm); err != nil {
		return model.Asset{}, err
	}
	if strings.TrimSpace(assetID) == "" || len(assetID) > 200 {
		return model.Asset{}, ErrNotFound
	}

	asset, err := s.assets.GetByID(ctx, principal.TenantID, assetID)
	if errors.Is(err, model.ErrAssetNotFound) {
		return model.Asset{}, ErrNotFound
	}
	if err != nil {
		return model.Asset{}, fmt.Errorf("load asset: %w", err)
	}
	// Assets belong to the organization: its members see them as their
	// role allows. The query above is already scoped to the tenant.
	if asset.Status == model.AssetStatusDeleted {
		return model.Asset{}, ErrNotFound
	}
	return *asset, nil
}

// deliverable is owned plus the delivery rules: AVAILABLE (so verified,
// validated, scanned and promoted), and stored at the promoted key derived
// from its tenant and ID.
func (s *Service) deliverable(ctx context.Context, principal model.Principal, assetID, operation, requestID string) (model.Asset, readyStorage, error) {
	asset, err := s.owned(ctx, principal, assetID, model.PermAssetsDownload)
	switch {
	case errors.Is(err, model.ErrUnauthenticated), errors.Is(err, model.ErrForbidden), errors.Is(err, model.ErrTenantRequired):
		s.deny(ctx, principal, assetID, operation, requestID, "forbidden")
		return model.Asset{}, readyStorage{}, err
	case errors.Is(err, ErrNotFound):
		s.deny(ctx, principal, assetID, operation, requestID, "not_found")
		return model.Asset{}, readyStorage{}, err
	case err != nil:
		return model.Asset{}, readyStorage{}, err
	}

	if asset.Status != model.AssetStatusAvailable {
		s.deny(ctx, principal, assetID, operation, requestID, strings.ToLower(string(asset.Status)))
		return model.Asset{}, readyStorage{}, ErrNotAvailable
	}
	r, err := s.ready(ctx, asset)
	if err != nil {
		s.fail(ctx, principal, assetID, operation, requestID, "storage_connection", err)
		return model.Asset{}, readyStorage{}, ErrStorageUnavailable
	}
	if asset.ObjectKey != promotedKey(r.prefix, asset) {
		s.log.Error("available asset is not at its promoted key; refusing delivery",
			"asset_id", asset.ID, "tenant_id", asset.TenantID)
		s.deny(ctx, principal, assetID, operation, requestID, "key_mismatch")
		return model.Asset{}, readyStorage{}, ErrNotAvailable
	}
	return asset, r, nil
}

func (s *Service) issueToken(ctx context.Context, principal model.Principal, asset model.Asset, filename string, expiresAt time.Time) (string, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", err
	}
	raw := base64.RawURLEncoding.EncodeToString(secret[:])
	err := s.tokens.Create(ctx, model.DownloadToken{
		ID: s.newID(), TokenHash: hashToken(raw), TenantID: asset.TenantID, AssetID: asset.ID,
		PrincipalID: principal.UserID, Filename: SafeFilename(filename), CreatedAt: s.now(), ExpiresAt: expiresAt,
	})
	if err != nil {
		return "", err
	}
	return raw, nil
}

// promotedKey is where promotion puts an asset under its connection's
// ready prefix.
func promotedKey(prefix string, asset model.Asset) string {
	return fmt.Sprintf("%s/%s/%s", prefix, asset.TenantID, asset.ID)
}

// cdnURL is the CDN URL for an object key, each segment escaped.
func (s *Service) cdnURL(objectKey string) string {
	segments := strings.Split(objectKey, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.TrimRight(s.cfg.CDN.BaseURL, "/") + "/" + strings.Join(segments, "/")
}

func (s *Service) event(principal model.Principal, assetID, operation, outcome, requestID string) model.DeliveryAuditEvent {
	return model.DeliveryAuditEvent{
		ID: s.newID(), TenantID: principal.TenantID, PrincipalID: principal.UserID, AssetID: assetID,
		Operation: operation, Outcome: outcome, RequestID: requestID, CreatedAt: s.now(),
	}
}

func (s *Service) record(ctx context.Context, event model.DeliveryAuditEvent) error {
	err := s.audit.Record(ctx, event)
	if err != nil {
		s.log.Error("record delivery audit event", "operation", event.Operation, "outcome", event.Outcome, "error", retry.Sanitize(err))
	}
	return err
}

// deny audits a refusal. It is best effort: refusing still refuses.
func (s *Service) deny(ctx context.Context, principal model.Principal, assetID, operation, requestID, reason string) {
	s.cfg.Metrics.Add("denied", 1)
	s.log.Info("delivery denied", "asset_id", assetID, "tenant_id", principal.TenantID, "operation", operation,
		"reason", reason, "request_id", requestID)
	event := s.event(principal, assetID, operation, model.DeliveryDenied, requestID)
	event.Reason = reason
	_ = s.record(ctx, event)
}

// fail logs a provider failure without its raw text reaching the client.
func (s *Service) fail(ctx context.Context, principal model.Principal, assetID, operation, requestID, reason string, err error) {
	s.log.Error("delivery failed", "asset_id", assetID, "tenant_id", principal.TenantID,
		"operation", operation, "reason", reason, "request_id", requestID, "error", retry.Sanitize(err))
	event := s.event(principal, assetID, operation, model.DeliveryFailed, requestID)
	event.Reason = reason
	_ = s.record(ctx, event)
}

func (d Download) byteRange() string {
	if !d.Partial {
		return ""
	}
	return fmt.Sprintf("%d-%d/%d", d.First, d.Last, d.TotalSize)
}

// entityTag is a strong validator for the published content: its verified
// SHA-256. It is not the provider's ETag, which is not a content digest
// for multipart objects.
func entityTag(asset model.Asset) string {
	if asset.ChecksumSHA256 == "" {
		return `"` + asset.ID + `"`
	}
	return `"` + asset.ChecksumSHA256 + `"`
}

func lastModified(asset model.Asset) time.Time {
	at := asset.UpdatedAt
	if asset.PublishedAt != nil {
		at = *asset.PublishedAt
	}
	return at.UTC().Truncate(time.Second)
}

// matchesETag checks an If-Match / If-None-Match list. "*" matches any
// existing representation. If-Match uses strong comparison (weak tags never
// match); If-None-Match uses weak comparison.
func matchesETag(header, etag string, weak bool) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" {
			return true
		}
		if strings.HasPrefix(candidate, "W/") {
			if !weak {
				continue
			}
			candidate = candidate[2:]
		}
		if candidate == etag {
			return true
		}
	}
	return false
}

// ifRangeMatches: a strong ETag equal to ours, or an HTTP date equal to
// Last-Modified. Anything else means "send the whole object".
func ifRangeMatches(header, etag string, modified time.Time) bool {
	header = strings.TrimSpace(header)
	if strings.HasPrefix(header, `"`) {
		return header == etag
	}
	if strings.HasPrefix(header, "W/") {
		return false
	}
	at, err := parseHTTPTime(header)
	return err == nil && at.Equal(modified)
}

// SafeContentType returns a content type that is safe to send with a
// download: well-formed, without parameters an attacker could abuse, and
// never one a browser would run as a page or script.
func SafeContentType(declared string) string {
	base, _, err := mime.ParseMediaType(declared)
	if err != nil || strings.ContainsAny(base, "\r\n") {
		return "application/octet-stream"
	}
	switch base {
	case "text/html", "application/xhtml+xml", "image/svg+xml", "text/xml", "application/xml",
		"application/javascript", "text/javascript", "application/ecmascript", "text/ecmascript":
		return "application/octet-stream"
	}
	return base
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
		// Bidirectional overrides can disguise an extension.
		if (r >= '\u202a' && r <= '\u202e') || (r >= '\u2066' && r <= '\u2069') {
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

// hashToken is how tokens are stored and looked up.
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// wellFormedToken accepts exactly what issueToken produces: 32 random
// bytes, base64url without padding.
func wellFormedToken(raw string) bool {
	if len(raw) != 43 {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(raw)
	return err == nil
}

func newEventID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
