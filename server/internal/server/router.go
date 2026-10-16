package server

import (
	"log/slog"
	"net/http"

	httpapi "github.com/sanjeev0303/object-storage-platform/internal/handler/http"
	"github.com/sanjeev0303/object-storage-platform/internal/server/middleware"
	"github.com/sanjeev0303/object-storage-platform/internal/service/delivery"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

// DeliveryRoutes configures the asset delivery endpoints.
type DeliveryRoutes struct {
	Service *delivery.Service
	// Identity authenticates delivery requests. nil answers every delivery
	// request with 401.
	Identity middleware.Resolver
	// ProxyEnabled adds GET /api/v1/assets/{id}/content.
	ProxyEnabled bool
	Log          *slog.Logger
}

// NewRouter connects HTTP routes to their handlers.
// The services are constructed outside the router and injected here.
func NewRouter(
	uploadService *uploadservice.Service,
	multipartService *uploadservice.MultipartService,
	deliveryRoutes DeliveryRoutes,
) http.Handler {
	mux := http.NewServeMux()

	healthHandler := httpapi.NewHealthHandler()
	mux.Handle("GET /health", healthHandler)

	// Temporary local-development identity.
	// Replace this with authenticated tenant and user context before
	// enabling the endpoint for real clients.
	uploadHandler := httpapi.NewUploadHandler(
		uploadService,
		"local-tenant",
		"local-user",
	)
	mux.HandleFunc("/api/v1/uploads", uploadHandler.Create)

	multipartHandler := httpapi.NewMultipartUploadHandler(multipartService)
	multipartHandler.Register(mux)

	if deliveryRoutes.Service != nil {
		authenticated := func(handler http.HandlerFunc) http.Handler {
			return middleware.Authenticate(deliveryRoutes.Identity, handler)
		}
		assets := httpapi.NewAssetHandler(deliveryRoutes.Service, deliveryRoutes.Log)
		downloads := httpapi.NewDownloadHandler(deliveryRoutes.Service, deliveryRoutes.Log)

		mux.Handle("GET /api/v1/assets/{id}", authenticated(assets.Get))
		mux.Handle("POST /api/v1/assets/{id}/download-url", authenticated(downloads.CreateURL))
		if deliveryRoutes.ProxyEnabled {
			mux.Handle("GET /api/v1/assets/{id}/content", authenticated(downloads.Content))
		}
	}

	return mux
}
