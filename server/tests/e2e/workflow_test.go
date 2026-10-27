package e2e

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const partSize = 5 << 20

// TestTwoTenantsThroughTheRunningStack is the product workflow, start to
// finish, for two organizations that each bring their own bucket. The step
// numbers are the release checklist's (docs/release-candidate.md).
func TestTwoTenantsThroughTheRunningStack(t *testing.T) {
	s := startStack(t)
	run := strconv.FormatInt(time.Now().UnixNano(), 36)
	db, err := pgx.Connect(context.Background(), s.dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())

	// 1-2. A platform administrator provisions two organizations, each with
	// its own owner, who signs in.
	admin := s.signInAdmin()
	a := s.newTenant(admin, "a", run)
	b := s.newTenant(admin, "b", run)
	t.Logf("organizations: a=%s b=%s", a.org, b.org)

	// 3-4. Each connects its own bucket, tests it and makes it the default.
	connA := a.connect("primary", "e2e-tenant-a", true)
	connB := b.connect("primary", "e2e-tenant-b", true)

	// 5, 7, 8, 9. A small object lands in tenant A's bucket, is checked
	// against its SHA-256, scanned by ClamAV and promoted there.
	hello := text("hello from tenant a", 4<<10)
	upload := a.upload("docs/hello.txt", hello, "")
	helloAsset := a.checkStored(db, upload, a.published("docs/hello.txt", 1), connA, "e2e-tenant-a", hello)

	// 6. A multipart object of three parts.
	big := text("multipart object of tenant a", 2*partSize+(1<<20))
	upload = a.uploadMultipart("data/big.txt", big)
	bigAsset := a.checkStored(db, upload, a.published("data/big.txt", 1), connA, "e2e-tenant-a", big)

	// 10. Whole and ranged reads through the API and a presigned URL; the
	// multipart object is read across a part boundary.
	a.checkReads(helloAsset, hello, "e2e-tenant-a", 10, 99)
	a.checkReads(bigAsset, big, "e2e-tenant-a", partSize-16, partSize+15)

	// 8. Malware is rejected, held and never promoted.
	a.checkMalwareRejected(db, "e2e-tenant-a")

	// 11. A second upload to the same path becomes version 2.
	hello2 := text("hello again from tenant a", 6<<10)
	upload = a.upload("docs/hello.txt", hello2, "")
	v2 := a.published("docs/hello.txt", 2)
	hello2Asset := a.checkStored(db, upload, v2, connA, "e2e-tenant-a", hello2)
	a.checkVersions("docs/hello.txt", helloAsset, hello2Asset)

	// 12-15. A dataset from a manifest, a snapshot, validation, readiness,
	// findings and lineage.
	a.checkDataset(run)

	// 17. Tenant B repeats the uploads on its own bucket, at the same paths.
	helloB := text("hello from tenant b", 3<<10)
	upload = b.upload("docs/hello.txt", helloB, "")
	helloBAsset := b.checkStored(db, upload, b.published("docs/hello.txt", 1), connB, "e2e-tenant-b", helloB)
	b.checkReads(helloBAsset, helloB, "e2e-tenant-b", 0, 9)
	bigB := text("multipart object of tenant b", partSize+(512<<10))
	upload = b.uploadMultipart("data/big.txt", bigB)
	b.checkStored(db, upload, b.published("data/big.txt", 1), connB, "e2e-tenant-b", bigB)

	// Neither tenant reaches the other's connections, objects or catalog.
	checkIsolation(a, b, connA, helloAsset)
	checkIsolation(b, a, connB, helloBAsset)

	// A second connection chosen per upload, then disabled; and a deleted one.
	b.checkConnectionStates(db, run)

	// Credential rotation: a failing key is refused, a good one is used.
	a.checkRotation(db, connA)

	// 16. Recovery: work completed while the processing worker is down is
	// picked up when it comes back.
	s.kill("processing-worker")
	recovery := text("written while the processing worker was down", 2<<10)
	upload = a.upload("docs/recovery.txt", recovery, "")
	time.Sleep(3 * time.Second)
	if current(a.lookup("docs/recovery.txt")) != nil {
		t.Fatal("an upload was published while no processing worker ran")
	}
	s.start("processing-worker")
	a.checkStored(db, upload, a.published("docs/recovery.txt", 1), connA, "e2e-tenant-a", recovery)

	// 16. Retention and deletion: the cleanup worker purges the object from
	// the tenant's bucket.
	a.must(http.StatusOK, http.MethodPut, "/api/v1/retention/policies", map[string]any{
		"scope": "TENANT", "deletion_grace": "0s", "reason": "e2e: purge deleted assets at once"})
	trash := text("deleted through the lifecycle", 1<<10)
	upload = a.upload("docs/trash.txt", trash, "")
	trashAsset := a.checkStored(db, upload, a.published("docs/trash.txt", 1), connA, "e2e-tenant-a", trash)
	deleted := a.must(http.StatusAccepted, http.MethodDelete, "/api/v1/assets/"+trashAsset, nil)
	if deleted["status"] != "SOFT_DELETED" {
		t.Fatalf("delete answered %v", deleted)
	}
	eventually(t, time.Minute, "the cleanup worker to finish the deletion", func() bool {
		return a.do(http.MethodGet, "/api/v1/assets/"+trashAsset, nil).status == http.StatusNotFound
	})
	if s.objectExists("e2e-tenant-a", readyKey(a.org, trashAsset)) {
		t.Fatal("the deleted asset's object is still in the tenant's bucket")
	}

	// Secrets never reach output, metrics or the database in plain text.
	s.checkNoSecrets(db)
}

