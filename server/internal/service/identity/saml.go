package identity

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/crewjam/saml"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// SAML 2.0 single sign-on (docs/sso.md, "SAML"). The SP-initiated flow:
// an unsigned AuthnRequest by the HTTP-Redirect binding, the response
// posted back to the console's ACS route. Responses are checked by
// crewjam/saml: the XML signature against the identity provider's
// certificate from its metadata, the audience, the destination, the times
// and InResponseTo against the request this server made, so a response
// can be used once, for the sign-in it answers. Identity-provider-initiated
// sign-ins are refused.

// samlState marks a sign-in state as SAML's.
const samlState = "saml"

// maxSAMLResponse bounds a posted response, base64 included.
const maxSAMLResponse = 1 << 20

// SAMLServiceProvider is what an administrator registers at the identity
// provider for this organization.
type SAMLServiceProvider struct {
	EntityID string
	ACSURL   string
}

// SAMLProvider describes this server's side of SAML for an organization,
// or false when SAML is off.
func (s *Service) SAMLProvider(orgID string) (SAMLServiceProvider, bool) {
	if s.sso == nil || s.cfg.SAMLACSURL == "" {
		return SAMLServiceProvider{}, false
	}
	return SAMLServiceProvider{EntityID: spEntityID(orgID), ACSURL: s.cfg.SAMLACSURL}, true
}

func spEntityID(orgID string) string { return "urn:object-storage:sp:" + orgID }

// parseIDPMetadata reads an identity provider's metadata and checks that it
// can be used: an entity ID, a single sign-on address and a signing
// certificate.
func parseIDPMetadata(raw string) (*saml.EntityDescriptor, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("idp_metadata is required: the identity provider's SAML metadata XML")
	}
	if len(raw) > 256*1024 {
		return nil, errors.New("idp_metadata is larger than 256 KiB")
	}
	var meta saml.EntityDescriptor
	if err := xml.Unmarshal([]byte(raw), &meta); err != nil {
		return nil, fmt.Errorf("idp_metadata is not SAML metadata: %v", err)
	}
	if meta.EntityID == "" || len(meta.IDPSSODescriptors) == 0 {
		return nil, errors.New("idp_metadata names no identity provider (EntityDescriptor with an IDPSSODescriptor)")
	}
	idp := meta.IDPSSODescriptors[0]
	redirect := false
	for _, svc := range idp.SingleSignOnServices {
		if svc.Binding == saml.HTTPRedirectBinding && svc.Location != "" {
			redirect = true
		}
	}
	if !redirect {
		return nil, errors.New("the identity provider offers no single sign-on address with the HTTP-Redirect binding")
	}
	signing := false
	for _, key := range idp.KeyDescriptors {
		if (key.Use == "" || key.Use == "signing") && len(key.KeyInfo.X509Data.X509Certificates) > 0 {
			signing = true
		}
	}
	if !signing {
		return nil, errors.New("the identity provider's metadata has no signing certificate")
	}
	return &meta, nil
}

func (s *Service) serviceProvider(conn model.SSOConnection) (*saml.ServiceProvider, error) {
	meta, err := parseIDPMetadata(conn.IDPMetadata)
	if err != nil {
		return nil, err
	}
	acs, err := url.Parse(s.cfg.SAMLACSURL)
	if err != nil {
		return nil, fmt.Errorf("AUTH_SAML_ACS_URL: %w", err)
	}
	return &saml.ServiceProvider{
		EntityID:          spEntityID(conn.OrgID),
		AcsURL:            *acs,
		IDPMetadata:       meta,
		AuthnNameIDFormat: saml.UnspecifiedNameIDFormat,
		AllowIDPInitiated: false,
	}, nil
}

// startSAML makes an AuthnRequest and returns the identity provider's
// address carrying it.
func (s *Service) startSAML(ctx context.Context, conn model.SSOConnection) (string, error) {
	sp, err := s.serviceProvider(conn)
	if err != nil {
		return "", fmt.Errorf("%w: %v", model.ErrSSOFailed, err)
	}
	req, err := sp.MakeAuthenticationRequest(sp.GetSSOBindingLocation(saml.HTTPRedirectBinding), saml.HTTPRedirectBinding, saml.HTTPPostBinding)
	if err != nil {
		return "", fmt.Errorf("make the SAML request: %w", err)
	}
	relay, err := randomText()
	if err != nil {
		return "", err
	}
	now := s.now()
	st := model.SSOLoginState{OrgID: conn.OrgID, Nonce: req.ID, Verifier: samlState, RedirectURI: s.cfg.SAMLACSURL, ExpiresAt: now.Add(ssoStateTTL)}
	if err := s.sso.store.SaveLoginState(ctx, hashToken(relay), st, now); err != nil {
		return "", err
	}
	u, err := req.Redirect(relay, sp)
	if err != nil {
		return "", fmt.Errorf("encode the SAML request: %w", err)
	}
	return u.String(), nil
}

