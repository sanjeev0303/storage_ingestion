package integration

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/beevik/etree"
	"github.com/crewjam/saml"
)

const samlACS = "http://localhost:3100/api/session/saml/acs"

// testIdP is a SAML identity provider with its own key, answering for one
// service provider.
type testIdP struct {
	idp *saml.IdentityProvider
	sp  *saml.EntityDescriptor
}

func (p *testIdP) GetServiceProvider(*http.Request, string) (*saml.EntityDescriptor, error) {
	return p.sp, nil
}

func newTestIdP(t *testing.T, spEntityID string) *testIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "idp.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	acs, _ := url.Parse(samlACS)
	sp := saml.ServiceProvider{EntityID: spEntityID, AcsURL: *acs}
	p := &testIdP{sp: sp.Metadata()}
	p.idp = &saml.IdentityProvider{
		Key: key, Certificate: cert,
		MetadataURL: url.URL{Scheme: "https", Host: "idp.test", Path: "/metadata"},
		SSOURL:      url.URL{Scheme: "https", Host: "idp.test", Path: "/sso"},
		ServiceProviderProvider: p,
	}
	return p
}

func (p *testIdP) metadata(t *testing.T) string {
	t.Helper()
	raw, err := xml.Marshal(p.idp.Metadata())
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// answer signs a response to the AuthnRequest in the redirect URL, for
// email, and returns it base64-encoded with the RelayState.
func (p *testIdP) answer(t *testing.T, redirect, email string) (string, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, redirect, nil)
	if err != nil {
		t.Fatal(err)
	}
	authn, err := saml.NewIdpAuthnRequest(p.idp, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := authn.Validate(); err != nil {
		t.Fatal(err)
	}
	session := &saml.Session{ID: "s1", CreateTime: time.Now(), ExpireTime: time.Now().Add(time.Hour), Index: "1",
		NameID: email, NameIDFormat: string(saml.EmailAddressNameIDFormat), UserEmail: email, UserCommonName: "Saml Person"}
	if err := (saml.DefaultAssertionMaker{}).MakeAssertion(authn, session); err != nil {
		t.Fatal(err)
	}
	if err := authn.MakeResponse(); err != nil {
		t.Fatal(err)
	}
	doc := etree.NewDocument()
	doc.SetRoot(authn.ResponseEl)
	raw, err := doc.WriteToBytes()
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw), req.URL.Query().Get("RelayState")
}