// ─── Accounts ─────────────────────────────────────────────────────────

func (s *stack) signInAdmin() *client {
	s.t.Helper()
	const email, password = "admin@e2e.test", "e2e administrator password"
	file := filepath.Join(s.dir, "admin-password")
	if err := os.WriteFile(file, []byte(password), 0o600); err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.admin("bootstrap", "-email", email, "-name", "E2E administrator", "-password-file", file); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			s.t.Fatal(err)
		}
		// A database this test ran on before: take the account back.
		out, err := s.admin("reset-password", "-email", email)
		if err != nil {
			s.t.Fatalf("%v (the database's platform administrator is not %s)", err, email)
		}
		anonymous := &client{s: s}
		anonymous.must(http.StatusOK, http.MethodPost, "/api/v1/auth/password/reset",
			map[string]any{"token": out["token"], "password": password})
	}
	return s.signIn(email, password, "")
}

func (s *stack) signIn(email, password, org string) *client {
	s.t.Helper()
	anonymous := &client{s: s}
	out := anonymous.must(http.StatusOK, http.MethodPost, "/api/v1/auth/login",
		map[string]any{"email": email, "password": password})
	return &client{s: s, token: str(s.t, out, "access_token"), org: org}
}

// newTenant creates an owner account and their organization, and signs
// the owner in to it.
func (s *stack) newTenant(admin *client, name, run string) *client {
	s.t.Helper()
	email := fmt.Sprintf("owner-%s-%s@e2e.test", name, run)
	password := "e2e owner password " + name
	created := admin.must(http.StatusCreated, http.MethodPost, "/api/v1/admin/users", map[string]any{"email": email})
	anonymous := &client{s: s}
	anonymous.must(http.StatusOK, http.MethodPost, "/api/v1/auth/activate",
		map[string]any{"token": str(s.t, obj(s.t, created, "activation"), "token"), "password": password})
	org := admin.must(http.StatusCreated, http.MethodPost, "/api/v1/admin/organizations", map[string]any{
		"name": "E2E " + name, "slug": "e2e-" + name + "-" + run, "owner_user_id": str(s.t, obj(s.t, created, "user"), "id")})
	return s.signIn(email, password, str(s.t, org, "id"))
}

// ─── Storage connections ──────────────────────────────────────────────

func (s *stack) connectionBody(name, bucket string) map[string]any {
	return map[string]any{
		"name": name, "provider": "S3_COMPATIBLE", "auth_method": "ACCESS_KEY", "bucket": bucket,
		"endpoint": s.s3cfg.Endpoint, "force_path_style": true,
		"credentials": map[string]any{"access_key_id": s.byosKey, "secret_access_key": s.byosSecret},
	}
}

