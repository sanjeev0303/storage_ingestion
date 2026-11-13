package integration

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// The retention and hold engine over HTTP: templates and dry runs,
// COMPLIANCE guardrails, a query hold that covers objects created after
// it, its proof, the two-person rule, bulk deletes and signed disposition
// certificates.
func TestHTTPGovernance(t *testing.T) {
	s := openSecurityStack(t, nil)
	acme := s.team(t, "gov-a")
	owner := s.signIn(t, acme.ownerUser.Email, acme.ownerPW).AccessToken
	admin := s.signIn(t, acme.adminUser.Email, acme.adminPW).AccessToken
	first := s.availableAsset(t, acme.developer, "ledger-1.csv", []byte("trade,qty\nA,1\n"))

	var templates struct {
		Templates []struct {
			ID       string `json:"id"`
			LockMode string `json:"lock_mode"`
		} `json:"templates"`
	}
	s.expect(t, owner, "", http.MethodGet, "/api/v1/retention/templates", "", http.StatusOK, &templates)
	found := false
	for _, tp := range templates.Templates {
		found = found || (tp.ID == "sec-17a-4" && tp.LockMode == model.LockCompliance)
	}
	if !found {
		t.Fatalf("templates: %+v", templates)
	}

	// A dry run says what a COMPLIANCE policy would cover; making it needs
	// the confirmation.
	var dry struct {
		Impact struct {
			Assets   int64  `json:"assets"`
			LockMode string `json:"lock_mode"`
		} `json:"impact"`
	}
	policy := `{"scope":"TENANT","template":"sec-17a-4","reason":"broker-dealer records"%s}`
	s.expect(t, owner, "", http.MethodPut, "/api/v1/retention/policies", fmt.Sprintf(policy, `,"dry_run":true`), http.StatusOK, &dry)
	if dry.Impact.Assets < 1 || dry.Impact.LockMode != model.LockCompliance {
		t.Fatalf("dry run: %+v", dry)
	}
	s.expect(t, owner, "", http.MethodPut, "/api/v1/retention/policies", fmt.Sprintf(policy, ""), http.StatusBadRequest, nil)
	s.expect(t, owner, "", http.MethodPut, "/api/v1/retention/policies", fmt.Sprintf(policy, `,"confirm_compliance":true`), http.StatusOK, nil)
	// A COMPLIANCE policy only gets stricter.
	s.expect(t, owner, "", http.MethodPut, "/api/v1/retention/policies",
		`{"scope":"TENANT","min_retention":"24h","reason":"too long"}`, http.StatusConflict, nil)

	// The two-person rule.
	var g map[string]any
	s.expect(t, owner, "", http.MethodPut, "/api/v1/governance", `{"profile":"STANDARD","two_person":true}`, http.StatusOK, &g)
	if g["two_person"] != true {
		t.Fatalf("governance: %v", g)
	}

	// A query hold covers assets created after it.
	since := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	var hold struct {
		ID string `json:"id"`
	}
	s.expect(t, owner, "", http.MethodPost, "/api/v1/legal-holds",
		fmt.Sprintf(`{"scope":"QUERY","matter":"case-7","reason":"litigation","custodians":["alice"],"query":{"created_after":%q}}`, since),
		http.StatusCreated, &hold)
	later := s.availableAsset(t, acme.developer, "ledger-2.csv", []byte("trade,qty\nB,2\n"))
	var proof struct {
		Assets []struct {
			AssetID string `json:"asset_id"`
		} `json:"assets"`
	}
	s.expect(t, owner, "", http.MethodGet, "/api/v1/legal-holds/"+hold.ID+"/proof", "", http.StatusOK, &proof)
	covers := map[string]bool{}
	for _, a := range proof.Assets {
		covers[a.AssetID] = true
	}
	if !covers[later.ID] || !covers[first.ID] {
		t.Fatalf("proof covers %v; want %s and %s", covers, first.ID, later.ID)
	}

	// Releasing it waits for a second member; the requester cannot approve.
	var pending struct {
		Status     string `json:"status"`
		ApprovalID string `json:"approval_id"`
	}
	s.expect(t, owner, "", http.MethodDelete, "/api/v1/legal-holds/"+hold.ID+"?reason=settled", "", http.StatusAccepted, &pending)
	if pending.Status != "PENDING_APPROVAL" || pending.ApprovalID == "" {
		t.Fatalf("release: %+v", pending)
	}
	s.expect(t, owner, "", http.MethodPost, "/api/v1/approvals/"+pending.ApprovalID+"/approve", "", http.StatusForbidden, nil)
	var decided model.Approval
	s.expect(t, admin, "", http.MethodPost, "/api/v1/approvals/"+pending.ApprovalID+"/approve", `{"note":"checked with counsel"}`, http.StatusOK, &decided)
	if decided.State != model.ApprovalExecuted || decided.Kind != model.ApprovalHoldRelease {
		t.Fatalf("approval: %+v", decided)
	}
	s.expect(t, admin, "", http.MethodPost, "/api/v1/approvals/"+pending.ApprovalID+"/approve", "", http.StatusConflict, nil)
	var holds struct {
		Holds []struct {
			ID string `json:"id"`
		} `json:"holds"`
	}
	s.expect(t, owner, "", http.MethodGet, "/api/v1/legal-holds", "", http.StatusOK, &holds)
	for _, h := range holds.Holds {
		if h.ID == hold.ID {
			t.Fatal("the approved release did not happen")
		}
	}

	// Bulk delete: the dry run names what holds the assets back; the real
	// one waits for approval.
	var plan struct {
		Matched    int            `json:"matched"`
		Blocked    map[string]int `json:"blocked"`
		ApprovalID string         `json:"approval_id"`
	}
	bulk := fmt.Sprintf(`{"asset_ids":[%q,%q],"reason":"cleanup"%%s}`, first.ID, later.ID)
	s.expect(t, owner, "", http.MethodPost, "/api/v1/assets/bulk-delete", fmt.Sprintf(bulk, `,"dry_run":true`), http.StatusOK, &plan)
	if plan.Matched != 2 || plan.Blocked["retention"] != 2 {
		t.Fatalf("dry run: %+v", plan)
	}
	s.expect(t, owner, "", http.MethodPost, "/api/v1/assets/bulk-delete", fmt.Sprintf(bulk, ""), http.StatusAccepted, &plan)
	if plan.ApprovalID == "" {
		t.Fatalf("bulk delete was not sent for approval: %+v", plan)
	}
	// Selecting by path prefix, without ids, finds the same assets.
	var byPrefix struct {
		Matched int `json:"matched"`
	}
	s.expect(t, owner, "", http.MethodPost, "/api/v1/assets/bulk-delete",
		`{"path_prefix":"ledger-","reason":"cleanup","dry_run":true}`, http.StatusOK, &byPrefix)
	if byPrefix.Matched != 2 {
		t.Fatalf("by path prefix: %+v", byPrefix)
	}
	s.expect(t, admin, "", http.MethodPost, "/api/v1/approvals/"+plan.ApprovalID+"/reject", `{"note":"no"}`, http.StatusOK, &decided)
	if decided.State != model.ApprovalRejected {
		t.Fatalf("reject: %+v", decided)
	}
	var list struct {
		Approvals []model.Approval `json:"approvals"`
	}
	s.expect(t, owner, "", http.MethodGet, "/api/v1/approvals", "", http.StatusOK, &list)
	if len(list.Approvals) != 2 {
		t.Fatalf("approvals: %d", len(list.Approvals))
	}
}

