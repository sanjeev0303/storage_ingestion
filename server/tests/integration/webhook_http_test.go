package integration

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/service/webhook"
)

type hookReceived struct {
	header http.Header
	body   []byte
}

type hookDelivery struct {
	ID             string `json:"id"`
	EventType      string `json:"event_type"`
	Status         string `json:"status"`
	Attempts       int    `json:"attempts"`
	LastStatusCode int    `json:"last_status_code"`
}

func TestHTTPWebhooksDeliverSignedEvents(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "hook-a")
	globex := s.team(t, "hook-b")
	adminTok := s.signIn(t, acme.adminUser.Email, acme.adminPW).AccessToken
	devTok := s.signIn(t, acme.devUser.Email, acme.devPW).AccessToken
	globexTok := s.signIn(t, globex.adminUser.Email, globex.adminPW).AccessToken

	var answer atomic.Int32
	answer.Store(http.StatusOK)
	received := make(chan hookReceived, 10)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- hookReceived{header: r.Header.Clone(), body: body}
		w.WriteHeader(int(answer.Load()))
	}))
	defer receiver.Close()
	sender := webhook.NewSender(postgres.NewWebhookRepository(s.db), s.hookKeys, s.hookPolicy.Client(), "it-hooks", 4, s.log)
	send := func() {
		t.Helper()
		if _, err := sender.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// Refusals: a metadata address, an unknown type, a role without org.manage.
	for _, body := range []string{
		`{"url":"http://169.254.169.254/latest","event_types":["object.available"]}`,
		`{"url":"https://example.com/x#frag","event_types":["object.available"]}`,
		fmt.Sprintf(`{"url":%q,"event_types":["object.nope"]}`, receiver.URL),
		fmt.Sprintf(`{"url":%q,"event_types":[]}`, receiver.URL),
	} {
		if r := s.call(t, adminTok, "", http.MethodPost, "/api/v1/webhooks", body); r.status != http.StatusBadRequest {
			t.Fatalf("%s: %s, want 400", body, r)
		}
	}
	create := fmt.Sprintf(`{"url":%q,"description":"ci","event_types":["object.available","dataset.snapshot_created"]}`, receiver.URL+"/hooks?src=it")
	if r := s.call(t, devTok, "", http.MethodPost, "/api/v1/webhooks", create); r.status != http.StatusForbidden {
		t.Fatalf("developer creates a webhook: %s", r)
	}

	var hook struct {
		ID         string   `json:"id"`
		URL        string   `json:"url"`
		EventTypes []string `json:"event_types"`
		Enabled    bool     `json:"enabled"`
		Secret     string   `json:"secret"`
	}
	s.expect(t, adminTok, "", http.MethodPost, "/api/v1/webhooks", create, http.StatusCreated, &hook)
	if !strings.HasPrefix(hook.Secret, "whsec_") || !hook.Enabled || len(hook.EventTypes) != 2 {
		t.Fatalf("created webhook: %+v", hook)
	}
	secret, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(hook.Secret, "whsec_"))
	if err != nil {
		t.Fatal(err)
	}
	list := s.expect(t, adminTok, "", http.MethodGet, "/api/v1/webhooks", "", http.StatusOK, nil)
	if strings.Contains(string(list.body), hook.Secret) || !strings.Contains(string(list.body), hook.ID) {
		t.Fatalf("list shows the secret or misses the endpoint: %s", list.body)
	}
	if r := s.call(t, globexTok, "", http.MethodGet, "/api/v1/webhooks/"+hook.ID, ""); r.status != http.StatusNotFound {
		t.Fatalf("another organization reads the webhook: %s", r)
	}

	// A published object is an object.available event, signed.
	asset := s.availableAsset(t, acme.developer, "hooked.txt", []byte("hello hooks"))
	send()
	got := waitHook(t, received)
	id, ts := got.header.Get("webhook-id"), got.header.Get("webhook-timestamp")
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(got.body)
	if want := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)); got.header.Get("webhook-signature") != want {
		t.Fatalf("signature %q, want %q", got.header.Get("webhook-signature"), want)
	}
	var event struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Data struct {
			AssetID        string `json:"asset_id"`
			ChecksumSHA256 string `json:"checksum_sha256"`
		} `json:"data"`
	}
	if err := json.Unmarshal(got.body, &event); err != nil {
		t.Fatal(err)
	}
	if event.ID != id || event.Type != "object.available" || event.Data.AssetID != asset.ID || event.Data.ChecksumSHA256 == "" {
		t.Fatalf("event %s for asset %s", got.body, asset.ID)
	}
	if got.header.Get("Content-Type") != "application/json" {
		t.Fatalf("content type %q", got.header.Get("Content-Type"))
	}

	deliveries := func(query string) []hookDelivery {
		t.Helper()
		var page struct {
			Deliveries []hookDelivery `json:"deliveries"`
		}
		s.expect(t, adminTok, "", http.MethodGet, "/api/v1/webhooks/"+hook.ID+"/deliveries"+query, "", http.StatusOK, &page)
		return page.Deliveries
	}
	if d := deliveries(""); len(d) != 1 || d[0].Status != "DELIVERED" || d[0].LastStatusCode != 200 || d[0].Attempts != 1 {
		t.Fatalf("deliveries after success: %+v", d)
	}

	// A failing receiver: the attempt is kept for a retry; a redelivery
	// sends it again at once.
	answer.Store(http.StatusInternalServerError)
	var test hookDelivery
	s.expect(t, adminTok, "", http.MethodPost, "/api/v1/webhooks/"+hook.ID+"/test", "", http.StatusAccepted, &test)
	send()
	if got := waitHook(t, received); !strings.Contains(string(got.body), `"webhook.test"`) {
		t.Fatalf("test event: %s", got.body)
	}
	failed := deliveries("?status=PENDING")
	if len(failed) != 1 || failed[0].ID != test.ID || failed[0].Attempts != 1 || failed[0].LastStatusCode != 500 {
		t.Fatalf("after a 500: %+v", failed)
	}
	answer.Store(http.StatusNoContent)
	s.expect(t, adminTok, "", http.MethodPost, "/api/v1/webhooks/"+hook.ID+"/deliveries/"+test.ID+"/retry", "", http.StatusAccepted, nil)
	send()
	waitHook(t, received)
	if pending := deliveries("?status=PENDING"); len(pending) != 0 {
		t.Fatalf("still pending after the redelivery: %+v", pending)
	}
	if r := s.call(t, globexTok, "", http.MethodPost, "/api/v1/webhooks/"+hook.ID+"/deliveries/"+test.ID+"/retry", ""); r.status != http.StatusNotFound {
		t.Fatalf("another organization redelivers: %s", r)
	}

	// Disabled: events are not queued for it at all.
	s.expect(t, adminTok, "", http.MethodPatch, "/api/v1/webhooks/"+hook.ID, `{"enabled":false}`, http.StatusOK, nil)
	s.availableAsset(t, acme.developer, "quiet.txt", []byte("not sent"))
	send()
	select {
	case got := <-received:
		t.Fatalf("a disabled endpoint received %s", got.body)
	case <-time.After(300 * time.Millisecond):
	}
	if d := deliveries(""); len(d) != 2 {
		t.Fatalf("a disabled endpoint got a delivery queued: %+v", d)
	}

	var rotated struct {
		Secret string `json:"secret"`
	}
	s.expect(t, adminTok, "", http.MethodPost, "/api/v1/webhooks/"+hook.ID+"/rotate-secret", "", http.StatusOK, &rotated)
	if !strings.HasPrefix(rotated.Secret, "whsec_") || rotated.Secret == hook.Secret {
		t.Fatalf("rotated secret %q", rotated.Secret)
	}
	s.expect(t, adminTok, "", http.MethodDelete, "/api/v1/webhooks/"+hook.ID, "", http.StatusNoContent, nil)
	if r := s.call(t, adminTok, "", http.MethodGet, "/api/v1/webhooks/"+hook.ID, ""); r.status != http.StatusNotFound {
		t.Fatalf("deleted webhook: %s", r)
	}
	// The developer's refusal and the admin's creation; the creation's
	// details name the host, never the path or query.
	events := s.auditEvents(t, acme.org.ID, "webhook.create")
	if len(events) != 2 || events[0].Outcome != "DENIED" || events[1].Outcome != "SUCCESS" ||
		strings.Contains(fmt.Sprint(events[1].Details), "src=it") {
		t.Fatalf("audit of the creation: %+v", events)
	}
}