// connect creates, tests and optionally defaults a connection to bucket.
func (c *client) connect(name, bucket string, makeDefault bool) string {
	t := c.s.t
	t.Helper()
	c.s.bucket(bucket)
	created := c.must(http.StatusCreated, http.MethodPost, "/api/v1/storage-connections", c.s.connectionBody(name, bucket))
	id := str(t, created, "id")
	if created["status"] != "PENDING" {
		t.Fatalf("a new connection is %v, want PENDING", created["status"])
	}
	// Nothing goes to a connection before it passes a test.
	r := c.do(http.MethodPost, "/api/v1/uploads", uploadBody("early.txt", []byte("early"), id))
	if r.status < 400 {
		t.Fatalf("an upload to an untested connection was accepted: HTTP %d", r.status)
	}
	tested := c.must(http.StatusOK, http.MethodPost, "/api/v1/storage-connections/"+id+"/test", map[string]any{"write_probe": true})
	if tested["ok"] != true || obj(t, tested, "connection")["status"] != "ACTIVE" {
		t.Fatalf("connection test failed: %v", tested)
	}
	if makeDefault {
		set := c.must(http.StatusOK, http.MethodPost, "/api/v1/storage-connections/"+id+"/set-default", nil)
		if set["is_default"] != true {
			t.Fatalf("set-default answered %v", set)
		}
	}
	return id
}

func (c *client) checkConnectionStates(db *pgx.Conn, run string) {
	t := c.s.t
	t.Helper()
	// Chosen per upload, a second connection holds the object in its bucket.
	second := c.connect("secondary", "e2e-tenant-b2", false)
	data := text("chosen per upload", 2<<10)
	upload := c.upload("docs/secondary.txt", data, second)
	asset := c.checkStored(db, upload, c.published("docs/secondary.txt", 1), second, "e2e-tenant-b2", data)

	// Disabled: no new uploads, but what it holds is still served from its
	// own bucket, never moved to another.
	c.must(http.StatusOK, http.MethodPost, "/api/v1/storage-connections/"+second+"/disable", nil)
	r := c.do(http.MethodPost, "/api/v1/uploads", uploadBody("docs/after-disable.txt", data, second))
	if r.status < 400 {
		t.Fatalf("an upload to a disabled connection was accepted: HTTP %d %s", r.status, r.body)
	}
	c.checkReads(asset, data, "e2e-tenant-b2", 0, 9)
	if !c.s.objectExists("e2e-tenant-b2", readyKey(c.org, asset)) {
		t.Fatal("a disabled connection's object left its bucket")
	}
	// It cannot be deleted while it holds a live asset.
	if r := c.do(http.MethodDelete, "/api/v1/storage-connections/"+second, nil); r.status < 400 {
		t.Fatalf("a connection holding assets was deleted: HTTP %d", r.status)
	}

	// Deleted: never selectable again.
	created := c.must(http.StatusCreated, http.MethodPost, "/api/v1/storage-connections",
		c.s.connectionBody("unused-"+run, "e2e-tenant-b3"))
	unused := str(t, created, "id")
	if r := c.do(http.MethodDelete, "/api/v1/storage-connections/"+unused, nil); r.status >= 300 {
		t.Fatalf("delete an unused connection: HTTP %d %s", r.status, r.body)
	}
	r = c.do(http.MethodPost, "/api/v1/uploads", uploadBody("docs/after-delete.txt", data, unused))
	if r.status < 400 {
		t.Fatalf("an upload to a deleted connection was accepted: HTTP %d", r.status)
	}
	r = c.do(http.MethodPost, "/api/v1/storage-connections/"+unused+"/set-default", nil)
	if r.status < 400 {
		t.Fatalf("a deleted connection became the default: HTTP %d", r.status)
	}
}

