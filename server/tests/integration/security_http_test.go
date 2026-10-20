package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	eventdto "github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/delivery"
)

// End-to-end through the real router: sign-in, organizations and
// permissions as a client sees them.

func TestHTTPSignInFlow(t *testing.T) {
	s := openSecurityStack(t, nil)
	acme := s.team(t, "web")

	// The development identity headers are gone: they authenticate nobody.
	req, _ := http.NewRequest(http.MethodGet, s.api.URL+"/api/v1/organization", nil)
	req.Header.Set("X-Dev-Tenant", acme.org.ID)
	req.Header.Set("X-Dev-User", acme.ownerUser.ID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("development headers: HTTP %d", resp.StatusCode)
	}

	bad := s.call(t, "", "", http.MethodPost, "/api/v1/auth/login", fmt.Sprintf(`{"email":%q,"password":"wrong password!"}`, acme.devUser.Email))
	if bad.status != http.StatusUnauthorized || !strings.Contains(string(bad.body), "invalid_credentials") {
		t.Fatalf("wrong password: %s", bad)
	}

	tokens := s.signIn(t, acme.devUser.Email, acme.devPW)
	var me response.MeResponse
	r := s.expect(t, tokens.AccessToken, "", http.MethodGet, "/api/v1/auth/me", "", http.StatusOK, &me)
	if me.User.ID != acme.devUser.ID || len(me.Memberships) != 1 || me.Memberships[0].RoleID != model.RoleDeveloper {
		t.Fatalf("me: %+v", me)
	}
	if strings.Contains(string(r.body), "argon2") || strings.Contains(string(r.body), "password") {
		t.Fatalf("no password material in responses: %s", r.body)
	}
	// One organization: it applies without naming it.
	var org response.OrganizationResponse
	s.expect(t, tokens.AccessToken, "", http.MethodGet, "/api/v1/organization", "", http.StatusOK, &org)
	if org.ID != acme.org.ID {
		t.Fatalf("organization: %+v", org)
	}

	// Refresh rotates; the spent token's reuse ends the session.
	var next response.TokensResponse
	s.expect(t, "", "", http.MethodPost, "/api/v1/auth/refresh", fmt.Sprintf(`{"refresh_token":%q}`, tokens.RefreshToken), http.StatusOK, &next)
	if r := s.call(t, tokens.AccessToken, "", http.MethodGet, "/api/v1/auth/me", ""); r.status != http.StatusUnauthorized {
		t.Fatalf("old access token after refresh: %s", r)
	}
	s.expect(t, next.AccessToken, "", http.MethodGet, "/api/v1/auth/me", "", http.StatusOK, nil)
	replay := s.call(t, "", "", http.MethodPost, "/api/v1/auth/refresh", fmt.Sprintf(`{"refresh_token":%q}`, tokens.RefreshToken))
	if replay.status != http.StatusUnauthorized || !strings.Contains(string(replay.body), "refresh_reused") {
		t.Fatalf("replayed refresh: %s", replay)
	}
	if r := s.call(t, next.AccessToken, "", http.MethodGet, "/api/v1/auth/me", ""); r.status != http.StatusUnauthorized {
		t.Fatalf("after a replay: %s", r)
	}

	// Logout.
	again := s.signIn(t, acme.devUser.Email, acme.devPW)
	s.expect(t, again.AccessToken, "", http.MethodPost, "/api/v1/auth/logout", "", http.StatusNoContent, nil)
	if r := s.call(t, again.AccessToken, "", http.MethodGet, "/api/v1/auth/me", ""); r.status != http.StatusUnauthorized {
		t.Fatalf("after logout: %s", r)
	}

	// A deactivated account's tokens stop at once.
	third := s.signIn(t, acme.auditorUser.Email, acme.auditorPW)
	if _, err := s.identity.SetUserStatus(context.Background(), operator, acme.auditorUser.ID, false); err != nil {
		t.Fatal(err)
	}
	if r := s.call(t, third.AccessToken, "", http.MethodGet, "/api/v1/organization", ""); r.status != http.StatusUnauthorized {
		t.Fatalf("deactivated account: %s", r)
	}
	inactive := s.call(t, "", "", http.MethodPost, "/api/v1/auth/login", fmt.Sprintf(`{"email":%q,"password":%q}`, acme.auditorUser.Email, acme.auditorPW))
	if inactive.status != http.StatusForbidden || !strings.Contains(string(inactive.body), "account_inactive") {
		t.Fatalf("deactivated sign-in: %s", inactive)
	}
}

