package http

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	nethttp "net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/correlation"
	"github.com/sanjeev0303/object-storage-platform/internal/service/identity"
)

// SCIMHandler serves SCIM 2.0 (RFC 7643/7644) for identity providers:
// /scim/v2/Users and /scim/v2/Groups of the organization the bearer
// token belongs to (docs/sso.md, "SCIM").
type SCIMHandler struct {
	identity *identity.Service
	log      *slog.Logger
}

func NewSCIMHandler(identity *identity.Service, log *slog.Logger) *SCIMHandler {
	return &SCIMHandler{identity: identity, log: log}
}

const (
	scimUserSchema  = "urn:ietf:params:scim:schemas:core:2.0:User"
	scimGroupSchema = "urn:ietf:params:scim:schemas:core:2.0:Group"
	scimListSchema  = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	scimErrorSchema = "urn:ietf:params:scim:api:messages:2.0:Error"
	scimPatchSchema = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
)

func (h *SCIMHandler) Routes() map[string]nethttp.HandlerFunc {
	return map[string]nethttp.HandlerFunc{
		"GET /scim/v2/ServiceProviderConfig": h.config,
		"GET /scim/v2/Users":                 h.authed(h.listUsers),
		"POST /scim/v2/Users":                h.authed(h.createUser),
		"GET /scim/v2/Users/{id}":            h.authed(h.getUser),
		"PUT /scim/v2/Users/{id}":            h.authed(h.replaceUser),
		"PATCH /scim/v2/Users/{id}":          h.authed(h.patchUser),
		"DELETE /scim/v2/Users/{id}":         h.authed(h.deleteUser),
		"GET /scim/v2/Groups":                h.authed(h.listGroups),
		"GET /scim/v2/Groups/{id}":           h.authed(h.getGroup),
		"PATCH /scim/v2/Groups/{id}":         h.authed(h.patchGroup),
	}
}