// checkRotation sends a key that fails its test, then a good one.
func (c *client) checkRotation(db *pgx.Conn, id string) {
	t := c.s.t
	t.Helper()
	before := c.must(http.StatusOK, http.MethodGet, "/api/v1/storage-connections/"+id, nil)
	wrong := randomHex(20)
	c.s.secrets = append(c.s.secrets, wrong)
	r := c.do(http.MethodPost, "/api/v1/storage-connections/"+id+"/rotate-credentials", map[string]any{
		"revision": before["revision"], "credentials": map[string]any{"access_key_id": c.s.byosKey, "secret_access_key": wrong}})
	if r.status < 400 {
		t.Fatalf("credentials that fail their test were accepted: HTTP %d %s", r.status, r.body)
	}
	after := c.must(http.StatusOK, http.MethodGet, "/api/v1/storage-connections/"+id, nil)
	if after["status"] != "ACTIVE" || after["credential_rotation"] != before["credential_rotation"] {
		t.Fatalf("a refused rotation changed the connection: %v", after)
	}

	rotated := c.must(http.StatusOK, http.MethodPost, "/api/v1/storage-connections/"+id+"/rotate-credentials", map[string]any{
		"revision": after["revision"], "credentials": map[string]any{"access_key_id": c.s.byosKey, "secret_access_key": c.s.byosSecret}})
	connection := obj(t, rotated, "connection")
	if num(t, connection, "credential_rotation") != num(t, after, "credential_rotation")+1 {
		t.Fatalf("rotation answered %v", rotated)
	}
	// The workers pick the new credentials up: an upload still goes through.
	data := text("after rotation", 1<<10)
	upload := c.upload("docs/after-rotation.txt", data, "")
	c.checkStored(db, upload, c.published("docs/after-rotation.txt", 1), id, "e2e-tenant-a", data)
}

// ─── Uploads ──────────────────────────────────────────────────────────

func uploadBody(path string, data []byte, connection string) map[string]any {
	sum := sha256.Sum256(data)
	body := map[string]any{
		"file_name": filepath.Base(path), "content_type": "text/plain", "size_bytes": len(data),
		"checksum_sha256": hex.EncodeToString(sum[:]), "idempotency_key": randomHex(12), "path": path,
	}
	if connection != "" {
		body["storage_connection_id"] = connection
	}
	return body
}

// upload sends data in one PUT and returns the upload ID.
func (c *client) upload(path string, data []byte, connection string) string {
	t := c.s.t
	t.Helper()
	started := c.must(http.StatusCreated, http.MethodPost, "/api/v1/uploads", uploadBody(path, data, connection))
	header := map[string]string{"Content-Type": "text/plain"}
	if extra, ok := started["upload_headers"].(map[string]any); ok {
		for k, v := range extra {
			header[k] = fmt.Sprint(v)
		}
	}
	put(t, str(t, started, "upload_url"), data, header)
	id := str(t, started, "upload_id")
	c.must(http.StatusOK, http.MethodPost, "/api/v1/uploads/"+id+"/complete", map[string]any{})
	return id
}

// uploadMultipart sends data in 5 MiB parts and returns the upload ID.
func (c *client) uploadMultipart(path string, data []byte) string {
	t := c.s.t
	t.Helper()
	started := c.must(http.StatusCreated, http.MethodPost, "/api/v1/uploads/multipart", uploadBody(path, data, ""))
	id := str(t, started, "upload_id")

	var numbers []int
	for n := 1; (n-1)*partSize < len(data); n++ {
		numbers = append(numbers, n)
	}
	urls := c.must(http.StatusOK, http.MethodPost, "/api/v1/uploads/"+id+"/parts/urls", map[string]any{"part_numbers": numbers})
	var recorded, completed []map[string]any
	for i, p := range urls["parts"].([]any) {
		part := p.(map[string]any)
		first := i * partSize
		last := min(first+partSize, len(data))
		etag := put(t, str(t, part, "url"), data[first:last], nil)
		recorded = append(recorded, map[string]any{"part_number": numbers[i], "etag": etag, "size_bytes": last - first})
		completed = append(completed, map[string]any{"part_number": numbers[i], "etag": etag})
	}
	c.must(http.StatusOK, http.MethodPost, "/api/v1/uploads/"+id+"/parts/confirm", map[string]any{"parts": recorded})
	c.must(http.StatusOK, http.MethodPost, "/api/v1/uploads/"+id+"/complete", map[string]any{"parts": completed})
	return id
}

