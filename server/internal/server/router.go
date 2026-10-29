package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	httpapi "github.com/sanjeev0303/object-storage-platform/internal/handler/http"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/ratelimit"
	"github.com/sanjeev0303/object-storage-platform/internal/server/middleware"
	assetservice "github.com/sanjeev0303/object-storage-platform/internal/service/asset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
	"github.com/sanjeev0303/object-storage-platform/internal/service/catalog"
	datasetservice "github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/delivery"
	"github.com/sanjeev0303/object-storage-platform/internal/service/events"
	"github.com/sanjeev0303/object-storage-platform/internal/service/identity"
	ingestionservice "github.com/sanjeev0303/object-storage-platform/internal/service/ingestion"
	"github.com/sanjeev0303/object-storage-platform/internal/service/processing"
	"github.com/sanjeev0303/object-storage-platform/internal/service/quota"
	"github.com/sanjeev0303/object-storage-platform/internal/service/storageconn"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
	"github.com/sanjeev0303/object-storage-platform/internal/service/webhook"
)

// Routes configures the API. Every route but sign-in, token redemption,
// download links and the health check needs an authenticated caller, and
// every data route an organization the caller is an active member of.
// A nil service leaves its routes out.
type Routes struct {
	// Resolver authenticates requests: middleware.BearerResolver over the
	// identity service. Nil answers every authenticated route with 401.
	Resolver middleware.Resolver
	// Identity serves sign-in, accounts, organizations and administration.
	Identity    *identity.Service
	Quotas      *quota.Service
	Audit       *audit.Service
	DeadLetters *events.GuardedAdmin

	Uploads    *uploadservice.Service
	Multipart  *uploadservice.MultipartService
	Delivery   *delivery.Service
	Lifecycle  *assetservice.Service
	Ingestion  *ingestionservice.Service
	Quarantine *processing.Quarantine
	Catalog    *catalog.Service
	// Datasets serves managed datasets (Phase 10). When set, it also owns
	// POST /api/v1/datasets and GET /api/v1/datasets/{id}.
	Datasets *datasetservice.Service
	// StorageConnections manages organizations' storage connections
	// (Phase 11).
	StorageConnections *storageconn.Service
	// StorageCapabilities reports the delivery bucket's provider
	// capabilities (GET /api/v1/catalog/capabilities); optional.
	StorageCapabilities func(*http.Request) (interfaces.StorageCapabilities, error)
	// Webhooks manages the organization's webhook endpoints.
	Webhooks *webhook.Service

	// ProxyEnabled adds GET/HEAD /api/v1/assets/{id}/content and the token
	// link routes, which stream through the API.
	ProxyEnabled bool
	Stream       httpapi.StreamConfig

	// Limiter and Rates rate-limit sensitive and expensive operations, by
	// name (see config.Security.Rates).
	Limiter *ratelimit.Limiter
	Rates   map[string]ratelimit.Rule
	// TrustProxyHeaders takes client addresses from X-Forwarded-For.
	TrustProxyHeaders bool
	Log               *slog.Logger

	// Ready, when set, serves GET /ready: a dependency check (the
	// database), apart from the liveness of GET /health.
	Ready func(context.Context) error
	// Operations, when set, serves GET /api/v1/operations/status: counts of
	// the caller's organization's background work.
	Operations func(ctx context.Context, tenantID string, now time.Time) (model.OperationsStatus, error)
	// MetricsAddr is where the process serves its metrics; the caller
	// starts that listener (metrics.Serve), not the router.
	MetricsAddr string
}

