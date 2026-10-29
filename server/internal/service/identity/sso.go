package identity

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"golang.org/x/oauth2"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/netguard"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/secretbox"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

// Single sign-on with OpenID Connect, one provider per organization
// (docs/sso.md). The authorization code flow with PKCE, a nonce, and the
// ID token's signature checked against the provider's published keys.

// ErrSSOOff: single sign-on needs the credential keyring and
// AUTH_SSO_REDIRECT_URIS.
var ErrSSOOff = errors.New("single sign-on is not enabled on this server")

type SSOStore interface {
	GetSSO(ctx context.Context, orgID string) (model.SSOConnection, error)
	PutSSO(ctx context.Context, c model.SSOConnection, audit ...model.AuditEvent) error
	DeleteSSO(ctx context.Context, orgID string, audit ...model.AuditEvent) error
	OrganizationBySlug(ctx context.Context, slug string) (model.Organization, error)
	SaveLoginState(ctx context.Context, stateHash []byte, st model.SSOLoginState, now time.Time) error
	TakeLoginState(ctx context.Context, stateHash []byte, now time.Time) (model.SSOLoginState, error)
	LinkedUser(ctx context.Context, issuer, subject string) (string, error)
	Provision(ctx context.Context, p model.SSOProvision, now time.Time, audit ...model.AuditEvent) error
}

type sso struct {
	store  SSOStore
	sealer secretbox.Sealer
	policy netguard.Policy
	client *http.Client
}

const ssoStateTTL = 10 * time.Minute

// EnableSSO turns single sign-on on. Without a sealer (no credential
// keyring) or redirect URIs it stays off.
func (s *Service) EnableSSO(store SSOStore, sealer secretbox.Sealer, policy netguard.Policy) {
	if sealer == nil || (len(s.cfg.SSORedirectURIs) == 0 && s.cfg.SAMLACSURL == "") {
		return
	}
	s.sso = &sso{store: store, sealer: sealer, policy: policy, client: policy.Client()}
}

// SSOSettingsInput is what an administrator sets. An empty ClientSecret on
// an update keeps the stored one.
type SSOSettingsInput struct {
	// Protocol is OIDC (the default) or SAML.
	Protocol string
	// IDPMetadata is a SAML identity provider's metadata XML.
	IDPMetadata    string
	Issuer         string
	ClientID       string
	ClientSecret   string
	AllowedDomains []string
	DefaultRole    string
	Enabled        bool
}

// SSORedirectURIs are the callback addresses to register at the provider.
func (s *Service) SSORedirectURIs() []string { return s.cfg.SSORedirectURIs }

func (s *Service) SSOSettings(ctx context.Context, p model.Principal) (model.SSOConnection, error) {
	if err := s.require(ctx, p, model.PermOrgManage, "sso.read", "organization", p.TenantID); err != nil {
		return model.SSOConnection{}, err
	}
	if s.sso == nil {
		return model.SSOConnection{}, ErrSSOOff
	}
	return s.sso.store.GetSSO(ctx, p.TenantID)
}

