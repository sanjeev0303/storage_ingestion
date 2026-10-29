package integration

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// fakeIdP is an OpenID provider for tests: discovery, keys, and a token
// endpoint that checks the code and the PKCE verifier and answers with an
// ID token for whoever the test says is signing in.
type fakeIdP struct {
	t      *testing.T
	server *httptest.Server
	key    *rsa.PrivateKey
	// signWith, when set, signs ID tokens with another key.
	signWith *rsa.PrivateKey

	mu        sync.Mutex
	challenge string
	nonce     string
	claims    map[string]any
}

func newFakeIdP(t *testing.T) *fakeIdP {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIdP{t: t, key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer": idp.server.URL, "authorization_endpoint": idp.server.URL + "/authorize",
			"token_endpoint": idp.server.URL + "/token", "jwks_uri": idp.server.URL + "/jwks",
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &idp.key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"},
		}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		idp.mu.Lock()
		defer idp.mu.Unlock()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if r.Form.Get("code") != "good-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != idp.challenge {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": idp.idToken()})
	})
	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)
	return idp
}

// login remembers what the authorization URL carried and who signs in.
func (idp *fakeIdP) login(authURL string, claims map[string]any) (state string) {
	idp.t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		idp.t.Fatal(err)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("client_id") != "console" || q.Get("scope") != "openid email profile" {
		idp.t.Fatalf("authorization URL %s", authURL)
	}
	idp.mu.Lock()
	idp.challenge, idp.nonce, idp.claims = q.Get("code_challenge"), q.Get("nonce"), claims
	idp.mu.Unlock()
	return q.Get("state")
}

func (idp *fakeIdP) idToken() string {
	key := idp.key
	if idp.signWith != nil {
		key = idp.signWith
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		idp.t.Fatal(err)
	}
	now := time.Now()
	claims := map[string]any{
		"iss": idp.server.URL, "aud": "console", "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"nonce": idp.nonce, "email_verified": true,
	}
	for k, v := range idp.claims {
		claims[k] = v
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		idp.t.Fatal(err)
	}
	return raw
}

const ssoCallback = "http://localhost:3100/api/session/sso/callback"

