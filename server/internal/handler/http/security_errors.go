package http

import (
	"context"
	"errors"
	"log/slog"
	nethttp "net/http"
	"strconv"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/service/events"
	"github.com/sanjeev0303/object-storage-platform/internal/service/identity"
	"github.com/sanjeev0303/object-storage-platform/internal/service/processing"
)

// problem is the error body of the security endpoints: a message for
// people and a stable code for programs.
type problem struct {
	Error string `json:"error"`
	Code  string `json:"code"`
	// Quota refusals say which limit and what is left. They describe only
	// the caller's own organization.
	Limit     string `json:"limit,omitempty"`
	Max       int64  `json:"max,omitempty"`
	Used      int64  `json:"used,omitempty"`
	Requested int64  `json:"requested,omitempty"`
	Remaining *int64 `json:"remaining,omitempty"`
}

// securityStatus maps an error to its status and code; ok is false for
// errors with no client meaning (a 500).
func securityStatus(err error) (int, string, bool) {
	switch {
	case errors.Is(err, model.ErrPlacementRefused):
		return nethttp.StatusConflict, "placement_refused", true
	case errors.Is(err, model.ErrVaultRequirements):
		return nethttp.StatusConflict, "vault_requirements", true
	case errors.Is(err, model.ErrEscrowRequired):
		return nethttp.StatusConflict, "escrow_required", true
	case errors.Is(err, model.ErrInvalidCredentials):
		return nethttp.StatusUnauthorized, "invalid_credentials", true
	case errors.Is(err, processing.ErrInvalidContentPolicy):
		return nethttp.StatusBadRequest, "invalid_content_policy", true
	case errors.Is(err, processing.ErrChecksNotConfigured):
		return nethttp.StatusNotImplemented, "content_checks_not_configured", true
	case errors.Is(err, model.ErrRefreshReplayed):
		return nethttp.StatusUnauthorized, "refresh_reused", true
	case errors.Is(err, model.ErrSessionInvalid), errors.Is(err, model.ErrUnauthenticated):
		return nethttp.StatusUnauthorized, "unauthenticated", true
	case errors.Is(err, identity.ErrRegistrationClosed):
		return nethttp.StatusForbidden, "registration_closed", true
	case errors.Is(err, model.ErrSSOFailed):
		return nethttp.StatusUnauthorized, "sso_failed", true
	case errors.Is(err, model.ErrSSONotConfigured):
		return nethttp.StatusNotFound, "sso_not_configured", true
	case errors.Is(err, identity.ErrSSOOff):
		return nethttp.StatusNotImplemented, "sso_disabled", true
	case errors.Is(err, model.ErrAccountInactive):
		return nethttp.StatusForbidden, "account_inactive", true
	case errors.Is(err, model.ErrEscalation):
		return nethttp.StatusForbidden, "escalation", true
	case errors.Is(err, model.ErrForbidden):
		return nethttp.StatusForbidden, "forbidden", true
	case errors.Is(err, model.ErrOrgInactive):
		return nethttp.StatusForbidden, "organization_inactive", true
	case errors.Is(err, model.ErrTenantRequired):
		return nethttp.StatusBadRequest, "organization_required", true
	case errors.Is(err, model.ErrRateLimited):
		return nethttp.StatusTooManyRequests, "rate_limited", true
	case errors.Is(err, model.ErrCredentialToken):
		return nethttp.StatusBadRequest, "invalid_token", true
	case errors.Is(err, model.ErrWeakPassword), errors.Is(err, identity.ErrInvalidInput), errors.Is(err, events.ErrReasonRequired):
		return nethttp.StatusBadRequest, "invalid_request", true
	case errors.Is(err, model.ErrLastOwner):
		return nethttp.StatusConflict, "last_owner", true
	case errors.Is(err, model.ErrPathExists):
		return nethttp.StatusConflict, "path_exists", true
	case errors.Is(err, model.ErrPathBusy):
		return nethttp.StatusConflict, "path_busy", true
	case errors.Is(err, model.ErrRevisionConflict):
		return nethttp.StatusPreconditionFailed, "revision_conflict", true
	case errors.Is(err, model.ErrConnectionNotFound):
		return nethttp.StatusNotFound, "not_found", true
	case errors.Is(err, model.ErrConnectionNotActive):
		return nethttp.StatusConflict, "connection_not_active", true
	case errors.Is(err, model.ErrConnectionInUse):
		return nethttp.StatusConflict, "connection_in_use", true
	case errors.Is(err, model.ErrConnectionExists):
		return nethttp.StatusConflict, "conflict", true
	case errors.Is(err, model.ErrEndpointRejected):
		return nethttp.StatusBadRequest, "endpoint_rejected", true
	case errors.Is(err, model.ErrInvalidConnection):
		return nethttp.StatusBadRequest, "invalid_request", true
	case errors.Is(err, model.ErrUnsupportedProvider):
		return nethttp.StatusUnprocessableEntity, "unsupported", true
	case errors.Is(err, model.ErrCredentialMissing):
		return nethttp.StatusConflict, "credentials_missing", true
	case errors.Is(err, model.ErrStorageNotConfigured):
		return nethttp.StatusNotImplemented, "byos_disabled", true
	case errors.Is(err, model.ErrQueryTimeout):
		return nethttp.StatusUnprocessableEntity, "query_too_broad", true
	case errors.Is(err, model.ErrObjectNotFound), errors.Is(err, model.ErrVersionNotFound):
		return nethttp.StatusNotFound, "not_found", true
	case errors.Is(err, model.ErrEmailTaken), errors.Is(err, model.ErrMembershipExists), errors.Is(err, model.ErrOrgExists),
		errors.Is(err, model.ErrRoleExists), errors.Is(err, model.ErrRoleInUse), errors.Is(err, model.ErrOrgState),
		errors.Is(err, model.ErrOrgNotEmpty), errors.Is(err, identity.ErrBootstrapDone), errors.Is(err, model.ErrNotReplayable),
		errors.Is(err, model.ErrInvalidAssetState), errors.Is(err, model.ErrDeadLetterClosed):
		return nethttp.StatusConflict, "conflict", true
	case errors.Is(err, model.ErrUserNotFound), errors.Is(err, model.ErrOrgNotFound), errors.Is(err, model.ErrMembershipNotFound),
		errors.Is(err, model.ErrInvitationNotFound), errors.Is(err, model.ErrRoleNotFound), errors.Is(err, model.ErrDeadLetterNotFound),
		errors.Is(err, model.ErrAssetNotFound), errors.Is(err, model.ErrUploadNotFound):
		return nethttp.StatusNotFound, "not_found", true
	}
	return nethttp.StatusInternalServerError, "internal", false
}