func TestHTTPSAMLSignIn(t *testing.T) {
	s := openSecurityStack(t, map[string]string{"AUTH_SAML_ACS_URL": samlACS, "RATE_LIMIT_SSO": "1000/1m"})
	acme := s.team(t, "saml-a")
	adminTok := s.signIn(t, acme.adminUser.Email, acme.adminPW).AccessToken
	domain := s.run + ".example.test"

	var settings struct {
		SAML *struct {
			EntityID string `json:"entity_id"`
			ACSURL   string `json:"acs_url"`
		} `json:"saml"`
		Protocol string `json:"protocol"`
	}
	s.expect(t, adminTok, "", http.MethodGet, "/api/v1/organization/sso", "", http.StatusOK, &settings)
	if settings.SAML == nil || settings.SAML.ACSURL != samlACS || !strings.Contains(settings.SAML.EntityID, acme.org.ID) {
		t.Fatalf("SAML details to register: %+v", settings.SAML)
	}
	idp := newTestIdP(t, settings.SAML.EntityID)
	if r := s.call(t, adminTok, "", http.MethodPut, "/api/v1/organization/sso",
		`{"protocol":"SAML","idp_metadata":"<not xml","allowed_domains":["a.test"],"enabled":true}`); r.status != http.StatusBadRequest {
		t.Fatalf("broken metadata: %s", r)
	}
	body, _ := json.Marshal(map[string]any{"protocol": "SAML", "idp_metadata": idp.metadata(t), "allowed_domains": []string{domain},
		"default_role": "developer", "enabled": true})
	s.expect(t, adminTok, "", http.MethodPut, "/api/v1/organization/sso", string(body), http.StatusOK, &settings)
	if settings.Protocol != "SAML" {
		t.Fatalf("saved protocol %q", settings.Protocol)
	}
	// A later save without the metadata keeps the stored one.
	s.expect(t, adminTok, "", http.MethodPut, "/api/v1/organization/sso",
		fmt.Sprintf(`{"protocol":"SAML","allowed_domains":[%q],"default_role":"developer","enabled":true}`, domain), http.StatusOK, &settings)

	start := func() string {
		t.Helper()
		var out struct {
			URL string `json:"authorization_url"`
		}
		s.expect(t, "", "", http.MethodPost, "/api/v1/auth/sso/start",
			fmt.Sprintf(`{"organization":%q,"redirect_uri":"ignored-for-saml"}`, acme.org.Slug), http.StatusOK, &out)
		if !strings.HasPrefix(out.URL, "https://idp.test/sso?") {
			t.Fatalf("authorization URL %s", out.URL)
		}
		return out.URL
	}
	acs := func(response, relay string) httpResult {
		b, _ := json.Marshal(map[string]string{"saml_response": response, "relay_state": relay})
		return s.call(t, "", "", http.MethodPost, "/api/v1/auth/saml/acs", string(b))
	}

	// An existing member signs in.
	response, relay := idp.answer(t, start(), acme.devUser.Email)
	r := acs(response, relay)
	var signed struct {
		AccessToken    string `json:"access_token"`
		OrganizationID string `json:"organization_id"`
		User           struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if r.status != http.StatusOK || json.Unmarshal(r.body, &signed) != nil || signed.User.ID != acme.devUser.ID || signed.OrganizationID != acme.org.ID {
		t.Fatalf("SAML sign-in: %s", r)
	}
	s.expect(t, signed.AccessToken, "", http.MethodGet, "/api/v1/auth/me", "", http.StatusOK, nil)
	if r := acs(response, relay); r.status != http.StatusUnauthorized {
		t.Fatalf("a replayed SAML response: %s", r)
	}

	// A response for another sign-in, a forged one, a foreign domain.
	response1, _ := idp.answer(t, start(), acme.devUser.Email)
	_, relay2 := idp.answer(t, start(), acme.devUser.Email)
	if r := acs(response1, relay2); r.status != http.StatusUnauthorized {
		t.Fatalf("a response for another sign-in: %s", r)
	}
	forger := newTestIdP(t, settings.SAML.EntityID)
	forged, relay3 := forger.answer(t, start(), acme.devUser.Email)
	if r := acs(forged, relay3); r.status != http.StatusUnauthorized {
		t.Fatalf("a response signed by another key: %s", r)
	}
	foreign, relay4 := idp.answer(t, start(), "someone@elsewhere.test")
	if r := acs(foreign, relay4); r.status != http.StatusUnauthorized {
		t.Fatalf("a foreign domain: %s", r)
	}

	// A first sign-in makes the account, without a password.
	newcomer := "saml-newcomer@" + domain
	fresh, relay5 := idp.answer(t, start(), newcomer)
	if r := acs(fresh, relay5); r.status != http.StatusOK {
		t.Fatalf("first SAML sign-in: %s", r)
	}
}

func TestHTTPSCIMProvisioning(t *testing.T) {
	s := openSecurityStack(t, map[string]string{"AUTH_SAML_ACS_URL": samlACS})
	acme := s.team(t, "scim-a")
	globex := s.team(t, "scim-b")
	adminTok := s.signIn(t, acme.adminUser.Email, acme.adminPW).AccessToken
	devTok := s.signIn(t, acme.devUser.Email, acme.devPW).AccessToken
	domain := s.run + ".example.test"

	if r := s.call(t, devTok, "", http.MethodPost, "/api/v1/organization/scim-token", ""); r.status != http.StatusForbidden {
		t.Fatalf("a developer issues a SCIM token: %s", r)
	}
	var issued struct {
		Token string `json:"token"`
	}
	s.expect(t, adminTok, "", http.MethodPost, "/api/v1/organization/scim-token", "", http.StatusCreated, &issued)
	if !strings.HasPrefix(issued.Token, "sst_") {
		t.Fatalf("token %q", issued.Token)
	}
	scim := func(method, path, body string, want int) map[string]any {
		t.Helper()
		r := s.call(t, issued.Token, "", method, path, body)
		if r.status != want {
			t.Fatalf("%s %s: %s, want %d", method, path, r, want)
		}
		out := map[string]any{}
		if len(r.body) > 0 {
			_ = json.Unmarshal(r.body, &out)
		}
		return out
	}
	if r := s.call(t, "sst_wrong", "", http.MethodGet, "/scim/v2/Users", ""); r.status != http.StatusUnauthorized {
		t.Fatalf("a wrong SCIM token: %s", r)
	}

	list := scim(http.MethodGet, "/scim/v2/Users", "", http.StatusOK)
	if list["totalResults"].(float64) != 4 {
		t.Fatalf("members listed: %v", list["totalResults"])
	}
	email := "scim-person@" + domain
	created := scim(http.MethodPost, "/scim/v2/Users", fmt.Sprintf(
		`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":%q,"name":{"givenName":"Sam","familyName":"Kim"},"active":true}`, email),
		http.StatusCreated)
	id := created["id"].(string)
	if created["active"] != true || created["userName"] != email {
		t.Fatalf("created: %v", created)
	}
	scim(http.MethodPost, "/scim/v2/Users", fmt.Sprintf(`{"userName":%q}`, email), http.StatusConflict)
	found := scim(http.MethodGet, "/scim/v2/Users?filter="+url.QueryEscape(fmt.Sprintf(`userName eq "%s"`, email)), "", http.StatusOK)
	if found["totalResults"].(float64) != 1 {
		t.Fatalf("filter: %v", found)
	}

	// Deactivation in the directory suspends the membership.
	off := scim(http.MethodPatch, "/scim/v2/Users/"+id,
		`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"Replace","path":"active","value":"False"}]}`, http.StatusOK)
	if off["active"] != false {
		t.Fatalf("after deactivation: %v", off)
	}
	on := scim(http.MethodPatch, "/scim/v2/Users/"+id, `{"Operations":[{"op":"replace","value":{"active":true}}]}`, http.StatusOK)
	if on["active"] != true {
		t.Fatalf("after reactivation: %v", on)
	}

	// Groups are roles.
	scim(http.MethodPatch, "/scim/v2/Groups/auditor", fmt.Sprintf(`{"Operations":[{"op":"add","path":"members","value":[{"value":%q}]}]}`, id), http.StatusOK)
	if got := scim(http.MethodGet, "/scim/v2/Users/"+id, "", http.StatusOK); fmt.Sprint(got["roles"]) != "[map[value:auditor]]" {
		t.Fatalf("role after joining the auditor group: %v", got["roles"])
	}
	scim(http.MethodPatch, "/scim/v2/Groups/auditor", fmt.Sprintf(`{"Operations":[{"op":"remove","path":"members[value eq \"%s\"]"}]}`, id), http.StatusOK)
	if got := scim(http.MethodGet, "/scim/v2/Users/"+id, "", http.StatusOK); fmt.Sprint(got["roles"]) != "[map[value:developer]]" {
		t.Fatalf("role after leaving the auditor group: %v", got["roles"])
	}

	// Owners are out of SCIM's reach.
	ownerID := acme.ownerUser.ID
	scim(http.MethodPatch, "/scim/v2/Users/"+ownerID, `{"Operations":[{"op":"replace","path":"active","value":false}]}`, http.StatusForbidden)
	scim(http.MethodDelete, "/scim/v2/Users/"+ownerID, "", http.StatusForbidden)
	scim(http.MethodPatch, "/scim/v2/Groups/owner", fmt.Sprintf(`{"Operations":[{"op":"add","value":[{"value":%q}]}]}`, id), http.StatusForbidden)

	// Another organization's people are not there.
	scim(http.MethodGet, "/scim/v2/Users/"+globex.devUser.ID, "", http.StatusNotFound)

	scim(http.MethodDelete, "/scim/v2/Users/"+id, "", http.StatusNoContent)
	scim(http.MethodGet, "/scim/v2/Users/"+id, "", http.StatusNotFound)

	s.expect(t, adminTok, "", http.MethodDelete, "/api/v1/organization/scim-token", "", http.StatusNoContent, nil)
	if r := s.call(t, issued.Token, "", http.MethodGet, "/scim/v2/Users", ""); r.status != http.StatusUnauthorized {
		t.Fatalf("a revoked token: %s", r)
	}
}