func writeSCIM(w nethttp.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/scim+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (h *SCIMHandler) fail(w nethttp.ResponseWriter, err error) {
	status, scimType := nethttp.StatusInternalServerError, ""
	switch {
	case errors.Is(err, model.ErrUnauthenticated), errors.Is(err, model.ErrOrgInactive):
		status = nethttp.StatusUnauthorized
	case errors.Is(err, identity.ErrSSOOff):
		status = nethttp.StatusNotImplemented
	case errors.Is(err, identity.ErrSCIMNotFound):
		status = nethttp.StatusNotFound
	case errors.Is(err, identity.ErrSCIMConflict):
		status, scimType = nethttp.StatusConflict, "uniqueness"
	case errors.Is(err, identity.ErrSCIMOwner), errors.Is(err, model.ErrForbidden), errors.Is(err, model.ErrEscalation),
		errors.Is(err, model.ErrLastOwner):
		status, scimType = nethttp.StatusForbidden, "mutability"
	case errors.Is(err, identity.ErrInvalidInput):
		status, scimType = nethttp.StatusBadRequest, "invalidValue"
	case errors.Is(err, model.ErrRateLimited):
		status = nethttp.StatusTooManyRequests
	}
	detail := err.Error()
	if status == nethttp.StatusInternalServerError {
		h.log.Error("scim request failed", "error", err)
		detail = "the request could not be completed"
	}
	body := map[string]any{"schemas": []string{scimErrorSchema}, "status": strconv.Itoa(status), "detail": detail}
	if scimType != "" {
		body["scimType"] = scimType
	}
	writeSCIM(w, status, body)
}

// authed checks the bearer token and runs next as the organization's SCIM
// principal.
func (h *SCIMHandler) authed(next func(nethttp.ResponseWriter, *nethttp.Request, model.Principal)) nethttp.HandlerFunc {
	return func(w nethttp.ResponseWriter, r *nethttp.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="scim"`)
			h.fail(w, model.ErrUnauthenticated)
			return
		}
		p, err := h.identity.SCIMAuthenticate(r.Context(), strings.TrimSpace(token), identity.Client{IP: correlation.Source(r.Context())})
		if err != nil {
			h.fail(w, err)
			return
		}
		next(w, r, p)
	}
}

func (h *SCIMHandler) config(w nethttp.ResponseWriter, r *nethttp.Request) {
	writeSCIM(w, nethttp.StatusOK, map[string]any{
		"schemas":          []string{"urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"},
		"patch":            map[string]bool{"supported": true},
		"bulk":             map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"filter":           map[string]any{"supported": true, "maxResults": 200},
		"changePassword":   map[string]bool{"supported": false},
		"sort":             map[string]bool{"supported": false},
		"etag":             map[string]bool{"supported": false},
		"authenticationSchemes": []map[string]any{{
			"type": "oauthbearertoken", "name": "Bearer token", "primary": true,
			"description": "The organization's SCIM token, from Settings > Single sign-on",
		}},
	})
}

func scimUser(u identity.SCIMUser) map[string]any {
	name := u.User.DisplayName
	return map[string]any{
		"schemas": []string{scimUserSchema}, "id": u.User.ID, "userName": u.User.Email,
		"displayName": name, "name": map[string]string{"formatted": name},
		"emails": []map[string]any{{"value": u.User.Email, "primary": true, "type": "work"}},
		"active": u.Member.Status == model.MembershipActive && u.User.Status == model.UserActive,
		"roles":  []map[string]string{{"value": u.Member.RoleID}},
		"meta": map[string]any{"resourceType": "User", "created": u.Member.CreatedAt, "lastModified": u.Member.UpdatedAt,
			"location": "/scim/v2/Users/" + u.User.ID},
	}
}

// The one filter identity providers send: userName eq "someone@example.com".
var userNameFilter = regexp.MustCompile(`(?i)^\s*userName\s+eq\s+"([^"]*)"\s*$`)

func (h *SCIMHandler) listUsers(w nethttp.ResponseWriter, r *nethttp.Request, p model.Principal) {
	q := r.URL.Query()
	userName := ""
	if f := q.Get("filter"); f != "" {
		m := userNameFilter.FindStringSubmatch(f)
		if m == nil {
			writeSCIM(w, nethttp.StatusBadRequest, map[string]any{"schemas": []string{scimErrorSchema}, "status": "400",
				"scimType": "invalidFilter", "detail": `only userName eq "..." is supported`})
			return
		}
		userName = m[1]
	}
	start, _ := strconv.Atoi(q.Get("startIndex"))
	count := 100
	if c := q.Get("count"); c != "" {
		count, _ = strconv.Atoi(c)
	}
	users, total, err := h.identity.SCIMUsers(r.Context(), p, userName, start, count)
	if err != nil {
		h.fail(w, err)
		return
	}
	resources := make([]map[string]any, 0, len(users))
	for _, u := range users {
		resources = append(resources, scimUser(u))
	}
	writeSCIM(w, nethttp.StatusOK, map[string]any{"schemas": []string{scimListSchema}, "totalResults": total,
		"startIndex": max(start, 1), "itemsPerPage": len(resources), "Resources": resources})
}

type scimUserBody struct {
	UserName    string `json:"userName"`
	DisplayName string `json:"displayName"`
	Name        struct {
		Formatted  string `json:"formatted"`
		GivenName  string `json:"givenName"`
		FamilyName string `json:"familyName"`
	} `json:"name"`
	Emails []struct {
		Value   string `json:"value"`
		Primary bool   `json:"primary"`
	} `json:"emails"`
	Active *bool `json:"active"`
}

func readSCIM(w nethttp.ResponseWriter, r *nethttp.Request, out any) bool {
	data, err := io.ReadAll(nethttp.MaxBytesReader(w, r.Body, 1<<20))
	if err == nil {
		err = json.Unmarshal(data, out)
	}
	if err != nil {
		writeSCIM(w, nethttp.StatusBadRequest, map[string]any{"schemas": []string{scimErrorSchema}, "status": "400",
			"scimType": "invalidSyntax", "detail": "the body is not a SCIM JSON document"})
		return false
	}
	return true
}

func (h *SCIMHandler) createUser(w nethttp.ResponseWriter, r *nethttp.Request, p model.Principal) {
	var body scimUserBody
	if !readSCIM(w, r, &body) {
		return
	}
	email := body.UserName
	for _, e := range body.Emails {
		if e.Primary || !strings.Contains(email, "@") {
			email = e.Value
		}
	}
	name := body.DisplayName
	if name == "" {
		name = body.Name.Formatted
	}
	if name == "" {
		name = strings.TrimSpace(body.Name.GivenName + " " + body.Name.FamilyName)
	}
	u, err := h.identity.SCIMCreateUser(r.Context(), p, email, name, body.Active == nil || *body.Active)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeSCIM(w, nethttp.StatusCreated, scimUser(u))
}

func (h *SCIMHandler) getUser(w nethttp.ResponseWriter, r *nethttp.Request, p model.Principal) {
	u, err := h.identity.SCIMUser(r.Context(), p, r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeSCIM(w, nethttp.StatusOK, scimUser(u))
}

// replaceUser honours active; the account's name and email belong to the
// person, not to the directory.
func (h *SCIMHandler) replaceUser(w nethttp.ResponseWriter, r *nethttp.Request, p model.Principal) {
	var body scimUserBody
	if !readSCIM(w, r, &body) {
		return
	}
	h.setActive(w, r, p, body.Active)
}

func (h *SCIMHandler) setActive(w nethttp.ResponseWriter, r *nethttp.Request, p model.Principal, active *bool) {
	var u identity.SCIMUser
	var err error
	if active == nil {
		u, err = h.identity.SCIMUser(r.Context(), p, r.PathValue("id"))
	} else {
		u, err = h.identity.SCIMSetActive(r.Context(), p, r.PathValue("id"), *active)
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	writeSCIM(w, nethttp.StatusOK, scimUser(u))
}

type scimPatch struct {
	Operations []struct {
		Op    string          `json:"op"`
		Path  string          `json:"path"`
		Value json.RawMessage `json:"value"`
	} `json:"Operations"`
}

// scimBool reads true/false sent as a boolean or, as Entra ID does, a string.
func scimBool(raw json.RawMessage) (bool, bool) {
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b, true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && (strings.EqualFold(s, "true") || strings.EqualFold(s, "false")) {
		return strings.EqualFold(s, "true"), true
	}
	return false, false
}

// patchUser applies "active" in either form identity providers use:
// {"op":"replace","path":"active","value":false} or
// {"op":"replace","value":{"active":false}}. Other paths are ignored.
func (h *SCIMHandler) patchUser(w nethttp.ResponseWriter, r *nethttp.Request, p model.Principal) {
	var body scimPatch
	if !readSCIM(w, r, &body) {
		return
	}
	var active *bool
	for _, op := range body.Operations {
		if !strings.EqualFold(op.Op, "replace") && !strings.EqualFold(op.Op, "add") {
			continue
		}
		if strings.EqualFold(op.Path, "active") {
			if v, ok := scimBool(op.Value); ok {
				active = &v
			}
			continue
		}
		if op.Path == "" {
			var fields map[string]json.RawMessage
			if json.Unmarshal(op.Value, &fields) == nil {
				for k, raw := range fields {
					if strings.EqualFold(k, "active") {
						if v, ok := scimBool(raw); ok {
							active = &v
						}
					}
				}
			}
		}
	}
	h.setActive(w, r, p, active)
}

func (h *SCIMHandler) deleteUser(w nethttp.ResponseWriter, r *nethttp.Request, p model.Principal) {
	if err := h.identity.SCIMDeleteUser(r.Context(), p, r.PathValue("id")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

func scimGroup(g identity.SCIMGroup) map[string]any {
	members := make([]map[string]string, 0, len(g.Members))
	for _, m := range g.Members {
		members = append(members, map[string]string{"value": m.UserID, "display": m.Email, "$ref": "/scim/v2/Users/" + m.UserID})
	}
	return map[string]any{"schemas": []string{scimGroupSchema}, "id": g.Role.ID, "displayName": g.Role.Name,
		"members": members, "meta": map[string]any{"resourceType": "Group", "location": "/scim/v2/Groups/" + g.Role.ID}}
}

func (h *SCIMHandler) listGroups(w nethttp.ResponseWriter, r *nethttp.Request, p model.Principal) {
	groups, err := h.identity.SCIMGroups(r.Context(), p)
	if err != nil {
		h.fail(w, err)
		return
	}
	resources := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		resources = append(resources, scimGroup(g))
	}
	writeSCIM(w, nethttp.StatusOK, map[string]any{"schemas": []string{scimListSchema}, "totalResults": len(resources),
		"startIndex": 1, "itemsPerPage": len(resources), "Resources": resources})
}

func (h *SCIMHandler) getGroup(w nethttp.ResponseWriter, r *nethttp.Request, p model.Principal) {
	g, err := h.identity.SCIMGroup(r.Context(), p, r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeSCIM(w, nethttp.StatusOK, scimGroup(g))
}

// memberPath reads a removal's target in the path form identity providers
// use: members[value eq "<user id>"].
var memberPath = regexp.MustCompile(`(?i)^members\[value eq "([^"]+)"\]$`)

func (h *SCIMHandler) patchGroup(w nethttp.ResponseWriter, r *nethttp.Request, p model.Principal) {
	var body scimPatch
	if !readSCIM(w, r, &body) {
		return
	}
	var add, remove []string
	for _, op := range body.Operations {
		var values []struct {
			Value string `json:"value"`
		}
		_ = json.Unmarshal(op.Value, &values)
		ids := make([]string, 0, len(values))
		for _, v := range values {
			ids = append(ids, v.Value)
		}
		if m := memberPath.FindStringSubmatch(op.Path); m != nil {
			ids = append(ids, m[1])
		}
		switch strings.ToLower(op.Op) {
		case "add":
			add = append(add, ids...)
		case "remove":
			remove = append(remove, ids...)
		}
	}
	g, err := h.identity.SCIMPatchGroup(r.Context(), p, r.PathValue("id"), add, remove)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeSCIM(w, nethttp.StatusOK, scimGroup(g))
}