// NewRouter connects HTTP routes to their handlers.
func NewRouter(rt Routes) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /health", httpapi.NewHealthHandler())
	if rt.Ready != nil {
		mux.Handle("GET /ready", httpapi.NewReadinessHandler(rt.Ready))
	}
	if rt.Limiter == nil {
		rt.Limiter = ratelimit.New()
	}
	if rt.Log == nil {
		rt.Log = slog.Default()
	}

	limit := func(op string, key func(*http.Request) string, h http.Handler) http.Handler {
		rule, ok := rt.Rates[op]
		if !ok {
			return h
		}
		return middleware.RateLimit(rt.Limiter, op, rule, key, h)
	}
	// account routes need a signed-in caller; tenant routes also an
	// organization, and count against its request quota.
	account := func(h http.Handler) http.Handler {
		return middleware.Authenticate(rt.Resolver, h)
	}
	tenant := func(h http.Handler) http.Handler {
		if rt.Quotas != nil {
			h = middleware.TenantRequests(rt.Limiter, rt.requestsPerMinute, h)
		}
		return middleware.Authenticate(rt.Resolver, middleware.RequireTenant(h))
	}
	byTenant := func(op string, h http.HandlerFunc) http.Handler {
		return tenant(limit(op, middleware.ByTenant, h))
	}

	if rt.Uploads != nil {
		uploads := httpapi.NewUploadHandler(rt.Uploads, rt.Log)
		mux.Handle("POST /api/v1/uploads", byTenant("upload_init", uploads.Create))
		mux.Handle("GET /api/v1/uploads", byTenant("listing", uploads.List))
		mux.Handle("GET /api/v1/uploads/{id}", byTenant("listing", uploads.Get))
	}
	if rt.Multipart != nil {
		multipart := httpapi.NewMultipartUploadHandler(rt.Multipart, rt.Uploads, rt.Log)
		for pattern, h := range multipart.Routes() {
			op := "upload_finish"
			if pattern == "POST /api/v1/uploads/multipart" {
				op = "upload_init"
			}
			mux.Handle(pattern, byTenant(op, h))
		}
	}

	if rt.Delivery != nil {
		assets := httpapi.NewAssetHandler(rt.Delivery, rt.Log)
		downloads := httpapi.NewDownloadHandler(rt.Delivery, rt.Log, rt.Stream)
		// GET patterns also answer HEAD.
		mux.Handle("GET /api/v1/assets/{id}", byTenant("listing", assets.Get))
		mux.Handle("POST /api/v1/assets/{id}/download-url", byTenant("download_url", downloads.CreateURL))
		mux.Handle("DELETE /api/v1/assets/{id}/download-links", byTenant("download_url", downloads.RevokeLinks))
		if rt.ProxyEnabled {
			mux.Handle("GET /api/v1/assets/{id}/content", byTenant("download_url", downloads.Content))
			// The token is the credential: no Authorization header. The
			// service checks its issuer's current access on every use.
			mux.Handle("GET /api/v1/downloads/{token}", limit("download_url", middleware.ByAddress, http.HandlerFunc(downloads.TokenContent)))
		}
	}
	if rt.Lifecycle != nil {
		lifecycle := httpapi.NewLifecycleHandler(rt.Lifecycle, rt.Log)
		mux.Handle("DELETE /api/v1/assets/{id}", byTenant("upload_finish", lifecycle.Delete))
		mux.Handle("POST /api/v1/assets/{id}/restore", byTenant("upload_finish", lifecycle.Restore))
		mux.Handle("GET /api/v1/assets/{id}/lifecycle", byTenant("listing", lifecycle.Get))
	}
	if rt.Quarantine != nil || rt.Lifecycle != nil {
		governance := httpapi.NewGovernanceHandler(rt.Quarantine, rt.Lifecycle, rt.Log)
		for pattern, h := range governance.Routes() {
			mux.Handle(pattern, byTenant("admin", h))
		}
	}

	if rt.Catalog != nil {
		reads, changes := httpapi.NewCatalogHandler(rt.Catalog, rt.StorageCapabilities, rt.Log).Routes()
		for pattern, h := range reads {
			mux.Handle(pattern, byTenant("listing", h))
		}
		for pattern, h := range changes {
			mux.Handle(pattern, byTenant("upload_finish", h))
		}
	}

	if rt.Ingestion != nil {
		ingestion := httpapi.NewIngestionHandler(rt.Ingestion, rt.Log)
		for pattern, h := range ingestion.Routes() {
			if rt.Datasets != nil && (pattern == "POST /api/v1/datasets" || pattern == "GET /api/v1/datasets/{id}") {
				continue // the dataset handler serves these
			}
			mux.Handle(pattern, byTenant(ingestionOperation(pattern), h))
		}
	}
	if rt.Datasets != nil {
		datasets := httpapi.NewDatasetHandler(rt.Datasets, rt.Log)
		for pattern, h := range datasets.Routes() {
			mux.Handle(pattern, byTenant(datasetOperation(pattern), h))
		}
	}

	if rt.StorageConnections != nil {
		connections := httpapi.NewStorageConnectionHandler(rt.StorageConnections, rt.Log)
		for pattern, h := range connections.Routes() {
			mux.Handle(pattern, byTenant(storageOperation(pattern), h))
		}
	}

	if rt.Webhooks != nil {
		hooks := httpapi.NewWebhookHandler(rt.Webhooks, rt.Log)
		for pattern, h := range hooks.Routes() {
			op := "admin"
			if strings.HasPrefix(pattern, "GET ") {
				op = "listing"
			}
			mux.Handle(pattern, byTenant(op, h))
		}
	}

	if rt.Operations != nil {
		ops := httpapi.NewOperationsHandler(rt.Operations, rt.Log)
		mux.Handle("GET /api/v1/operations/status", byTenant("listing", ops.Status))
	}

	if rt.Identity != nil && rt.Quotas != nil && rt.Audit != nil {
		ids := httpapi.NewIdentityHandler(rt.Identity, rt.Quotas, rt.Audit, rt.Resolver, rt.Log)
		routes := ids.Routes()
		// The identity service throttles the public routes itself: sign-in
		// per account and address, refresh and token redemption per address.
		for pattern, h := range routes.Public {
			mux.Handle(pattern, h)
		}
		for pattern, h := range routes.Account {
			mux.Handle(pattern, account(h))
		}
		for pattern, h := range routes.Tenant {
			mux.Handle(pattern, byTenant("membership", h))
		}

		// SCIM: identity providers authenticate with the organization's
		// SCIM token, rate-limited per address.
		for pattern, h := range httpapi.NewSCIMHandler(rt.Identity, rt.Log).Routes() {
			mux.Handle(pattern, limit("admin", middleware.ByAddress, h))
		}

		admin := httpapi.NewAdminHandler(rt.Identity, rt.Quotas, rt.Audit, rt.DeadLetters, rt.Log)
		for pattern, h := range admin.Routes() {
			mux.Handle(pattern, account(limit("admin", middleware.ByUser, h)))
		}
	}

	// Every request gets an ID and its client address; events and audit
	// records written while handling it carry both.
	return middleware.RequestID(middleware.ClientIP(rt.TrustProxyHeaders, mux))
}

