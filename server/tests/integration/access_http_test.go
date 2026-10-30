package integration

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/request"
	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// API tokens, vendor workspaces, the link and egress policy, classification
// and read-anomaly alerts, over HTTP.
func TestHTTPAccessGovernance(t *testing.T) {
	s := openSecurityStack(t, nil)
	acme := s.team(t, "access-a")
	owner := s.signIn(t, acme.ownerUser.Email, acme.ownerPW).AccessToken
	dev := s.signIn(t, acme.devUser.Email, acme.devPW).AccessToken

	inDataset := func(name string) func(*request.CreateUploadRequest) {
		return func(r *request.CreateUploadRequest) { r.Dataset = name }
	}
	scan := s.publishAt(t, acme.developer, "scans/a.txt", textData("scan", 40), inDataset("scans"))
	other := s.publishAt(t, acme.developer, "other/b.txt", textData("other", 40), inDataset("other"))

	// A token cannot hold more than its member, and is shown once.
	s.expect(t, dev, "", http.MethodPost, "/api/v1/api-tokens", `{"name":"x","permissions":["org.manage"]}`, http.StatusForbidden, nil)
	s.expect(t, dev, "", http.MethodPost, "/api/v1/api-tokens", `{"name":"x","permissions":["assets.read"],"expires_in":"10m"}`, http.StatusBadRequest, nil)
	var vendor struct {
		ID    string `json:"id"`
		Token string `json:"token"`
		Hint  string `json:"hint"`
	}
	s.expect(t, dev, "", http.MethodPost, "/api/v1/api-tokens",
		`{"name":"vendor","permissions":["assets.read","assets.download"],"datasets":["scans"]}`, http.StatusCreated, &vendor)
	if !strings.HasPrefix(vendor.Token, "osp_pat_") || strings.Contains(vendor.Hint, vendor.Token) {
		t.Fatalf("token: %+v", vendor)
	}
	var mine struct {
		Tokens []map[string]any `json:"tokens"`
	}
	s.expect(t, dev, "", http.MethodGet, "/api/v1/api-tokens", "", http.StatusOK, &mine)
	if len(mine.Tokens) != 1 || mine.Tokens[0]["token"] != nil {
		t.Fatalf("listing shows the secret or misses the token: %+v", mine)
	}

	// The vendor token reads its dataset and cannot see anything else.
	s.expect(t, vendor.Token, "", http.MethodGet, "/api/v1/assets/"+scan.ID+"/content", "", http.StatusOK, nil)
	s.expect(t, vendor.Token, "", http.MethodGet, "/api/v1/assets/"+other.ID+"/content", "", http.StatusNotFound, nil)
	// It cannot do what its permissions leave out, nor mint tokens or share links.
	s.expect(t, vendor.Token, "", http.MethodPost, "/api/v1/api-tokens", `{"name":"y","permissions":["assets.read"]}`, http.StatusForbidden, nil)
	s.expect(t, vendor.Token, "", http.MethodPost, "/api/v1/assets/"+scan.ID+"/download-url", `{"mode":"token"}`, http.StatusForbidden, nil)
	s.expect(t, vendor.Token, "", http.MethodGet, "/api/v1/access-policy", "", http.StatusForbidden, nil)

	// A token bound to another network is refused here.
	var bound struct {
		Token string `json:"token"`
	}
	s.expect(t, dev, "", http.MethodPost, "/api/v1/api-tokens",
		`{"name":"office","permissions":["assets.read"],"networks":["10.20.0.0/16"]}`, http.StatusCreated, &bound)
	s.expect(t, bound.Token, "", http.MethodGet, "/api/v1/assets/"+scan.ID, "", http.StatusUnauthorized, nil)

	// Revoked tokens stop at once.
	s.expect(t, dev, "", http.MethodDelete, "/api/v1/api-tokens/"+vendor.ID, "", http.StatusNoContent, nil)
	s.expect(t, vendor.Token, "", http.MethodGet, "/api/v1/assets/"+scan.ID+"/content", "", http.StatusUnauthorized, nil)

	// Share links: a RESTRICTED asset has none, and a policy can turn them off.
	var link response.DownloadURLResponse
	s.expect(t, dev, "", http.MethodPost, "/api/v1/assets/"+scan.ID+"/download-url", `{"mode":"token"}`, http.StatusOK, &link)
	if res := s.call(t, "", "", http.MethodGet, strings.TrimPrefix(link.URL, s.api.URL), ""); res.status != http.StatusOK {
		t.Fatalf("share link: %s", res)
	}
	var links struct {
		Links []model.ShareLink `json:"links"`
	}
	s.expect(t, dev, "", http.MethodGet, "/api/v1/links", "", http.StatusOK, &links)
	if len(links.Links) != 1 || links.Links[0].Uses != 1 || links.Links[0].AssetID != scan.ID {
		t.Fatalf("link inventory: %+v", links)
	}
	s.expect(t, dev, "", http.MethodPut, "/api/v1/assets/"+other.ID+"/classification", `{"classification":"restricted"}`, http.StatusOK, nil)
	s.expect(t, dev, "", http.MethodPut, "/api/v1/assets/"+other.ID+"/classification", `{"classification":"secret"}`, http.StatusBadRequest, nil)
	res := s.expect(t, dev, "", http.MethodPost, "/api/v1/assets/"+other.ID+"/download-url", `{"mode":"token"}`, http.StatusForbidden, nil)
	if !strings.Contains(string(res.body), "link_policy") {
		t.Fatalf("restricted share link: %s", res)
	}
	s.expect(t, dev, "", http.MethodPost, "/api/v1/assets/"+other.ID+"/download-url", "", http.StatusOK, nil)

	s.expect(t, dev, "", http.MethodPut, "/api/v1/access-policy", `{"links_enabled":false}`, http.StatusForbidden, nil)
	s.expect(t, owner, "", http.MethodPut, "/api/v1/access-policy", `{"egress_action":"maybe"}`, http.StatusBadRequest, nil)
	s.expect(t, owner, "", http.MethodPut, "/api/v1/access-policy", `{"links_enabled":false}`, http.StatusOK, nil)
	s.expect(t, dev, "", http.MethodPost, "/api/v1/assets/"+scan.ID+"/download-url", `{"mode":"token"}`, http.StatusForbidden, nil)

	// Mass link creation alerts, and REVOKE_LINKS takes the links back.
	s.expect(t, owner, "", http.MethodPut, "/api/v1/access-policy", `{"links_enabled":true,"mass_link_threshold":10,"anomaly_action":"REVOKE_LINKS"}`, http.StatusOK, nil)
	for i := 0; i < 11; i++ {
		s.expect(t, dev, "", http.MethodPost, "/api/v1/assets/"+scan.ID+"/download-url", `{"mode":"token"}`, http.StatusOK, nil)
	}
	if _, err := s.access.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}
	var alerts struct {
		Alerts []model.SecurityAlert `json:"alerts"`
	}
	s.expect(t, dev, "", http.MethodGet, "/api/v1/security/alerts", "", http.StatusForbidden, nil)
	s.expect(t, owner, "", http.MethodGet, "/api/v1/security/alerts", "", http.StatusOK, &alerts)
	var mass *model.SecurityAlert
	for i, a := range alerts.Alerts {
		if a.Kind == model.AlertMassLinks && a.PrincipalID == acme.devUser.ID {
			mass = &alerts.Alerts[i]
		}
	}
	if mass == nil || !strings.Contains(mass.ActionTaken, "revoked") {
		t.Fatalf("mass links alert: %+v", alerts.Alerts)
	}
	s.expect(t, dev, "", http.MethodGet, "/api/v1/links", "", http.StatusOK, &links)
	if len(links.Links) != 0 {
		t.Fatalf("links survived REVOKE_LINKS: %d", len(links.Links))
	}
	s.expect(t, owner, "", http.MethodPost, "/api/v1/security/alerts/"+mass.ID+"/acknowledge", "", http.StatusOK, nil)
	s.expect(t, owner, "", http.MethodGet, "/api/v1/security/alerts?state=open", "", http.StatusOK, &alerts)
	for _, a := range alerts.Alerts {
		if a.ID == mass.ID {
			t.Fatal("an acknowledged alert is still open")
		}
	}

	// An egress budget that is used up blocks downloads with 429.
	var egress struct {
		MonthBytes int64 `json:"month_bytes"`
	}
	s.expect(t, owner, "", http.MethodGet, "/api/v1/egress", "", http.StatusOK, &egress)
	if egress.MonthBytes < 40 {
		t.Fatalf("egress not counted: %+v", egress)
	}
	s.expect(t, owner, "", http.MethodPut, "/api/v1/access-policy",
		fmt.Sprintf(`{"egress_monthly_bytes":%d,"egress_action":"BLOCK"}`, egress.MonthBytes+1), http.StatusOK, nil)
	res = s.expect(t, dev, "", http.MethodGet, "/api/v1/assets/"+scan.ID+"/content", "", http.StatusTooManyRequests, nil)
	if res.header.Get("Retry-After") == "" || !strings.Contains(string(res.body), "egress_budget_exceeded") {
		t.Fatalf("egress block: %s", res)
	}

	// Another organization sees none of it.
	globex := s.team(t, "access-b")
	stranger := s.signIn(t, globex.ownerUser.Email, globex.ownerPW).AccessToken
	s.expect(t, stranger, "", http.MethodPut, "/api/v1/assets/"+scan.ID+"/classification", `{"classification":"PUBLIC"}`, http.StatusNotFound, nil)
	s.expect(t, stranger, "", http.MethodPost, "/api/v1/security/alerts/"+mass.ID+"/acknowledge", "", http.StatusNotFound, nil)
}