func (c *client) lookup(path string) map[string]any {
	c.s.t.Helper()
	r := c.do(http.MethodGet, "/api/v1/catalog/lookup?path="+url.QueryEscape(path), nil)
	if r.status != http.StatusOK {
		return nil
	}
	return r.json(c.s.t)
}

func current(object map[string]any) map[string]any {
	version, _ := object["current_version"].(map[string]any)
	return version
}

// published waits until version is the current one at path.
func (c *client) published(path string, version int) map[string]any {
	t := c.s.t
	t.Helper()
	var found map[string]any
	eventually(t, 90*time.Second, fmt.Sprintf("%s version %d to be published", path, version), func() bool {
		found = current(c.lookup(path))
		return found != nil && int(num(t, found, "version")) == version
	})
	return found
}

// checkStored checks a published version against what was uploaded and
// where it must live, and returns its asset ID.
func (c *client) checkStored(db *pgx.Conn, uploadID string, version map[string]any, connection, bucket string, data []byte) string {
	t := c.s.t
	t.Helper()
	asset := str(t, version, "asset_id")
	sum := sha256.Sum256(data)
	if version["storage_connection_id"] != connection {
		t.Fatalf("asset %s is on connection %v, want %s", asset, version["storage_connection_id"], connection)
	}
	got := c.must(http.StatusOK, http.MethodGet, "/api/v1/assets/"+asset, nil)
	if got["status"] != "AVAILABLE" || got["deliverable"] != true || got["checksum_sha256"] != hex.EncodeToString(sum[:]) ||
		int(num(t, got, "size_bytes")) != len(data) {
		t.Fatalf("asset %s: %v", asset, got)
	}
	// The promoted object is in the connection's bucket; the staged copy is
	// gone, and nothing of the tenant's is in the platform bucket.
	if !c.s.objectExists(bucket, readyKey(c.org, asset)) {
		t.Fatalf("asset %s is not at %s/%s", asset, bucket, readyKey(c.org, asset))
	}
	var staged string
	if err := db.QueryRow(context.Background(), `SELECT object_key FROM upload_sessions WHERE id = $1`, uploadID).Scan(&staged); err != nil {
		t.Fatal(err)
	}
	// The version is published before the cleanup step deletes the staged
	// copy, a moment later in the same pass.
	eventually(t, 10*time.Second, fmt.Sprintf("the staged object %s/%s to be deleted after promotion", bucket, staged), func() bool {
		return !c.s.objectExists(bucket, staged)
	})
	for _, prefix := range []string{"assets/" + c.org + "/", "quarantine/" + c.org + "/"} {
		if keys := c.s.keysIn(c.s.platform, prefix); len(keys) > 0 {
			t.Fatalf("the platform bucket holds the tenant's objects: %v", keys)
		}
	}
	return asset
}

// checkReads reads the asset whole and in a range, through the API and
// through a presigned URL to the tenant's bucket.
func (c *client) checkReads(asset string, data []byte, bucket string, first, last int) {
	t := c.s.t
	t.Helper()
	r := c.do(http.MethodGet, "/api/v1/assets/"+asset+"/content", nil)
	if r.status != http.StatusOK || string(r.body) != string(data) {
		t.Fatalf("content of %s: HTTP %d, %d bytes, want %d", asset, r.status, len(r.body), len(data))
	}
	byteRange := fmt.Sprintf("bytes=%d-%d", first, last)
	r = c.do(http.MethodGet, "/api/v1/assets/"+asset+"/content", nil, "Range", byteRange)
	want := fmt.Sprintf("bytes %d-%d/%d", first, last, len(data))
	if r.status != http.StatusPartialContent || r.header.Get("Content-Range") != want || string(r.body) != string(data[first:last+1]) {
		t.Fatalf("range of %s: HTTP %d %q", asset, r.status, r.header.Get("Content-Range"))
	}

	link := c.must(http.StatusOK, http.MethodPost, "/api/v1/assets/"+asset+"/download-url", map[string]any{"mode": "storage"})
	presigned := str(t, link, "url")
	if !strings.HasPrefix(presigned, c.s.s3cfg.Endpoint+"/"+bucket+"/") {
		t.Fatalf("the download URL does not point at %s", bucket)
	}
	req, err := http.NewRequest(http.MethodGet, presigned, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", byteRange)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent || string(body) != string(data[first:last+1]) {
		t.Fatalf("presigned range of %s: HTTP %d", asset, resp.StatusCode)
	}
}

