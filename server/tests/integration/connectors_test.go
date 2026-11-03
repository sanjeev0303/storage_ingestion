package integration

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/request"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/server"
	"github.com/sanjeev0303/object-storage-platform/internal/server/middleware"
	"github.com/sanjeev0303/object-storage-platform/internal/service/connectors"
	"github.com/sanjeev0303/object-storage-platform/internal/service/identity"
)

func dicomElement(tag uint32, vr, value string) []byte {
	if len(value)%2 == 1 {
		value += " "
	}
	out := binary.LittleEndian.AppendUint16(nil, uint16(tag>>16))
	out = binary.LittleEndian.AppendUint16(out, uint16(tag))
	out = append(out, vr...)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(value)))
	return append(out, value...)
}

func dicomFile(study, series, instance, modality, patient string) []byte {
	out := append(make([]byte, 128), "DICM"...)
	for _, e := range [][]byte{
		dicomElement(0x00020010, "UI", "1.2.840.10008.1.2.1"),
		dicomElement(0x00080018, "UI", instance), dicomElement(0x00080060, "CS", modality), dicomElement(0x00100020, "LO", patient),
		dicomElement(0x0020000D, "UI", study), dicomElement(0x0020000E, "UI", series),
		dicomElement(0x7FE00010, "OB", "pixels-"+instance),
	} {
		out = append(out, e...)
	}
	return out
}