// PutSSO saves the organization's provider after checking that its
// discovery document answers for exactly this issuer.
func (s *Service) PutSSO(ctx context.Context, p model.Principal, in SSOSettingsInput) (model.SSOConnection, error) {
	if err := s.require(ctx, p, model.PermOrgManage, "sso.update", "organization", p.TenantID); err != nil {
		return model.SSOConnection{}, err
	}
	if s.sso == nil {
		return model.SSOConnection{}, ErrSSOOff
	}
	invalid := func(reason string) (model.SSOConnection, error) {
		return model.SSOConnection{}, fmt.Errorf("%w: %s", ErrInvalidInput, reason)
	}
	protocol := strings.ToUpper(strings.TrimSpace(in.Protocol))
	if protocol == "" {
		protocol = model.ProtocolOIDC
	}
	switch {
	case protocol != model.ProtocolOIDC && protocol != model.ProtocolSAML:
		return invalid("protocol must be OIDC or SAML")
	case protocol == model.ProtocolSAML && s.cfg.SAMLACSURL == "":
		return model.SSOConnection{}, fmt.Errorf("%w: SAML needs AUTH_SAML_ACS_URL", ErrSSOOff)
	case protocol == model.ProtocolOIDC && len(s.cfg.SSORedirectURIs) == 0:
		return model.SSOConnection{}, fmt.Errorf("%w: OpenID Connect needs AUTH_SSO_REDIRECT_URIS", ErrSSOOff)
	}
	domains := []string{}
	for _, d := range in.AllowedDomains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" || !strings.Contains(d, ".") || strings.ContainsAny(d, "@/ ") || len(d) > 253 {
			return invalid(fmt.Sprintf("%q is not a domain", d))
		}
		if !slices.Contains(domains, d) {
			domains = append(domains, d)
		}
	}
	if len(domains) == 0 || len(domains) > 20 {
		return invalid("allowed_domains needs between 1 and 20 domains")
	}
	if in.DefaultRole != "" {
		if in.DefaultRole == model.RoleOwner {
			return invalid("the owner role cannot be given by sign-in")
		}
		if _, err := s.role(ctx, p.TenantID, in.DefaultRole); err != nil {
			return invalid("default_role is not a role of this organization")
		}
	}
	current, err := s.sso.store.GetSSO(ctx, p.TenantID)
	exists := err == nil
	if err != nil && !errors.Is(err, model.ErrSSONotConfigured) {
		return model.SSOConnection{}, err
	}

	now := s.now()
	c := model.SSOConnection{
		OrgID: p.TenantID, Protocol: protocol, AllowedDomains: domains, DefaultRole: in.DefaultRole,
		Enabled: in.Enabled, UpdatedBy: p.UserID, CreatedAt: now, UpdatedAt: now,
	}
	if exists {
		c.CreatedAt = current.CreatedAt
	}
	if protocol == model.ProtocolSAML {
		// Like the client secret: empty keeps the stored metadata.
		if strings.TrimSpace(in.IDPMetadata) == "" && exists && current.Protocol == model.ProtocolSAML {
			in.IDPMetadata = current.IDPMetadata
		}
		meta, err := parseIDPMetadata(in.IDPMetadata)
		if err != nil {
			return invalid(err.Error())
		}
		c.Issuer, c.IDPMetadata = meta.EntityID, strings.TrimSpace(in.IDPMetadata)
	} else {
		issuer := strings.TrimSpace(in.Issuer)
		if _, err := s.sso.policy.ValidateURLWithPath(issuer); err != nil {
			return invalid("issuer: " + err.Error())
		}
		clientID := strings.TrimSpace(in.ClientID)
		if clientID == "" || len(clientID) > 512 {
			return invalid("client_id is required, at most 512 characters")
		}
		keepSecret := exists && current.Protocol == model.ProtocolOIDC && len(current.SecretCipher) > 0
		if in.ClientSecret == "" && !keepSecret {
			return invalid("client_secret is required")
		}
		doc, err := s.sso.discover(ctx, issuer)
		if err != nil {
			return invalid(err.Error())
		}
		c.Issuer, c.ClientID = doc.Issuer, clientID
		if keepSecret {
			c.SecretKeyVersion, c.SecretNonce, c.SecretCipher = current.SecretKeyVersion, current.SecretNonce, current.SecretCipher
		}
		if in.ClientSecret != "" {
			if len(in.ClientSecret) > 1024 {
				return invalid("client_secret is longer than 1024 characters")
			}
			sealed, err := s.sso.sealer.Seal([]byte(in.ClientSecret), ssoAD(p.TenantID))
			if err != nil {
				return model.SSOConnection{}, err
			}
			c.SecretKeyVersion, c.SecretNonce, c.SecretCipher = sealed.KeyVersion, sealed.Nonce, sealed.Ciphertext
		}
	}
	e := s.event(ctx, p, "sso.update", "organization", p.TenantID, map[string]any{
		"protocol": protocol, "issuer": c.Issuer, "allowed_domains": domains, "default_role": in.DefaultRole,
		"enabled": in.Enabled, "secret_changed": in.ClientSecret != "",
	})
	return c, s.sso.store.PutSSO(ctx, c, e)
}

func (s *Service) DeleteSSO(ctx context.Context, p model.Principal) error {
	if err := s.require(ctx, p, model.PermOrgManage, "sso.delete", "organization", p.TenantID); err != nil {
		return err
	}
	if s.sso == nil {
		return ErrSSOOff
	}
	return s.sso.store.DeleteSSO(ctx, p.TenantID, s.event(ctx, p, "sso.delete", "organization", p.TenantID, nil))
}