// checkMalwareRejected uploads the EICAR test file: it is rejected, kept in
// quarantine and never promoted.
func (c *client) checkMalwareRejected(db *pgx.Conn, bucket string) {
	t := c.s.t
	t.Helper()
	upload := c.upload("docs/eicar.txt", eicar(), "")
	// A rejected asset is never published, so its ID is read from the database.
	var asset, status string
	eventually(t, 90*time.Second, "the EICAR upload to be judged", func() bool {
		err := db.QueryRow(context.Background(), `SELECT id, status FROM assets WHERE upload_id = $1`, upload).Scan(&asset, &status)
		return err == nil && status != "QUARANTINED" && status != "PROCESSING"
	})
	got := c.must(http.StatusOK, http.MethodGet, "/api/v1/assets/"+asset, nil)
	if got["status"] != "REJECTED" || got["rejection_reason"] != "malware_detected" || got["deliverable"] != false {
		t.Fatalf("EICAR asset: %v", got)
	}
	if r := c.do(http.MethodGet, "/api/v1/assets/"+asset+"/content", nil); r.status == http.StatusOK {
		t.Fatal("a rejected asset was served")
	}
	if c.s.objectExists(bucket, readyKey(c.org, asset)) {
		t.Fatal("a rejected asset was promoted")
	}
	if current(c.lookup("docs/eicar.txt")) != nil {
		t.Fatal("a rejected asset was published to the catalog")
	}
}

// eicar is assembled at run time so the source file itself does not
// trip a virus scanner.
func eicar() []byte {
	parts := []string{`X5O!P%@AP[4\PZX54(P^)7CC)7}$`, `EICAR-STANDARD-ANTIVIRUS-TEST-FILE!`, `$H+H*`}
	return []byte(strings.Join(parts, ""))
}

func (c *client) checkVersions(path, first, second string) {
	t := c.s.t
	t.Helper()
	object := c.lookup(path)
	versions := c.must(http.StatusOK, http.MethodGet, "/api/v1/catalog/objects/"+str(t, object, "id")+"/versions", nil)
	list := versions["versions"].([]any)
	if len(list) != 2 {
		t.Fatalf("%s has %d versions, want 2", path, len(list))
	}
	byAsset := map[string]map[string]any{}
	for _, v := range list {
		version := v.(map[string]any)
		byAsset[str(t, version, "asset_id")] = version
	}
	if byAsset[first]["current"] != false || byAsset[second]["current"] != true ||
		num(t, byAsset[first], "version") != 1 || num(t, byAsset[second], "version") != 2 {
		t.Fatalf("versions of %s: %v", path, list)
	}
}

// ─── Datasets ─────────────────────────────────────────────────────────