func TestHTTPCrossTenantAccessIsHidden(t *testing.T) {
	s := openSecurityStack(t, nil)
	acme := s.team(t, "acme")
	globex := s.team(t, "globex")
	secret := s.availableAsset(t, acme.developer, "secret", textData("secret", 64))
	acmeTok := s.signIn(t, acme.devUser.Email, acme.devPW).AccessToken
	auditorTok := s.signIn(t, acme.auditorUser.Email, acme.auditorPW).AccessToken
	globexTok := s.signIn(t, globex.devUser.Email, globex.devPW).AccessToken

	// Every asset route answers another tenant exactly like a missing
	// asset, and signs nothing.
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/assets/%s"},
		{http.MethodPost, "/api/v1/assets/%s/download-url"},
		{http.MethodGet, "/api/v1/assets/%s/content"},
		{http.MethodDelete, "/api/v1/assets/%s/download-links"},
		{http.MethodGet, "/api/v1/assets/%s/quarantine"},
	} {
		// The route exists and serves its own tenant...
		if mine := s.call(t, acmeTok, "", c.method, fmt.Sprintf(c.path, secret.ID), ""); mine.status != http.StatusOK {
			t.Fatalf("acme %s %s: %s", c.method, c.path, mine)
		}
		// ...and answers another tenant like a missing asset.
		presigned := len(s.ie.storage.Presigns)
		theirs := s.call(t, globexTok, "", c.method, fmt.Sprintf(c.path, secret.ID), "")
		missing := s.call(t, globexTok, "", c.method, fmt.Sprintf(c.path, "no-such-asset"), "")
		if theirs.status != http.StatusNotFound || string(theirs.body) != string(missing.body) || len(s.ie.storage.Presigns) != presigned {
			t.Fatalf("%s %s across tenants: %s, missing: %s", c.method, c.path, theirs, missing)
		}
	}
	if r := s.call(t, globexTok, "", http.MethodGet, "/api/v1/uploads/"+secret.UploadID, ""); r.status != http.StatusNotFound {
		t.Fatalf("another tenant's upload: %s", r)
	}
	// Naming the other organization does not help: membership decides.
	if r := s.call(t, globexTok, acme.org.ID, http.MethodGet, "/api/v1/assets/"+secret.ID, ""); r.status != http.StatusForbidden ||
		!strings.Contains(string(r.body), "not_a_member") {
		t.Fatalf("naming acme: %s", r)
	}
	// Nor does putting a tenant in the body.
	body := `{"file_name":"x.txt","content_type":"text/plain","size_bytes":3,"idempotency_key":"k","tenant_id":"` + acme.org.ID + `"}`
	if r := s.call(t, globexTok, "", http.MethodPost, "/api/v1/uploads", body); r.status != http.StatusBadRequest {
		t.Fatalf("a tenant in the body: %s", r)
	}

	// Datasets, jobs and manifests: hidden from other tenants, read-only
	// for auditors.
	var ds response.DatasetResponse
	s.expect(t, acmeTok, "", http.MethodPost, "/api/v1/datasets", `{"name":"private-set"}`, http.StatusCreated, &ds)
	var job response.IngestionJobResponse
	s.expect(t, acmeTok, "", http.MethodPost, "/api/v1/ingestion/jobs", fmt.Sprintf(`{"dataset_id":%q,"idempotency_key":"j1"}`, ds.ID), http.StatusCreated, &job)
	manifest := manifestOf(textFile("a.txt", 10))
	ndjson := map[string]string{"Content-Type": "application/x-ndjson"}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/datasets/" + ds.ID, ""},
		{http.MethodGet, "/api/v1/ingestion/jobs/" + job.ID, ""},
		{http.MethodGet, "/api/v1/ingestion/jobs/" + job.ID + "/entries", ""},
		{http.MethodPut, "/api/v1/ingestion/jobs/" + job.ID + "/manifest", manifest},
		{http.MethodPost, "/api/v1/ingestion/jobs", fmt.Sprintf(`{"dataset_id":%q,"idempotency_key":"j2"}`, ds.ID)},
	} {
		headers := map[string]string{}
		if strings.HasSuffix(c.path, "/manifest") {
			headers = ndjson
		}
		if r := s.call(t, globexTok, "", c.method, c.path, c.body, headers); r.status != http.StatusNotFound {
			t.Fatalf("globex %s %s: %s", c.method, c.path, r)
		}
	}
	s.expect(t, auditorTok, "", http.MethodGet, "/api/v1/datasets/"+ds.ID, "", http.StatusOK, nil)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/datasets", `{"name":"auditor-set"}`},
		{http.MethodPost, "/api/v1/ingestion/jobs", fmt.Sprintf(`{"dataset_id":%q,"idempotency_key":"j3"}`, ds.ID)},
		{http.MethodPut, "/api/v1/ingestion/jobs/" + job.ID + "/manifest", manifest},
		{http.MethodPost, "/api/v1/uploads", `{"file_name":"x.txt","content_type":"text/plain","size_bytes":3,"idempotency_key":"k"}`},
	} {
		headers := map[string]string{}
		if strings.HasSuffix(c.path, "/manifest") {
			headers = ndjson
		}
		if r := s.call(t, auditorTok, "", c.method, c.path, c.body, headers); r.status != http.StatusForbidden {
			t.Fatalf("auditor %s %s: %s", c.method, c.path, r)
		}
	}
	// The job still takes its manifest from its own organization.
	if r := s.call(t, acmeTok, "", http.MethodPut, "/api/v1/ingestion/jobs/"+job.ID+"/manifest", manifest, ndjson); r.status != http.StatusOK {
		t.Fatalf("acme's own manifest: %s", r)
	}
}