// writeSecurityError answers err. Quota refusals carry their limit; rate
// limits carry Retry-After; anything unexpected is logged, sanitized, and
// answered with a fixed message.
func writeSecurityError(w nethttp.ResponseWriter, log *slog.Logger, err error) {
	if errors.Is(err, context.Canceled) {
		return // the client is gone
	}
	var quotaErr *model.QuotaError
	if errors.As(err, &quotaErr) {
		writeQuotaError(w, quotaErr)
		return
	}
	status, code, ok := securityStatus(err)
	if !ok {
		log.Error("request failed", "error", retry.Sanitize(err))
		writeJSON(w, status, problem{Error: "the request could not be completed", Code: code})
		return
	}
	var rateErr *model.RateLimitError
	if errors.As(err, &rateErr) {
		seconds := int(rateErr.RetryAfter.Round(time.Second) / time.Second)
		w.Header().Set("Retry-After", strconv.Itoa(max(seconds, 1)))
	}
	if status == nethttp.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="object-storage"`)
	}
	writeJSON(w, status, problem{Error: err.Error(), Code: code})
}

// writeQuotaError answers a refused reservation: 413 for an object larger
// than the tenant allows, 429 for limits that free up by themselves
// (uploads in flight, processing backlog), 403 for the rest.
func writeQuotaError(w nethttp.ResponseWriter, e *model.QuotaError) {
	status := nethttp.StatusForbidden
	switch e.Limit {
	case "max_object_bytes":
		status = nethttp.StatusRequestEntityTooLarge
	case "active_uploads", "active_multipart", "pending_processing":
		status = nethttp.StatusTooManyRequests
		w.Header().Set("Retry-After", "5")
	}
	remaining := e.Remaining()
	writeJSON(w, status, problem{
		Error: e.Error(), Code: "quota_exceeded", Limit: e.Limit, Max: e.Max, Used: e.Used,
		Requested: e.Requested, Remaining: &remaining,
	})
}

// storageConnectionError tells whether err is about a storage connection,
// for handlers that map their own errors first.
func storageConnectionError(err error) bool {
	return errors.Is(err, model.ErrConnectionNotFound) || errors.Is(err, model.ErrConnectionNotActive) ||
		errors.Is(err, model.ErrUnsupportedProvider) || errors.Is(err, model.ErrStorageNotConfigured) ||
		errors.Is(err, model.ErrCredentialMissing) || errors.Is(err, model.ErrPlacementRefused)
}