// StartSSO begins a sign-in to the organization with this slug and returns
// the provider's address to send the browser to. redirectURI must be one
// of AUTH_SSO_REDIRECT_URIS: the code goes nowhere else.
func (s *Service) StartSSO(ctx context.Context, slug, redirectURI string, client Client) (string, error) {
	if err := s.throttle("sso", client.IP); err != nil {
		return "", err
	}
	if s.sso == nil {
		return "", ErrSSOOff
	}
	org, err := s.sso.store.OrganizationBySlug(ctx, strings.TrimSpace(slug))
	if err != nil || org.Status != model.OrgActive {
		return "", model.ErrSSONotConfigured
	}
	conn, err := s.sso.store.GetSSO(ctx, org.ID)
	if err != nil || !conn.Enabled {
		return "", model.ErrSSONotConfigured
	}
	if conn.Protocol == model.ProtocolSAML {
		return s.startSAML(ctx, conn)
	}
	if !slices.Contains(s.cfg.SSORedirectURIs, redirectURI) {
		return "", fmt.Errorf("%w: redirect_uri is not one this server allows", ErrInvalidInput)
	}
	doc, err := s.sso.discover(ctx, conn.Issuer)
	if err != nil {
		return "", fmt.Errorf("%w: %v", model.ErrSSOFailed, err)
	}
	state, err := randomText()
	if err != nil {
		return "", err
	}
	nonce, err := randomText()
	if err != nil {
		return "", err
	}
	verifier := oauth2.GenerateVerifier()
	now := s.now()
	st := model.SSOLoginState{OrgID: org.ID, Nonce: nonce, Verifier: verifier, RedirectURI: redirectURI, ExpiresAt: now.Add(ssoStateTTL)}
	if err := s.sso.store.SaveLoginState(ctx, hashToken(state), st, now); err != nil {
		return "", err
	}
	cfg := oauthConfig(conn, doc, redirectURI, "")
	return cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("nonce", nonce)), nil
}

// FinishSSO completes a sign-in with the provider's answer and starts a
// session. It also returns the organization signed in to.
func (s *Service) FinishSSO(ctx context.Context, state, code string, client Client) (Tokens, string, error) {
	if err := s.throttle("sso", client.IP); err != nil {
		return Tokens{}, "", err
	}
	if s.sso == nil {
		return Tokens{}, "", ErrSSOOff
	}
	if state == "" || code == "" || len(state) > 512 || len(code) > 4096 {
		return Tokens{}, "", fmt.Errorf("%w: state and code are required", ErrInvalidInput)
	}
	now := s.now()
	st, err := s.sso.store.TakeLoginState(ctx, hashToken(state), now)
	if err != nil {
		return Tokens{}, "", s.ssoRefused(ctx, "", "", err)
	}
	tokens, err := s.finishSSO(ctx, st, code, client, now)
	if err != nil {
		return Tokens{}, "", s.ssoRefused(ctx, st.OrgID, tokens.User.ID, err)
	}
	return tokens, st.OrgID, nil
}

func (s *Service) finishSSO(ctx context.Context, st model.SSOLoginState, code string, client Client, now time.Time) (Tokens, error) {
	refuse := func(reason string) (Tokens, error) { return Tokens{}, fmt.Errorf("%w: %s", model.ErrSSOFailed, reason) }
	conn, err := s.sso.store.GetSSO(ctx, st.OrgID)
	if err != nil || !conn.Enabled || conn.Protocol == model.ProtocolSAML || st.Verifier == samlState {
		return refuse("OpenID Connect single sign-on is not enabled for this organization")
	}
	doc, err := s.sso.discover(ctx, conn.Issuer)
	if err != nil {
		return refuse(err.Error())
	}
	secret, err := s.sso.sealer.Open(secretbox.Sealed{KeyVersion: conn.SecretKeyVersion, Nonce: conn.SecretNonce, Ciphertext: conn.SecretCipher},
		ssoAD(conn.OrgID))
	if err != nil {
		return Tokens{}, fmt.Errorf("open the sso client secret: %w", err)
	}
	cfg := oauthConfig(conn, doc, st.RedirectURI, string(secret))
	exchangeCtx, cancel := context.WithTimeout(context.WithValue(ctx, oauth2.HTTPClient, s.sso.client), 15*time.Second)
	defer cancel()
	token, err := cfg.Exchange(exchangeCtx, code, oauth2.VerifierOption(st.Verifier))
	if err != nil {
		return refuse("the provider did not accept the code")
	}
	rawID, _ := token.Extra("id_token").(string)
	if rawID == "" {
		return refuse("the provider answered without an ID token")
	}
	claims, err := s.sso.verifyIDToken(ctx, doc, conn.ClientID, rawID, st.Nonce, now)
	if err != nil {
		return refuse(err.Error())
	}

	return s.signInFederated(ctx, conn, doc.Issuer, federatedClaims{
		Subject: claims.Subject, Email: claims.Email, EmailVerified: claims.EmailVerified, Name: claims.Name,
	}, client, now)
}