// Chat shares post a sign-in-gated link that stops working; Git LFS speaks
// its Batch and locking APIs; collections are recorded; and DICOMweb
// searches and retrieves over a snapshot.
func TestConnectors(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "connect")
	dev, owner := acme.developer, acme.owner
	devToken := s.signIn(t, acme.devUser.Email, acme.devPW).AccessToken
	ownerToken := s.signIn(t, acme.ownerUser.Email, acme.ownerPW).AccessToken

	var mu sync.Mutex
	var messages []string
	fail := false
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		messages = append(messages, body.Text)
	}))
	defer chat.Close()

	// Allowed hosts: the real list refuses anything else.
	strict := connectors.New(connectors.Deps{Store: postgres.NewConnectorsRepository(s.db), Sealer: s.hookKeys, Audit: s.audit, Log: s.log})
	if _, err := strict.AddIntegration(ctx, owner, "SLACK", "x", "https://evil.example/services/T/B/C"); !errors.Is(err, model.ErrInvalidConn) {
		t.Fatalf("a host that is not Slack's: %v", err)
	}
	conn := connectors.New(connectors.Deps{Store: postgres.NewConnectorsRepository(s.db), Uploads: s.ie.single, Delivery: s.delivery, Datasets: s.datasets,
		Sealer: s.hookKeys, Audit: s.audit, Client: http.DefaultClient, ConsoleURL: "https://console.test", Log: s.log,
		ChatHosts: map[string][]string{"SLACK": {"*"}}, VerifyWait: -1})

	if _, err := conn.AddIntegration(ctx, dev, "SLACK", "eng", chat.URL); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("integrations need org.manage: %v", err)
	}
	in, err := conn.AddIntegration(ctx, owner, "SLACK", "eng", chat.URL)
	if err != nil || bytes.Contains(in.Cipher, []byte("127.0.0.1")) {
		t.Fatalf("integration: %+v %v", in.URLHost, err)
	}
	file := s.publishAt(t, dev, "docs/brief.txt", textData("brief", 200))
	share, err := conn.ShareToChat(ctx, dev, file.ID, in.ID, "for review", 0)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	sent := messages[0]
	mu.Unlock()
	if !strings.Contains(sent, "https://console.test/app/share/"+share.ID) || !strings.Contains(sent, "brief.txt") || strings.Contains(sent, "X-Amz") {
		t.Fatalf("the message holds a gated link, no credential: %s", sent)
	}
	mu.Lock()
	fail = true
	mu.Unlock()
	if _, err := conn.ShareToChat(ctx, dev, file.ID, in.ID, "", 0); !errors.Is(err, model.ErrChatDelivery) {
		t.Fatalf("a refused message: %v", err)
	}
	if got, _ := conn.Shares(ctx, owner); len(got) != 1 {
		t.Fatalf("a share whose message failed stands: %d", len(got))
	}
	opened, err := conn.OpenShare(ctx, dev, share.ID, "req")
	if err != nil || opened.URL == "" {
		t.Fatalf("open: %+v %v", opened, err)
	}
	if _, err := conn.OpenShare(ctx, acme.auditor, share.ID, "req"); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("opening still needs the right to download: %v", err)
	}
	if err := conn.RevokeShare(ctx, acme.auditor, share.ID); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("revoking someone's share: %v", err)
	}
	if err := conn.RevokeShare(ctx, dev, share.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.OpenShare(ctx, dev, share.ID, "req"); !errors.Is(err, model.ErrChatExpired) {
		t.Fatalf("after revoking: %v", err)
	}

	// Collections.
	d := s.newDataset(t, dev, "connect-set")
	got, err := conn.RecordCollection(ctx, dev, model.CollectionImport{DatasetID: d.ID, Format: "rsmf", Source: "Relativity export", Items: 3, Verified: 2,
		Mismatches: []string{"msg-3: declared abc, computed def"}})
	if err != nil || got.Format != model.CollectionRSMF {
		t.Fatalf("collection: %+v %v", got, err)
	}
	if _, err := conn.RecordCollection(ctx, dev, model.CollectionImport{DatasetID: d.ID, Format: "PST", Items: 1}); !errors.Is(err, model.ErrInvalidConn) {
		t.Fatalf("an unknown format: %v", err)
	}
	if list, _ := conn.Collections(ctx, dev, d.ID); len(list) != 1 || list[0].Mismatches[0] == "" {
		t.Fatalf("collections: %+v", list)
	}

	// Git LFS: batch, upload, verify, batch again, download.
	lfsAPI := httptest.NewServer(connectorAPI(conn, s))
	defer lfsAPI.Close()
	content := textData("a large game asset", 4096)
	oid := sha256Of(content)
	lfs := func(token, path, body string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, lfsAPI.URL+"/lfs/game"+path, strings.NewReader(body))
		req.SetBasicAuth("git", token)
		req.Header.Set("Content-Type", "application/vnd.git-lfs+json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, raw
	}
	var batch struct {
		Objects []connectors.LFSObject `json:"objects"`
	}
	batchBody := func(op string) string {
		return `{"operation":"` + op + `","transfers":["basic"],"objects":[{"oid":"` + oid + `","size":4096}]}`
	}
	if code, _ := lfs("wrong", "/objects/batch", batchBody("upload")); code != http.StatusUnauthorized {
		t.Fatalf("a bad token: %d", code)
	}
	code, raw := lfs(devToken, "/objects/batch", batchBody("download"))
	if err := json.Unmarshal(raw, &batch); err != nil || code != http.StatusOK || batch.Objects[0].Error == nil || batch.Objects[0].Error.Code != 404 {
		t.Fatalf("an object not there yet: %d %s", code, raw)
	}
	batch.Objects = nil
	code, raw = lfs(devToken, "/objects/batch", batchBody("upload"))
	if err := json.Unmarshal(raw, &batch); err != nil || code != http.StatusOK {
		t.Fatalf("batch: %d %s", code, raw)
	}
	if batch.Objects[0].Actions["upload"].Href == "" {
		t.Fatalf("no upload action: %s", raw)
	}
	verify := batch.Objects[0].Actions["verify"]
	path := strings.TrimPrefix(verify.Href, lfsAPI.URL+"/lfs/game")
	uploadID := path[strings.Index(path, "=")+1:]
	session, err := s.ie.single.GetFor(ctx, dev, uploadID)
	if err != nil {
		t.Fatal(err)
	}
	s.ie.storage.PutObject(session.ObjectKey, content) // the fake storage's PUT
	if code, raw := lfs(devToken, path, `{"oid":"`+oid+`","size":4096}`); code != http.StatusOK {
		t.Fatalf("verify: %d %s", code, raw)
	}
	if _, err := s.ie.proc.Intake(ctx, dev.TenantID, uploadID); err != nil {
		t.Fatal(err)
	}
	batch.Objects = nil
	code, raw = lfs(devToken, "/objects/batch", batchBody("upload"))
	if err := json.Unmarshal(raw, &batch); err != nil || code != http.StatusOK || batch.Objects[0].Actions != nil {
		t.Fatalf("an object already there needs no upload: %s", raw)
	}
	batch.Objects = nil
	code, raw = lfs(devToken, "/objects/batch", batchBody("download"))
	if err := json.Unmarshal(raw, &batch); err != nil || code != http.StatusOK || batch.Objects[0].Actions["download"].Href == "" {
		t.Fatalf("download: %s", raw)
	}
	if code, raw := lfs(devToken, "/objects/batch", `{"operation":"delete","objects":[]}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("an unknown operation: %d %s", code, raw)
	}

	// Locks.
	code, raw = lfs(devToken, "/locks", `{"path":"art/hero.psd"}`)
	var lock struct {
		Lock struct {
			ID string `json:"id"`
		} `json:"lock"`
	}
	if err := json.Unmarshal(raw, &lock); err != nil || code != http.StatusCreated {
		t.Fatalf("lock: %d %s", code, raw)
	}
	if code, _ := lfs(ownerToken, "/locks", `{"path":"art/hero.psd"}`); code != http.StatusConflict {
		t.Fatalf("a held path: %d", code)
	}
	code, raw = lfs(ownerToken, "/locks/verify", `{}`)
	var sides struct {
		Ours, Theirs []any
	}
	if err := json.Unmarshal(raw, &sides); err != nil || code != http.StatusOK || len(sides.Theirs) != 1 || len(sides.Ours) != 0 {
		t.Fatalf("verify locks: %s", raw)
	}
	if code, _ := lfs(ownerToken, "/locks/"+lock.Lock.ID+"/unlock", `{}`); code != http.StatusForbidden {
		t.Fatalf("unlocking another's: %d", code)
	}
	if code, _ := lfs(ownerToken, "/locks/"+lock.Lock.ID+"/unlock", `{"force":true}`); code != http.StatusOK {
		t.Fatalf("force: %d", code)
	}

	// DICOMweb over a snapshot, from the index the profile worker builds.
	dcm := func(req *request.CreateUploadRequest) { req.ContentType = "application/dicom" }
	a1 := s.publishAt(t, dev, "ct/a1.dcm", dicomFile("1.1", "1.1.1", "1.1.1.1", "CT", "P-1"), dcm)
	a2 := s.publishAt(t, dev, "ct/a2.dcm", dicomFile("1.1", "1.1.1", "1.1.1.2", "CT", "P-1"), dcm)
	a3 := s.publishAt(t, dev, "mr/b1.dcm", dicomFile("2.2", "2.2.1", "2.2.1.1", "MR", "P-2"), dcm)
	img := s.newDataset(t, dev, "imaging")
	s.addMembers(t, dev, img.ID, memberOf(a1, "", ""), memberOf(a2, "", ""), memberOf(a3, "", ""))
	v, _, err := s.datasets.Snapshot(ctx, dev, img.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	s.profileAll(t)
	web := httptest.NewServer(connectorAPI(conn, s))
	defer web.Close()
	wado := func(path, accept string) (*http.Response, []byte) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, web.URL+"/dicomweb/"+img.ID+"/"+v.ID+path, nil)
		req.Header.Set("Authorization", "Bearer "+devToken)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp, raw
	}
	resp, raw := wado("/studies", "")
	var studies []map[string]map[string]any
	if err := json.Unmarshal(raw, &studies); err != nil || len(studies) != 2 || resp.Header.Get("Content-Type") != "application/dicom+json" {
		t.Fatalf("studies: %s", raw)
	}
	if studies[0]["00201208"]["Value"].([]any)[0].(float64) != 2 || studies[0]["00080061"]["Value"].([]any)[0] != "CT" {
		t.Fatalf("study counts and modalities: %v", studies[0])
	}
	_, raw = wado("/studies?PatientID=P-2", "")
	if json.Unmarshal(raw, &studies); len(studies) != 1 || studies[0]["0020000D"]["Value"].([]any)[0] != "2.2" {
		t.Fatalf("search by patient: %s", raw)
	}
	if resp, _ := wado("/studies?ModalitiesInStudy=XR", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("no match: %d", resp.StatusCode)
	}
	_, raw = wado("/studies/1.1/series/1.1.1/instances", "")
	if json.Unmarshal(raw, &studies); len(studies) != 2 {
		t.Fatalf("instances: %s", raw)
	}
	if resp, _ := wado("/studies/1.1", "image/png"); resp.StatusCode != http.StatusNotAcceptable {
		t.Fatalf("rendered images are not served: %d", resp.StatusCode)
	}
	resp, raw = wado("/studies/1.1", `multipart/related; type="application/dicom"`)
	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || params["type"] != "application/dicom" {
		t.Fatalf("wado content type: %q %v", resp.Header.Get("Content-Type"), err)
	}
	parts := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
	count := 0
	for {
		p, err := parts.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(p)
		if !bytes.Contains(body, []byte("DICM")) || !bytes.Contains(body, []byte("pixels-1.1.1.")) {
			t.Fatalf("a part is not the stored file: %d bytes", len(body))
		}
		count++
	}
	if count != 2 {
		t.Fatalf("parts: %d", count)
	}
	_, raw = wado("/studies/2.2/series/2.2.1/instances/2.2.1.1/metadata", "")
	if json.Unmarshal(raw, &studies); len(studies) != 1 || studies[0]["00080060"]["Value"].([]any)[0] != "MR" {
		t.Fatalf("metadata: %s", raw)
	}
	if resp, _ := wado("/studies/9.9", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("an unknown study: %d", resp.StatusCode)
	}
}

// connectorAPI serves the connectors built here, with a local chat receiver.
func connectorAPI(conn *connectors.Service, s *securityStack) http.Handler {
	return server.NewRouter(server.Routes{Resolver: middleware.BearerResolver[identity.Identity](s.identity), Connectors: conn, Limiter: s.limiter, Log: s.log})
}
