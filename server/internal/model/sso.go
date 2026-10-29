package model

import (
	"errors"
	"time"
)

// Single sign-on with OpenID Connect (docs/sso.md).

var (
	ErrSSONotConfigured = errors.New("single sign-on is not set up for this organization")
	// ErrSSOFailed: the sign-in was refused; the wrapped text says why.
	ErrSSOFailed = errors.New("single sign-on failed")
)

// SSO protocols.
const (
	ProtocolOIDC = "OIDC"
	ProtocolSAML = "SAML"
)

// SSOConnection is an organization's identity provider.
type SSOConnection struct {
	OrgID string
	// Protocol is ProtocolOIDC or ProtocolSAML.
	Protocol string
	// Issuer is the OpenID issuer, or the SAML identity provider's entity ID.
	Issuer   string
	ClientID string
	// IDPMetadata is a SAML identity provider's metadata document.
	IDPMetadata string
	// The sealed client secret.
	SecretKeyVersion int
	SecretNonce      []byte
	SecretCipher     []byte
	AllowedDomains   []string
	// DefaultRole is given to people signing in for the first time; empty
	// lets only existing members in.
	DefaultRole string
	Enabled     bool
	UpdatedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// SSOLoginState is a sign-in between its start and the provider's answer.
type SSOLoginState struct {
	OrgID       string
	Nonce       string
	Verifier    string
	RedirectURI string
	ExpiresAt   time.Time
}

// SSOProvision is what a first sign-in writes, in one transaction: maybe a
// new account, the link from the provider's subject to the account, and
// maybe a membership.
type SSOProvision struct {
	Issuer  string
	Subject string
	User    User
	NewUser bool
	Member  *Membership
}