// federatedClaims is who a provider says signed in: the same for OpenID
// Connect and SAML.
type federatedClaims struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
}

// signInFederated turns a provider's verified answer into a session: the
// email must be verified and in an allowed domain; the subject is linked
// to its account on its first sign-in; a first sign-in gets an account
// and a membership when the connection gives a default role.
func (s *Service) signInFederated(ctx context.Context, conn model.SSOConnection, issuer string, claims federatedClaims, client Client, now time.Time) (Tokens, error) {
	refuse := func(reason string) (Tokens, error) { return Tokens{}, fmt.Errorf("%w: %s", model.ErrSSOFailed, reason) }
	email := strings.ToLower(strings.TrimSpace(claims.Email))
	_, domain, _ := strings.Cut(email, "@")
	switch {
	case claims.Subject == "":
		return refuse("the provider did not say who signed in")
	case email == "" || !claims.EmailVerified:
		return refuse("the provider did not give a verified email address")
	case !slices.Contains(conn.AllowedDomains, domain):
		return refuse("the email domain " + domain + " is not allowed to sign in to this organization")
	}

	provision := model.SSOProvision{Issuer: issuer, Subject: claims.Subject}
	userID, err := s.sso.store.LinkedUser(ctx, issuer, claims.Subject)
	if err != nil {
		return Tokens{}, err
	}
	var user model.User
	switch {
	case userID != "":
		if user, err = s.deps.Users.GetUser(ctx, userID); err != nil {
			return Tokens{}, err
		}
	default:
		user, err = s.deps.Users.GetUserByEmail(ctx, email)
		if errors.Is(err, model.ErrUserNotFound) {
			if conn.DefaultRole == "" {
				return refuse("there is no account for " + email + " in this organization")
			}
			name := strings.TrimSpace(claims.Name)
			if len(name) > 200 {
				name = name[:200]
			}
			user = model.User{ID: newID(), Email: email, DisplayName: name, Status: model.UserActive, CreatedAt: now, UpdatedAt: now}
			provision.NewUser = true
		} else if err != nil {
			return Tokens{}, err
		}
	}
	if user.Status != model.UserActive {
		return Tokens{User: user}, model.ErrAccountInactive
	}
	provision.User = user

	_, membership, _, err := s.deps.Tenancy.Access(ctx, conn.OrgID, user.ID)
	switch {
	case err == nil && membership.Status != model.MembershipActive:
		return Tokens{User: user}, fmt.Errorf("%w: your membership in this organization is suspended", model.ErrSSOFailed)
	case errors.Is(err, model.ErrMembershipNotFound):
		if conn.DefaultRole == "" {
			return refuse("you are not a member of this organization")
		}
		provision.Member = &model.Membership{
			OrgID: conn.OrgID, UserID: user.ID, RoleID: conn.DefaultRole, Status: model.MembershipActive,
			CreatedBy: user.ID, CreatedAt: now, UpdatedAt: now,
		}
	case err != nil:
		return Tokens{}, err
	}

	self := model.Principal{UserID: user.ID, TenantID: conn.OrgID}
	events := []model.AuditEvent{}
	if provision.NewUser {
		events = append(events, s.platformEvent(ctx, self, "user.create", "user", user.ID, map[string]any{"via": "sso"}))
	}
	if provision.Member != nil {
		events = append(events, s.event(ctx, self, "member.join", "user", user.ID, map[string]any{"via": "sso", "role": conn.DefaultRole}))
	}
	if err := s.sso.store.Provision(ctx, provision, now, events...); err != nil {
		return Tokens{User: user}, err
	}
	return s.startSession(ctx, user, client)
}

// ssoRefused records a refused sign-in and returns err.
func (s *Service) ssoRefused(ctx context.Context, orgID, userID string, err error) error {
	e := audit.Anonymous(ctx, "auth.sso_login", "user", userID)
	e.TenantID, e.Outcome, e.Reason = orgID, model.AuditFailed, err.Error()
	s.deps.Audit.Record(ctx, e)
	return err
}