func TestHTTPDownloadAuthorization(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "dl")
	ready := s.availableAsset(t, acme.developer, "ready", textData("ready", 64))
	devTok := s.signIn(t, acme.devUser.Email, acme.devPW).AccessToken
	auditorTok := s.signIn(t, acme.auditorUser.Email, acme.auditorPW).AccessToken
	adminTok := s.signIn(t, acme.adminUser.Email, acme.adminPW).AccessToken

	// Developers get a link; auditors read metadata but get no link.
	var link response.DownloadURLResponse
	s.expect(t, devTok, "", http.MethodPost, "/api/v1/assets/"+ready.ID+"/download-url", "", http.StatusOK, &link)
	if link.URL == "" || link.Delivery != delivery.ModeStorage {
		t.Fatalf("link: %+v", link)
	}
	presigned := len(s.ie.storage.Presigns)
	s.expect(t, auditorTok, "", http.MethodGet, "/api/v1/assets/"+ready.ID, "", http.StatusOK, nil)
	for _, path := range []string{"/download-url", "/content"} {
		method := http.MethodPost
		if path == "/content" {
			method = http.MethodGet
		}
		if r := s.call(t, auditorTok, "", method, "/api/v1/assets/"+ready.ID+path, ""); r.status != http.StatusForbidden {
			t.Fatalf("auditor %s: %s", path, r)
		}
	}
	if len(s.ie.storage.Presigns) != presigned {
		t.Fatal("no URL may be signed for an auditor")
	}

	// Nothing unpublished is delivered: an upload still in processing...
	s.ie.scannerDown.Store(true)
	held := s.upload(t, acme.developer, "held", textData("held", 64))
	if _, err := s.ie.single.CompleteFor(ctx, acme.developer, held.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ie.proc.Intake(ctx, acme.org.ID, held.ID); err != nil {
		t.Fatal(err)
	}
	s.ie.scannerDown.Store(false)
	pending, err := s.ie.assets.GetByUpload(ctx, acme.org.ID, held.ID)
	if err != nil || pending.Status == model.AssetStatusAvailable {
		t.Fatalf("held asset: %+v %v", pending, err)
	}
	// ...or one rejected as malware.
	rejectedUpload := s.upload(t, acme.developer, "infected", infectedFile("infected.txt").data)
	if _, err := s.ie.single.CompleteFor(ctx, acme.developer, rejectedUpload.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ie.proc.Intake(ctx, acme.org.ID, rejectedUpload.ID); err != nil {
		t.Fatal(err)
	}
	rejected, err := s.ie.assets.GetByUpload(ctx, acme.org.ID, rejectedUpload.ID)
	if err != nil || rejected.Status != model.AssetStatusRejected {
		t.Fatalf("infected asset: %+v %v", rejected, err)
	}
	presigned = len(s.ie.storage.Presigns)
	for _, id := range []string{pending.ID, rejected.ID} {
		for _, c := range []struct{ method, path string }{{http.MethodPost, "/download-url"}, {http.MethodGet, "/content"}} {
			if r := s.call(t, devTok, "", c.method, "/api/v1/assets/"+id+c.path, ""); r.status != http.StatusConflict {
				t.Fatalf("unpublished asset %s: %s", c.path, r)
			}
		}
	}
	if len(s.ie.storage.Presigns) != presigned {
		t.Fatal("no URL may be signed for an unpublished asset")
	}

	// Quarantine records: developers read them; releasing needs
	// quarantine.release, which administrators have.
	s.expect(t, devTok, "", http.MethodGet, "/api/v1/assets/"+rejected.ID+"/quarantine", "", http.StatusOK, nil)
	if r := s.call(t, devTok, "", http.MethodPost, "/api/v1/assets/"+pending.ID+"/quarantine/release", ""); r.status != http.StatusForbidden {
		t.Fatalf("developer releases: %s", r)
	}
	if r := s.call(t, adminTok, "", http.MethodPost, "/api/v1/assets/"+rejected.ID+"/quarantine/release", ""); r.status != http.StatusConflict {
		t.Fatalf("a rejected asset is not released: %s", r)
	}

	// A token link carries its issuer's current authority: once the
	// issuer leaves, it stops working.
	var tokenLink response.DownloadURLResponse
	s.expect(t, devTok, "", http.MethodPost, "/api/v1/assets/"+ready.ID+"/download-url", `{"mode":"token"}`, http.StatusOK, &tokenLink)
	path := tokenLink.URL[strings.Index(tokenLink.URL, delivery.TokenPathPrefix):]
	if r := s.call(t, "", "", http.MethodGet, path, ""); r.status != http.StatusOK || string(r.body) != string(textData("ready", 64)) {
		t.Fatalf("token link: %s", r)
	}
	if err := s.identity.RemoveMember(ctx, acme.admin, acme.devUser.ID); err != nil {
		t.Fatal(err)
	}
	if r := s.call(t, "", "", http.MethodGet, path, ""); r.status != http.StatusForbidden {
		t.Fatalf("a removed member's link: %s", r)
	}
}

func TestHTTPRateLimits(t *testing.T) {
	s := openSecurityStack(t, map[string]string{"RATE_LIMIT_LISTING": "3/1m", "AUTH_LOGIN_ATTEMPTS": "2"})
	acme := s.team(t, "rate")
	tok := s.signIn(t, acme.devUser.Email, acme.devPW).AccessToken

	for i := 0; i < 3; i++ {
		s.expect(t, tok, "", http.MethodGet, "/api/v1/ingestion/jobs", "", http.StatusOK, nil)
	}
	r := s.call(t, tok, "", http.MethodGet, "/api/v1/ingestion/jobs", "")
	retry, err := strconv.Atoi(r.header.Get("Retry-After"))
	if r.status != http.StatusTooManyRequests || err != nil || retry < 1 || !strings.Contains(string(r.body), "rate_limited") {
		t.Fatalf("over the listing limit: %s Retry-After=%q", r, r.header.Get("Retry-After"))
	}
	// The limit is the organization's: another one is unaffected.
	globex := s.team(t, "rate2")
	other := s.signIn(t, globex.devUser.Email, globex.devPW).AccessToken
	s.expect(t, other, "", http.MethodGet, "/api/v1/ingestion/jobs", "", http.StatusOK, nil)

	// Repeated failed sign-ins are throttled with Retry-After too.
	wrong := fmt.Sprintf(`{"email":%q,"password":"not the password"}`, acme.ownerUser.Email)
	for i := 0; i < 2; i++ {
		if r := s.call(t, "", "", http.MethodPost, "/api/v1/auth/login", wrong); r.status != http.StatusUnauthorized {
			t.Fatalf("failed sign-in %d: %s", i, r)
		}
	}
	right := fmt.Sprintf(`{"email":%q,"password":%q}`, acme.ownerUser.Email, acme.ownerPW)
	r = s.call(t, "", "", http.MethodPost, "/api/v1/auth/login", right)
	if r.status != http.StatusTooManyRequests || r.header.Get("Retry-After") == "" {
		t.Fatalf("throttled sign-in: %s", r)
	}

	// The organization's own request quota applies to all its routes.
	setQuota(t, s, globex.org.ID, model.TenantQuota{MaxRequestsPerMinute: 2})
	statuses := []int{}
	for i := 0; i < 4; i++ {
		statuses = append(statuses, s.call(t, other, "", http.MethodGet, "/api/v1/organization", "").status)
	}
	if statuses[len(statuses)-1] != http.StatusTooManyRequests {
		t.Fatalf("the tenant request quota: %v", statuses)
	}
}

func TestHTTPAuditAccess(t *testing.T) {
	s := openSecurityStack(t, nil)
	acme := s.team(t, "logs")
	auditorTok := s.signIn(t, acme.auditorUser.Email, acme.auditorPW).AccessToken
	devTok := s.signIn(t, acme.devUser.Email, acme.devPW).AccessToken
	ownerTok := s.signIn(t, acme.ownerUser.Email, acme.ownerPW).AccessToken

	var page response.AuditPageResponse
	s.expect(t, auditorTok, "", http.MethodGet, "/api/v1/organization/audit?limit=50", "", http.StatusOK, &page)
	if len(page.Events) == 0 {
		t.Fatal("the auditor sees the organization's events")
	}
	for _, ev := range page.Events {
		if ev.TenantID != acme.org.ID || ev.Hash == "" {
			t.Fatalf("event %+v", ev)
		}
	}
	var verified response.AuditVerificationResponse
	s.expect(t, auditorTok, "", http.MethodGet, "/api/v1/organization/audit/verify", "", http.StatusOK, &verified)
	if !verified.Intact || verified.Chain != acme.org.ID {
		t.Fatalf("verify: %+v", verified)
	}
	if r := s.call(t, devTok, "", http.MethodGet, "/api/v1/organization/audit", ""); r.status != http.StatusForbidden {
		t.Fatalf("developer reads the audit log: %s", r)
	}
	// Owners administer their organization, not the platform.
	for _, path := range []string{"/api/v1/admin/audit", "/api/v1/admin/users", "/api/v1/admin/organizations"} {
		if r := s.call(t, ownerTok, "", http.MethodGet, path, ""); r.status != http.StatusForbidden {
			t.Fatalf("owner GET %s: %s", path, r)
		}
	}
}

func TestHTTPDeadLetterReplayNeedsPlatformAdministrator(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "dlq")
	ownerTok := s.signIn(t, acme.ownerUser.Email, acme.ownerPW).AccessToken

	now := time.Now().UTC()
	dl := model.DeadLetter{
		ID: eventdto.NewID(), EventID: eventdto.NewID(), Source: model.SourceConsumer, Consumer: "it-consumer",
		EventType: "upload.completed", SchemaVersion: 1, TenantID: acme.org.ID, AggregateType: "upload", AggregateID: "u1",
		FailureCategory: model.FailureExhausted, LastError: "failed", Attempts: 3,
		FirstFailedAt: now, LastFailedAt: now, DeadLetteredAt: now, Status: model.DeadLetterOpen, UpdatedAt: now,
	}
	if err := s.dead.Add(ctx, dl); err != nil {
		t.Fatal(err)
	}

	// Even the owner of the event's organization cannot replay it: a
	// replay redoes work with the platform's authority.
	reason := `{"reason":"retry after fix"}`
	for _, action := range []string{"replay", "retry", "discard", "resolve"} {
		r := s.call(t, ownerTok, "", http.MethodPost, "/api/v1/admin/dead-letters/"+dl.ID+"/"+action, reason)
		if r.status != http.StatusForbidden {
			t.Fatalf("owner %s: %s", action, r)
		}
	}
	if r := s.call(t, ownerTok, "", http.MethodGet, "/api/v1/admin/dead-letters", ""); r.status != http.StatusForbidden {
		t.Fatalf("owner lists dead letters: %s", r)
	}
	if got, err := s.dead.Get(ctx, dl.ID); err != nil || got.Status != model.DeadLetterOpen {
		t.Fatalf("a refused replay changes nothing: %+v %v", got, err)
	}
	denied := 0
	for _, ev := range s.auditEvents(t, model.PlatformChain, "dead_letter.replay") {
		if ev.ResourceID == dl.ID && ev.Outcome == model.AuditDenied && ev.ActorID == acme.ownerUser.ID {
			denied++
		}
	}
	if denied != 1 {
		t.Fatalf("the refused replay is audited: %d", denied)
	}

	// A platform administrator gets through to the replay itself, and
	// must give a reason.
	adminUser, created, err := s.identity.CreateUser(ctx, operator, s.email("platform-admin"), "", model.PlatformAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.identity.Activate(ctx, created.Token, "platform admin password", s.client); err != nil {
		t.Fatal(err)
	}
	adminTok := s.signIn(t, adminUser.Email, "platform admin password").AccessToken
	s.expect(t, adminTok, "", http.MethodGet, "/api/v1/admin/dead-letters/"+dl.ID, "", http.StatusOK, nil)
	if r := s.call(t, adminTok, "", http.MethodPost, "/api/v1/admin/dead-letters/"+dl.ID+"/discard", `{"reason":""}`); r.status != http.StatusBadRequest {
		t.Fatalf("no reason: %s", r)
	}
	s.expect(t, adminTok, "", http.MethodPost, "/api/v1/admin/dead-letters/"+dl.ID+"/discard", `{"reason":"test event"}`, http.StatusNoContent, nil)
	if got, _ := s.dead.Get(ctx, dl.ID); got.Status != model.DeadLetterDiscarded {
		t.Fatalf("discarded by the administrator: %+v", got)
	}
}

func TestSensitiveDataStaysOutOfLogsAndAudit(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	started := time.Now().UTC().Add(-time.Second)
	acme := s.team(t, "quiet")
	ready := s.availableAsset(t, acme.developer, "doc", textData("doc", 64))

	// Exercise everything that handles a secret.
	secrets := []string{acme.ownerPW, acme.devPW, acme.adminPW, acme.auditorPW}
	tokens := s.signIn(t, acme.devUser.Email, acme.devPW)
	secrets = append(secrets, tokens.AccessToken, tokens.RefreshToken)
	s.call(t, "", "", http.MethodPost, "/api/v1/auth/login", fmt.Sprintf(`{"email":%q,"password":"wrong-but-secret-pw"}`, acme.devUser.Email))
	secrets = append(secrets, "wrong-but-secret-pw")
	var next response.TokensResponse
	s.expect(t, "", "", http.MethodPost, "/api/v1/auth/refresh", fmt.Sprintf(`{"refresh_token":%q}`, tokens.RefreshToken), http.StatusOK, &next)
	secrets = append(secrets, next.AccessToken, next.RefreshToken)
	s.call(t, "", "", http.MethodPost, "/api/v1/auth/refresh", fmt.Sprintf(`{"refresh_token":%q}`, tokens.RefreshToken))

	fresh := s.signIn(t, acme.devUser.Email, acme.devPW)
	secrets = append(secrets, fresh.AccessToken, fresh.RefreshToken)
	var link, tokenLink response.DownloadURLResponse
	s.expect(t, fresh.AccessToken, "", http.MethodPost, "/api/v1/assets/"+ready.ID+"/download-url", "", http.StatusOK, &link)
	s.expect(t, fresh.AccessToken, "", http.MethodPost, "/api/v1/assets/"+ready.ID+"/download-url", `{"mode":"token"}`, http.StatusOK, &tokenLink)
	secrets = append(secrets, link.URL, tokenLink.URL[strings.LastIndex(tokenLink.URL, "/")+1:])

	ownerTok := s.signIn(t, acme.ownerUser.Email, acme.ownerPW).AccessToken
	secrets = append(secrets, ownerTok)
	var inv response.InvitationResponse
	s.expect(t, ownerTok, "", http.MethodPost, "/api/v1/organization/invitations",
		fmt.Sprintf(`{"email":%q,"role_id":"auditor"}`, s.email("invitee")), http.StatusCreated, &inv)
	secrets = append(secrets, inv.Token)
	s.expect(t, "", "", http.MethodPost, "/api/v1/invitations/accept",
		fmt.Sprintf(`{"token":%q,"password":"invitee-secret-password"}`, inv.Token), http.StatusCreated, nil)
	secrets = append(secrets, "invitee-secret-password")

	reset, err := s.identity.IssueReset(ctx, operator, acme.auditorUser.ID)
	if err != nil {
		t.Fatal(err)
	}
	secrets = append(secrets, reset.Token)
	s.expect(t, "", "", http.MethodPost, "/api/v1/auth/password/reset",
		fmt.Sprintf(`{"token":%q,"password":"reset-secret-password"}`, reset.Token), http.StatusOK, nil)
	s.call(t, "", "", http.MethodPost, "/api/v1/auth/password/reset", fmt.Sprintf(`{"token":%q,"password":"reset-secret-password"}`, reset.Token))
	secrets = append(secrets, "reset-secret-password")
	s.call(t, "sat_"+strings.Repeat("Z", 43), "", http.MethodGet, "/api/v1/auth/me", "")
	secrets = append(secrets, "sat_"+strings.Repeat("Z", 43))

	// None of it reached the logs...
	logs := s.logs.String()
	if logs == "" {
		t.Fatal("expected some log output to check")
	}
	for _, secret := range secrets {
		if strings.Contains(logs, secret) {
			t.Fatalf("a secret reached the logs: %.12s...", secret)
		}
	}
	if strings.Contains(logs, "Bearer ") {
		t.Fatal("an authorization header reached the logs")
	}

	// ...or the audit log.
	rows, err := s.db.Pool.Query(ctx, `
		SELECT action, reason, resource_id, details::text FROM audit_events
		WHERE created_at >= $1 AND (chain = $2 OR chain = '_platform')
	`, started, acme.org.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	checked := 0
	for rows.Next() {
		var action, reason, resource, details string
		if err := rows.Scan(&action, &reason, &resource, &details); err != nil {
			t.Fatal(err)
		}
		checked++
		record := reason + " " + resource + " " + details
		for _, secret := range secrets {
			if strings.Contains(record, secret) {
				t.Fatalf("a secret reached the audit record of %s: %.12s...", action, secret)
			}
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(details), &parsed); err != nil {
			t.Fatal(err)
		}
		for key := range parsed {
			if strings.Contains(key, "password") || strings.Contains(key, "token") || strings.Contains(key, "url") {
				t.Fatalf("audit detail %q in %s", key, action)
			}
		}
	}
	if checked < 10 {
		t.Fatalf("expected the flow to write audit events, found %d", checked)
	}
}