// A permanent deletion leaves a certificate signed with the platform's key.
func TestHTTPDispositionCertificate(t *testing.T) {
	s := openSecurityStack(t, nil)
	team := s.team(t, "cert-a")
	dev := s.signIn(t, team.devUser.Email, team.devPW).AccessToken
	asset := s.availableAsset(t, team.developer, "old.txt", []byte("to be destroyed"))

	var deleted struct {
		Deletion struct {
			ID string `json:"id"`
		} `json:"deletion"`
	}
	s.expect(t, dev, "", http.MethodDelete, "/api/v1/assets/"+asset.ID, "", http.StatusAccepted, &deleted)
	s.expect(t, dev, "", http.MethodGet, "/api/v1/deletions/"+deleted.Deletion.ID+"/certificate", "", http.StatusNotFound, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for i := 0; i < 5; i++ {
		if _, err := s.lifecycle.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var cert model.DispositionCertificate
	s.expect(t, dev, "", http.MethodGet, "/api/v1/deletions/"+deleted.Deletion.ID+"/certificate", "", http.StatusOK, &cert)
	var key struct {
		KeyID     string `json:"key_id"`
		PublicKey string `json:"public_key"`
	}
	s.expect(t, dev, "", http.MethodGet, "/api/v1/certificates/public-key", "", http.StatusOK, &key)

	signed := []byte(cert.SignedBody)
	sum := sha256.Sum256(signed)
	pub, _ := base64.StdEncoding.DecodeString(key.PublicKey)
	sig, _ := base64.StdEncoding.DecodeString(cert.Signature)
	if hex.EncodeToString(sum[:]) != cert.BodySHA256 || cert.KeyID != key.KeyID || !ed25519.Verify(pub, signed, sig) {
		t.Fatalf("certificate does not verify: %+v", cert)
	}
	if cert.AssetID != asset.ID || !containsAll(string(cert.Body), asset.ChecksumSHA256, deleted.Deletion.ID) {
		t.Fatalf("certificate body: %s", cert.Body)
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

// Legal holds are listed in pages, newest first, with released ones only
// on request.
func TestHTTPLegalHoldPages(t *testing.T) {
	s := openSecurityStack(t, nil)
	acme := s.team(t, "gov-pages")
	owner := s.signIn(t, acme.ownerUser.Email, acme.ownerPW).AccessToken

	var placed []string
	for _, key := range []string{"set-a", "set-b", "set-c"} {
		var hold struct {
			ID string `json:"id"`
		}
		s.expect(t, owner, "", http.MethodPost, "/api/v1/legal-holds",
			fmt.Sprintf(`{"scope":"DATASET","scope_key":%q,"reason":"litigation"}`, key), http.StatusCreated, &hold)
		placed = append(placed, hold.ID)
	}
	s.expect(t, owner, "", http.MethodDelete, "/api/v1/legal-holds/"+placed[1]+"?reason=settled", "", http.StatusNoContent, nil)

	type page struct {
		Holds []struct {
			ID string `json:"id"`
		} `json:"holds"`
		Next string `json:"next_cursor"`
	}
	walk := func(path string) []string {
		var ids []string
		cursor := ""
		for range 5 {
			var p page
			s.expect(t, owner, "", http.MethodGet, path+"&cursor="+cursor, "", http.StatusOK, &p)
			for _, h := range p.Holds {
				ids = append(ids, h.ID)
			}
			if p.Next == "" {
				return ids
			}
			cursor = p.Next
		}
		t.Fatalf("%s never ended", path)
		return nil
	}
	if got := walk("/api/v1/legal-holds?limit=1"); !slices.Equal(got, []string{placed[2], placed[0]}) {
		t.Fatalf("active holds: %v, placed %v", got, placed)
	}
	if got := walk("/api/v1/legal-holds?include_released=true&limit=2"); !slices.Equal(got, []string{placed[2], placed[1], placed[0]}) {
		t.Fatalf("every hold: %v, placed %v", got, placed)
	}

	for _, path := range []string{"/api/v1/legal-holds?cursor=garbage", "/api/v1/legal-holds?limit=0", "/api/v1/legal-holds?limit=abc"} {
		if r := s.call(t, owner, "", http.MethodGet, path, ""); r.status != http.StatusBadRequest {
			t.Fatalf("%s: %s", path, r)
		}
	}
}