func ssoAD(orgID string) []byte { return []byte("sso:" + orgID) }

func randomText() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func oauthConfig(conn model.SSOConnection, doc discovery, redirectURI, secret string) oauth2.Config {
	return oauth2.Config{
		ClientID: conn.ClientID, ClientSecret: secret, RedirectURL: redirectURI,
		Endpoint: oauth2.Endpoint{AuthURL: doc.AuthorizationEndpoint, TokenURL: doc.TokenEndpoint},
		Scopes:   []string{"openid", "email", "profile"},
	}
}

type discovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// discover reads the provider's discovery document through the network
// guard. Its issuer must be the one configured, and its endpoints must be
// addresses the guard allows.
func (o *sso) discover(ctx context.Context, issuer string) (discovery, error) {
	var doc discovery
	if err := o.getJSON(ctx, strings.TrimSuffix(issuer, "/")+"/.well-known/openid-configuration", &doc); err != nil {
		return doc, fmt.Errorf("read the provider's discovery document: %w", err)
	}
	if strings.TrimSuffix(doc.Issuer, "/") != strings.TrimSuffix(issuer, "/") {
		return doc, fmt.Errorf("the discovery document is for issuer %q, not %q", doc.Issuer, issuer)
	}
	for _, endpoint := range []string{doc.AuthorizationEndpoint, doc.TokenEndpoint, doc.JWKSURI} {
		if _, err := o.policy.ValidateURLWithPath(endpoint); err != nil {
			return doc, fmt.Errorf("the provider's endpoint %q is not allowed: %w", endpoint, err)
		}
	}
	return doc, nil
}

func (o *sso) getJSON(ctx context.Context, url string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := o.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

type idClaims struct {
	jwt.Claims
	Nonce           string `json:"nonce"`
	Email           string `json:"email"`
	EmailVerified   any    `json:"email_verified"`
	Name            string `json:"name"`
	AuthorizedParty string `json:"azp"`
}

type verifiedClaims struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
}

// verifyIDToken checks the ID token's signature against the provider's
// keys, then its issuer, audience, times and nonce.
func (o *sso) verifyIDToken(ctx context.Context, doc discovery, clientID, raw, nonce string, now time.Time) (verifiedClaims, error) {
	tok, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256, jose.RS384, jose.RS512, jose.PS256, jose.ES256, jose.ES384, jose.EdDSA})
	if err != nil || len(tok.Headers) != 1 {
		return verifiedClaims{}, errors.New("the ID token is malformed or not signed with an accepted algorithm")
	}
	var keys jose.JSONWebKeySet
	if err := o.getJSON(ctx, doc.JWKSURI, &keys); err != nil {
		return verifiedClaims{}, fmt.Errorf("read the provider's keys: %w", err)
	}
	candidates := keys.Keys
	if kid := tok.Headers[0].KeyID; kid != "" {
		candidates = keys.Key(kid)
	}
	var claims idClaims
	verified := false
	for _, k := range candidates {
		if k.Use == "enc" {
			continue
		}
		if tok.Claims(k.Key, &claims) == nil {
			verified = true
			break
		}
	}
	if !verified {
		return verifiedClaims{}, errors.New("the ID token's signature does not match the provider's keys")
	}
	expected := jwt.Expected{Issuer: doc.Issuer, AnyAudience: jwt.Audience{clientID}, Time: now}
	if err := claims.ValidateWithLeeway(expected, time.Minute); err != nil {
		return verifiedClaims{}, fmt.Errorf("the ID token is not valid here: %v", err)
	}
	if claims.Expiry == nil {
		return verifiedClaims{}, errors.New("the ID token has no expiry")
	}
	if len(claims.Audience) > 1 && claims.AuthorizedParty != clientID {
		return verifiedClaims{}, errors.New("the ID token was issued to another client")
	}
	if claims.Nonce != nonce {
		return verifiedClaims{}, errors.New("the ID token's nonce does not match this sign-in")
	}
	if claims.Subject == "" {
		return verifiedClaims{}, errors.New("the ID token has no subject")
	}
	// Some providers send email_verified as a string.
	emailVerified := claims.EmailVerified == true || claims.EmailVerified == "true"
	return verifiedClaims{Subject: claims.Subject, Email: claims.Email, EmailVerified: emailVerified, Name: claims.Name}, nil
}