// FinishSAML completes a SAML sign-in with the response the identity
// provider posted (base64) and its RelayState, and starts a session. It
// also returns the organization signed in to.
func (s *Service) FinishSAML(ctx context.Context, samlResponse, relayState string, client Client) (Tokens, string, error) {
	if err := s.throttle("sso", client.IP); err != nil {
		return Tokens{}, "", err
	}
	if s.sso == nil || s.cfg.SAMLACSURL == "" {
		return Tokens{}, "", ErrSSOOff
	}
	if samlResponse == "" || relayState == "" || len(samlResponse) > maxSAMLResponse || len(relayState) > 512 {
		return Tokens{}, "", fmt.Errorf("%w: SAMLResponse and RelayState are required", ErrInvalidInput)
	}
	now := s.now()
	st, err := s.sso.store.TakeLoginState(ctx, hashToken(relayState), now)
	if err != nil {
		return Tokens{}, "", s.ssoRefused(ctx, "", "", err)
	}
	tokens, err := s.finishSAML(ctx, st, samlResponse, client)
	if err != nil {
		return Tokens{}, "", s.ssoRefused(ctx, st.OrgID, tokens.User.ID, err)
	}
	return tokens, st.OrgID, nil
}

func (s *Service) finishSAML(ctx context.Context, st model.SSOLoginState, samlResponse string, client Client) (Tokens, error) {
	refuse := func(reason string) (Tokens, error) { return Tokens{}, fmt.Errorf("%w: %s", model.ErrSSOFailed, reason) }
	if st.Verifier != samlState {
		return refuse("this sign-in was not started with SAML")
	}
	conn, err := s.sso.store.GetSSO(ctx, st.OrgID)
	if err != nil || !conn.Enabled || conn.Protocol != model.ProtocolSAML {
		return refuse("SAML single sign-on is not enabled for this organization")
	}
	sp, err := s.serviceProvider(conn)
	if err != nil {
		return refuse(err.Error())
	}
	decoded, err := base64.StdEncoding.DecodeString(samlResponse)
	if err != nil {
		return refuse("the SAML response is not base64")
	}
	assertion, err := sp.ParseXMLResponse(decoded, []string{st.Nonce}, sp.AcsURL)
	if err != nil {
		// The library keeps the reason private; it is logged, not shown.
		var invalid *saml.InvalidResponseError
		if errors.As(err, &invalid) {
			s.deps.Log.Warn("SAML response refused", "org_id", conn.OrgID, "reason", fmt.Sprint(invalid.PrivateErr))
		}
		return refuse("the identity provider's answer did not pass its checks")
	}
	claims := samlClaims(assertion)
	return s.signInFederated(ctx, conn, sp.IDPMetadata.EntityID, claims, client, s.now())
}

// The attribute names identity providers use for an email address and a
// display name.
var (
	samlEmailNames = []string{"email", "mail", "emailaddress", "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress",
		"urn:oid:0.9.2342.19200300.100.1.3"}
	samlNameNames = []string{"displayname", "name", "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/name",
		"http://schemas.microsoft.com/identity/claims/displayname", "urn:oid:2.16.840.1.113730.3.1.241"}
)

// samlClaims reads who signed in: the NameID as the subject, the email
// from an email attribute or an email-shaped NameID. The identity provider
// vouches for the address by signing the assertion; the allowed domains
// still decide who may sign in.
func samlClaims(a *saml.Assertion) federatedClaims {
	var c federatedClaims
	if a.Subject != nil && a.Subject.NameID != nil {
		c.Subject = a.Subject.NameID.Value
	}
	attr := func(names []string) string {
		for _, stmt := range a.AttributeStatements {
			for _, at := range stmt.Attributes {
				for _, name := range names {
					if (strings.EqualFold(at.Name, name) || strings.EqualFold(at.FriendlyName, name)) && len(at.Values) > 0 {
						return strings.TrimSpace(at.Values[0].Value)
					}
				}
			}
		}
		return ""
	}
	c.Email = attr(samlEmailNames)
	if c.Email == "" && strings.Contains(c.Subject, "@") {
		c.Email = c.Subject
	}
	c.EmailVerified = c.Email != ""
	c.Name = attr(samlNameNames)
	return c
}