func (c *client) checkDataset(run string) {
	t := c.s.t
	t.Helper()
	dataset := c.must(http.StatusCreated, http.MethodPost, "/api/v1/datasets", map[string]any{"name": "e2e-" + run})
	id := str(t, dataset, "id")

	manifest := `{"path":"docs/hello.txt","label":"greeting","split":"train"}` + "\n" +
		`{"path":"data/big.txt","label":"bulk","split":"test"}` + "\n" +
		`{"path":"docs/missing.txt","label":"none","split":"train"}` + "\n"
	r := c.send(http.MethodPost, "/api/v1/datasets/"+id+"/manifests", "application/x-ndjson", []byte(manifest),
		"Idempotency-Key", "e2e-"+run)
	if r.status != http.StatusAccepted {
		t.Fatalf("import manifest: HTTP %d %s", r.status, r.body)
	}
	job := str(t, r.json(t), "id")
	var imported map[string]any
	eventually(t, time.Minute, "the manifest import", func() bool {
		imported = c.must(http.StatusOK, http.MethodGet, "/api/v1/datasets/"+id+"/manifests/"+job, nil)
		return imported["state"] == "COMPLETED" || imported["state"] == "FAILED"
	})
	progress := obj(t, imported, "progress")
	if imported["state"] != "COMPLETED" || num(t, progress, "members_added") != 2 || num(t, progress, "lines_rejected") != 1 {
		t.Fatalf("import: %v", imported)
	}
	errs := c.must(http.StatusOK, http.MethodGet, "/api/v1/datasets/"+id+"/manifests/"+job+"/errors", nil)
	if list := errs["errors"].([]any); len(list) != 1 || num(t, list[0].(map[string]any), "line") != 3 {
		t.Fatalf("import errors: %v", errs)
	}

	snapshot := c.must(http.StatusCreated, http.MethodPost, "/api/v1/datasets/"+id+"/snapshots", map[string]any{"idempotency_key": "v1-" + run})
	versionID := str(t, snapshot, "id")
	if snapshot["status"] != "FINALIZED" || num(t, snapshot, "member_count") != 2 {
		t.Fatalf("snapshot: %v", snapshot)
	}

	report := c.runValidation("/api/v1/datasets/"+id+"/validations", map[string]any{"mode": "FULL"})
	c.must(http.StatusOK, http.MethodGet, "/api/v1/validation-reports/"+report, nil)
	c.must(http.StatusOK, http.MethodGet, "/api/v1/validation-reports/"+report+"/findings", nil)

	readiness := c.runValidation("/api/v1/datasets/"+id+"/readiness-assessments", map[string]any{"profile_id": "builtin:general"})
	reports := c.must(http.StatusOK, http.MethodGet, "/api/v1/datasets/"+id+"/readiness-reports", nil)
	if !strings.Contains(fmt.Sprint(reports["reports"]), readiness) {
		t.Fatalf("readiness report %s is not listed: %v", readiness, reports)
	}

	// Upstream follows the edges: the version was checked by its report, the
	// dataset contains the version and was imported from the manifest.
	// Downstream goes back from the version to the dataset holding it.
	lineage := c.must(http.StatusOK, http.MethodGet, "/api/v1/dataset-versions/"+versionID+"/lineage", nil)
	edges := fmt.Sprint(lineage["edges"])
	if !strings.Contains(edges, report) || !strings.Contains(edges, "VALIDATED_BY") {
		t.Fatalf("lineage of %s: %v", versionID, lineage)
	}
	lineage = c.must(http.StatusOK, http.MethodGet, "/api/v1/datasets/"+id+"/lineage", nil)
	edges = fmt.Sprint(lineage["edges"])
	if !strings.Contains(edges, versionID) || !strings.Contains(edges, "IMPORTED_FROM") || !strings.Contains(edges, job) {
		t.Fatalf("dataset lineage: %v", lineage)
	}
	lineage = c.must(http.StatusOK, http.MethodGet, "/api/v1/dataset-versions/"+versionID+"/lineage?direction=downstream", nil)
	edges = fmt.Sprint(lineage["edges"])
	if !strings.Contains(edges, id) || !strings.Contains(edges, "CONTAINS") {
		t.Fatalf("downstream lineage of %s: %v", versionID, lineage)
	}
}

// runValidation starts a validation or readiness job, waits for it to end
// and returns its report ID.
func (c *client) runValidation(path string, body map[string]any) string {
	t := c.s.t
	t.Helper()
	started := c.must(http.StatusAccepted, http.MethodPost, path, body)
	id := str(t, started, "id")
	var job map[string]any
	eventually(t, 2*time.Minute, "validation job "+id, func() bool {
		job = c.must(http.StatusOK, http.MethodGet, "/api/v1/validation-jobs/"+id, nil)
		switch job["state"] {
		case "COMPLETED", "COMPLETED_WITH_WARNINGS", "FAILED", "CANCELLED":
			return true
		}
		return false
	})
	report, _ := job["report_id"].(string)
	if report == "" {
		t.Fatalf("validation job ended without a report: %v", job)
	}
	return report
}