func TestHTTPSingleSignOn(t *testing.T) {
	s := openSecurityStack(t, map[string]string{"AUTH_SSO_REDIRECT_URIS": ssoCallback, "RATE_LIMIT_SSO": "1000/1m"})
	idp := newFakeIdP(t)
	acme := s.team(t, "sso-a")
	globex := s.team(t, "sso-b")
	adminTok := s.signIn(t, acme.adminUser.Email, acme.adminPW).AccessToken
	devTok := s.signIn(t, acme.devUser.Email, acme.devPW).AccessToken
	globexTok := s.signIn(t, globex.adminUser.Email, globex.adminPW).AccessToken
	domain := s.run + ".example.test"

	settings := func(defaultRole string) string {
		return fmt.Sprintf(`{"issuer":%q,"client_id":"console","client_secret":"shh","allowed_domains":[%q],"default_role":%q,"enabled":true}`,
			idp.server.URL, domain, defaultRole)
	}
	if r := s.call(t, devTok, "", http.MethodPut, "/api/v1/organization/sso", settings("")); r.status != http.StatusForbidden {
		t.Fatalf("a developer sets up SSO: %s", r)
	}
	if r := s.call(t, adminTok, "", http.MethodPut, "/api/v1/organization/sso",
		`{"issuer":"http://169.254.169.254","client_id":"c","client_secret":"x","allowed_domains":["a.test"],"enabled":true}`); r.status != http.StatusBadRequest {
		t.Fatalf("a metadata-address issuer: %s", r)
	}
	var got struct {
		Configured   bool     `json:"configured"`
		Issuer       string   `json:"issuer"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	s.expect(t, adminTok, "", http.MethodPut, "/api/v1/organization/sso", settings(""), http.StatusOK, &got)
	if !got.Configured || got.Issuer != idp.server.URL || len(got.RedirectURIs) != 1 {
		t.Fatalf("saved settings: %+v", got)
	}
	read := s.expect(t, adminTok, "", http.MethodGet, "/api/v1/organization/sso", "", http.StatusOK, nil)
	if strings.Contains(string(read.body), "shh") {
		t.Fatalf("the client secret is shown: %s", read.body)
	}
	var theirs struct {
		Configured bool `json:"configured"`
	}
	s.expect(t, globexTok, "", http.MethodGet, "/api/v1/organization/sso", "", http.StatusOK, &theirs)
	if theirs.Configured {
		t.Fatal("another organization sees acme's SSO settings")
	}

	start := func(slug string) string {
		t.Helper()
		var out struct {
			URL string `json:"authorization_url"`
		}
		s.expect(t, "", "", http.MethodPost, "/api/v1/auth/sso/start",
			fmt.Sprintf(`{"organization":%q,"redirect_uri":%q}`, slug, ssoCallback), http.StatusOK, &out)
		return out.URL
	}
	type signedIn struct {
		AccessToken    string `json:"access_token"`
		OrganizationID string `json:"organization_id"`
		User           struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"user"`
	}
	finish := func(state string, want int) signedIn {
		t.Helper()
		var out signedIn
		r := s.expect(t, "", "", http.MethodPost, "/api/v1/auth/sso/callback", fmt.Sprintf(`{"state":%q,"code":"good-code"}`, state), want, nil)
		if want == http.StatusOK {
			if err := json.Unmarshal(r.body, &out); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}

	if r := s.call(t, "", "", http.MethodPost, "/api/v1/auth/sso/start",
		fmt.Sprintf(`{"organization":%q,"redirect_uri":"https://evil.example/cb"}`, acme.org.Slug)); r.status != http.StatusBadRequest {
		t.Fatalf("an unlisted redirect URI: %s", r)
	}
	if r := s.call(t, "", "", http.MethodPost, "/api/v1/auth/sso/start",
		fmt.Sprintf(`{"organization":%q,"redirect_uri":%q}`, globex.org.Slug, ssoCallback)); r.status != http.StatusNotFound {
		t.Fatalf("an organization without SSO: %s", r)
	}

	// An existing member signs in; the session works; the state is spent.
	state := idp.login(start(acme.org.Slug), map[string]any{"sub": "dev-1", "email": acme.devUser.Email})
	dev := finish(state, http.StatusOK)
	if dev.User.ID != acme.devUser.ID || dev.OrganizationID != acme.org.ID {
		t.Fatalf("signed in as %+v", dev)
	}
	s.expect(t, dev.AccessToken, "", http.MethodGet, "/api/v1/auth/me", "", http.StatusOK, nil)
	finish(state, http.StatusUnauthorized)

	// Refusals: no account and no default role, an unverified email, a
	// foreign domain, a wrong nonce, a forged signature.
	newcomer := "newcomer@" + domain
	finish(idp.login(start(acme.org.Slug), map[string]any{"sub": "new-1", "email": newcomer}), http.StatusUnauthorized)
	finish(idp.login(start(acme.org.Slug), map[string]any{"sub": "x-1", "email": acme.adminUser.Email, "email_verified": false}), http.StatusUnauthorized)
	finish(idp.login(start(acme.org.Slug), map[string]any{"sub": "x-2", "email": "someone@elsewhere.test"}), http.StatusUnauthorized)
	finish(idp.login(start(acme.org.Slug), map[string]any{"sub": "x-3", "email": acme.adminUser.Email, "nonce": "replayed"}), http.StatusUnauthorized)
	idp.signWith, _ = rsa.GenerateKey(rand.Reader, 2048)
	finish(idp.login(start(acme.org.Slug), map[string]any{"sub": "x-4", "email": acme.adminUser.Email}), http.StatusUnauthorized)
	idp.signWith = nil

	// With a default role, a first sign-in creates the account and the
	// membership. The account has no password.
	s.expect(t, adminTok, "", http.MethodPut, "/api/v1/organization/sso", `{"issuer":`+fmt.Sprintf("%q", idp.server.URL)+
		`,"client_id":"console","allowed_domains":[`+fmt.Sprintf("%q", domain)+`],"default_role":"developer","enabled":true}`, http.StatusOK, nil)
	fresh := finish(idp.login(start(acme.org.Slug), map[string]any{"sub": "new-1", "email": newcomer, "name": "New Comer"}), http.StatusOK)
	if fresh.User.Email != newcomer {
		t.Fatalf("provisioned %+v", fresh)
	}
	var me struct {
		Memberships []struct {
			OrganizationID string `json:"organization_id"`
			RoleID         string `json:"role_id"`
		} `json:"memberships"`
	}
	s.expect(t, fresh.AccessToken, "", http.MethodGet, "/api/v1/auth/me", "", http.StatusOK, &me)
	if len(me.Memberships) != 1 || me.Memberships[0].OrganizationID != acme.org.ID || me.Memberships[0].RoleID != "developer" {
		t.Fatalf("memberships of the new account: %+v", me)
	}
	var account struct {
		User struct {
			HasPassword bool `json:"has_password"`
		} `json:"user"`
	}
	s.expect(t, fresh.AccessToken, "", http.MethodGet, "/api/v1/auth/me", "", http.StatusOK, &account)
	if account.User.HasPassword {
		t.Fatal("an SSO-only account says it has a password")
	}
	if r := s.call(t, "", "", http.MethodPost, "/api/v1/auth/login", fmt.Sprintf(`{"email":%q,"password":"anything at all here"}`, newcomer)); r.status != http.StatusUnauthorized {
		t.Fatalf("password sign-in to an SSO-only account: %s", r)
	}

	// The subject stays linked to its account even when the provider
	// changes the email; another subject cannot take a linked account.
	again := finish(idp.login(start(acme.org.Slug), map[string]any{"sub": "new-1", "email": "renamed@" + domain}), http.StatusOK)
	if again.User.ID != fresh.User.ID {
		t.Fatalf("the linked subject signed in as %s, want %s", again.User.ID, fresh.User.ID)
	}
	finish(idp.login(start(acme.org.Slug), map[string]any{"sub": "intruder", "email": newcomer}), http.StatusUnauthorized)

	s.expect(t, adminTok, "", http.MethodDelete, "/api/v1/organization/sso", "", http.StatusNoContent, nil)
	if r := s.call(t, "", "", http.MethodPost, "/api/v1/auth/sso/start",
		fmt.Sprintf(`{"organization":%q,"redirect_uri":%q}`, acme.org.Slug, ssoCallback)); r.status != http.StatusNotFound {
		t.Fatalf("after removing SSO: %s", r)
	}
}