func waitHook(t *testing.T, received <-chan hookReceived) hookReceived {
	t.Helper()
	select {
	case got := <-received:
		return got
	case <-time.After(15 * time.Second):
		t.Fatal("no webhook was received")
	}
	return hookReceived{}
}

// The processing record shows the scan's verdict and the stage history;
// the account record says whether a password exists.
func TestHTTPProcessingRecordAndAccountShowNewFields(t *testing.T) {
	s := openSecurityStack(t, nil)
	acme := s.team(t, "rec-a")
	devTok := s.signIn(t, acme.devUser.Email, acme.devPW).AccessToken
	asset := s.availableAsset(t, acme.developer, "scanned.txt", []byte("scan me"))

	var record struct {
		Scan *struct {
			Scanner string `json:"scanner"`
			Result  string `json:"result"`
		} `json:"scan"`
		Events []struct {
			Event string `json:"event"`
		} `json:"events"`
	}
	s.expect(t, devTok, "", http.MethodGet, "/api/v1/assets/"+asset.ID+"/quarantine", "", http.StatusOK, &record)
	if record.Scan == nil || record.Scan.Result != "CLEAN" || len(record.Events) == 0 {
		t.Fatalf("processing record: %+v", record)
	}

	var me struct {
		User struct {
			HasPassword bool `json:"has_password"`
		} `json:"user"`
	}
	s.expect(t, devTok, "", http.MethodGet, "/api/v1/auth/me", "", http.StatusOK, &me)
	if !me.User.HasPassword {
		t.Fatal("an account with a password says it has none")
	}
	var lifecycle map[string]any
	s.expect(t, devTok, "", http.MethodGet, "/api/v1/assets/"+asset.ID+"/lifecycle", "", http.StatusOK, &lifecycle)
	if _, ok := lifecycle["locked_until"]; ok {
		t.Fatalf("a version on a bucket without Object Lock shows a lock: %v", lifecycle)
	}
}