// ─── Isolation and secrets ────────────────────────────────────────────

// checkIsolation has other try to reach owner's connection and asset.
func checkIsolation(owner, other *client, connection, asset string) {
	t := owner.s.t
	t.Helper()
	for _, path := range []string{
		"/api/v1/storage-connections/" + connection,
		"/api/v1/assets/" + asset,
		"/api/v1/assets/" + asset + "/content",
		"/api/v1/assets/" + asset + "/lifecycle",
	} {
		if r := other.do(http.MethodGet, path, nil); r.status != http.StatusNotFound {
			t.Fatalf("another tenant's GET %s: HTTP %d, want 404", path, r.status)
		}
	}
	if r := other.do(http.MethodPost, "/api/v1/assets/"+asset+"/download-url", map[string]any{}); r.status != http.StatusNotFound {
		t.Fatalf("another tenant's download URL: HTTP %d, want 404", r.status)
	}
	if r := other.do(http.MethodPost, "/api/v1/storage-connections/"+connection+"/test", map[string]any{}); r.status != http.StatusNotFound {
		t.Fatalf("another tenant's connection test: HTTP %d, want 404", r.status)
	}
	if r := other.do(http.MethodPost, "/api/v1/uploads", uploadBody("docs/stolen.txt", []byte("x"), connection)); r.status < 400 {
		t.Fatalf("an upload to another tenant's connection was accepted: HTTP %d", r.status)
	}
	// Naming the other organization does not switch to it.
	intruder := &client{s: other.s, token: other.token, org: owner.org}
	if r := intruder.do(http.MethodGet, "/api/v1/assets/"+asset, nil); r.status != http.StatusForbidden {
		t.Fatalf("a non-member's X-Organization-ID: HTTP %d, want 403", r.status)
	}
}

// checkNoSecrets searches process output, the processing worker's metrics
// and every table for the secrets in plain text.
func (s *stack) checkNoSecrets(db *pgx.Conn) {
	t := s.t
	t.Helper()
	if secret := s.leakedInOutput(); secret != "" {
		t.Fatal("a secret appears in a process's output")
	}
	resp, err := http.Get("http://" + s.metrics + "/debug/vars")
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	vars, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if firstSecret(s.secrets, string(vars)) != "" {
		t.Fatal("a secret appears in the metrics")
	}

	ctx := context.Background()
	rows, err := db.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_type = 'BASE TABLE'`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range s.secrets {
		for _, table := range tables {
			var n int
			query := fmt.Sprintf(`SELECT count(*) FROM %s AS r WHERE strpos(r::text, $1) > 0`, pgx.Identifier{table}.Sanitize())
			if err := db.QueryRow(ctx, query, secret).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n > 0 {
				t.Fatalf("a secret appears in plain text in %s (%d rows)", table, n)
			}
		}
		// Sealed credentials are bytes: look for the raw secret in them too.
		var n int
		err := db.QueryRow(ctx, `SELECT count(*) FROM storage_connections
			WHERE position(convert_to($1, 'UTF8') IN coalesce(credential_ciphertext, ''::bytea)) > 0`, secret).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			t.Fatal("a credential is stored unencrypted")
		}
	}
	t.Logf("no secret in output, metrics, responses or %d tables (%d secrets checked)", len(tables), len(s.secrets))
}

// ─── Helpers ──────────────────────────────────────────────────────────

func readyKey(org, asset string) string { return "assets/" + org + "/" + asset }

// text is size bytes of readable lines, so content checks see plain text.
func text(seed string, size int) []byte {
	line := seed + " 0123456789 abcdefghijklmnopqrstuvwxyz\n"
	return []byte(strings.Repeat(line, size/len(line)+1)[:size])
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func str(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key].(string)
	if !ok || v == "" {
		t.Fatalf("no %q in %v", key, m)
	}
	return v
}

func num(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	v, ok := m[key].(float64)
	if !ok {
		t.Fatalf("no number %q in %v", key, m)
	}
	return v
}

func obj(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("no object %q in %v", key, m)
	}
	return v
}