// ingestionOperation names the rate limit an ingestion route counts
// against. Per-entry calls have their own limit, so a bulk job neither
// starves nor is starved by interactive uploads.
func ingestionOperation(pattern string) string {
	switch pattern {
	case "POST /api/v1/ingestion/jobs", "POST /api/v1/datasets":
		return "job_create"
	case "PUT /api/v1/ingestion/jobs/{id}/manifest", "POST /api/v1/ingestion/jobs/{id}/manifest/staged-url",
		"POST /api/v1/ingestion/jobs/{id}/manifest/staged":
		return "manifest"
	case "POST /api/v1/ingestion/jobs/{id}/claims",
		"POST /api/v1/ingestion/jobs/{id}/entries/{position}/upload-url",
		"POST /api/v1/ingestion/jobs/{id}/entries/{position}/parts/urls",
		"POST /api/v1/ingestion/jobs/{id}/entries/{position}/parts/confirm",
		"POST /api/v1/ingestion/jobs/{id}/entries/{position}/heartbeat",
		"POST /api/v1/ingestion/jobs/{id}/entries/{position}/complete":
		return "ingestion_entry"
	}
	if strings.HasPrefix(pattern, "GET ") {
		return "listing"
	}
	return "upload_finish"
}

// datasetOperation names the rate limit a dataset route counts against:
// reads are listings, starting work is job creation, manifests have their
// own limit, and profile changes are administration.
func datasetOperation(pattern string) string {
	switch {
	case strings.HasPrefix(pattern, "GET "):
		return "listing"
	case pattern == "POST /api/v1/datasets/{id}/manifests", pattern == "POST /api/v1/datasets/{id}/manifests/{jobID}/submit":
		return "manifest"
	case pattern == "POST /api/v1/datasets", pattern == "POST /api/v1/datasets/{id}/snapshots",
		pattern == "POST /api/v1/datasets/{id}/validations", pattern == "POST /api/v1/datasets/{id}/readiness-assessments":
		return "job_create"
	case pattern == "POST /api/v1/validation-profiles":
		return "admin"
	}
	return "upload_finish"
}

// storageOperation names the rate limit a storage connection route counts
// against: reads are listings; tests and credential rotations, which
// reach customer endpoints, have their own tight limit.
func storageOperation(pattern string) string {
	switch {
	case strings.HasPrefix(pattern, "GET "):
		return "listing"
	case strings.HasSuffix(pattern, "/test"), strings.HasSuffix(pattern, "/rotate-credentials"), strings.HasSuffix(pattern, "/enable"):
		return "storage_test"
	}
	return "storage_admin"
}

func (rt Routes) requestsPerMinute(ctx context.Context, tenantID string) (int, error) {
	q, err := rt.Quotas.Cached(ctx, tenantID)
	return q.MaxRequestsPerMinute, err
}
