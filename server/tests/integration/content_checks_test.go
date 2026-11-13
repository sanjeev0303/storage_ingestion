package integration

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/service/processing"
	"github.com/sanjeev0303/object-storage-platform/internal/service/promotion"
)

// signatureScanner stands in for ClamAV with signatures that can be
// updated: it reports a version and, after an update, can know a file it
// passed before.
type signatureScanner struct {
	mu      sync.Mutex
	version string
	bad     []byte
}

func (s *signatureScanner) Name() string { return "it-signatures" }
func (s *signatureScanner) Ping(context.Context) error { return nil }
func (s *signatureScanner) Version(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version, nil
}
func (s *signatureScanner) Scan(_ context.Context, r io.Reader) interfaces.ScanResult {
	data, err := io.ReadAll(r)
	if err != nil {
		return interfaces.ScanResult{Outcome: model.ScanError, Detail: err.Error()}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bad != nil && bytes.Contains(data, s.bad) {
		return interfaces.ScanResult{Outcome: model.ScanInfected, Signature: "IT.Test.Signature"}
	}
	return interfaces.ScanResult{Outcome: model.ScanClean}
}

func (s *signatureScanner) update(version string, bad []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version, s.bad = version, bad
}

// The quarantine gate's content checks against the local stack: findings
// are recorded and flag or reject by the tenant's policy, the readiness
// evidence sees them, and a signature update re-scans what was published.
func TestLocalStackContentChecksAndRescan(t *testing.T) {
	stack := openContentStack(t)
	ctx := context.Background()
	checks := postgres.NewContentCheckRepository(stack.env.db)
	scanner := &signatureScanner{version: "sigs-1"}
	assets := postgres.NewAssetRepository(stack.env.db)
	promoter, err := promotion.NewService(assets, stack.provider, promotion.Config{
		SourceBucket: stack.bucket, DestinationBucket: stack.bucket, Prefix: promotionPrefix,
	})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := processing.NewService(processing.Dependencies{
		Jobs: stack.env.jobs, Assets: assets, Uploads: stack.env.uploads,
		Quarantine: postgres.NewQuarantineRepository(stack.env.db), Events: postgres.NewProcessingEventRepository(stack.env.db),
		Policies: postgres.NewContentPolicyRepository(stack.env.db), Verifier: stack.multipart, Storage: stack.provider,
		Validators: []interfaces.FileValidator{processing.BasicValidator{}, processing.TypeValidator{}, processing.ContentInspector{
			Limits:   processing.ArchiveLimits{MaxArchiveBytes: 1 << 30, MaxEntries: 1000, MaxUncompressedBytes: 1 << 30, MaxRatio: 100},
			MaxBytes: 1 << 30, SecretScanBytes: 1 << 20,
		}},
		Scanner: scanner, Promoter: promoter, Checks: checks, Packs: postgres.NewPackRepository(stack.env.db),
	}, processing.Config{
		Owner: "it-content", Bucket: stack.bucket, ReadyBucket: stack.bucket, RescanBatch: 50,
		BatchSize: 100, Concurrency: 2, Lease: 2 * time.Minute,
		Retry:      retry.Policy{BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond, MaxAttempts: 3},
		Policy:     model.ContentPolicy{MaxSizeBytes: 1 << 30, UnknownTypeAction: "allow"},
		ScanPolicy: processing.ScanPolicyHold, ScanTimeout: time.Minute, QuarantineRetention: time.Hour,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	findings := func(assetID string) []model.ContentFinding {
		t.Helper()
		out, err := checks.ListFindings(ctx, stack.env.tenant, assetID)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	coverage := func(jobID string) string {
		var c *string
		if err := stack.env.db.Pool.QueryRow(ctx, `SELECT content_checks FROM processing_jobs WHERE id = $1`, jobID).Scan(&c); err != nil || c == nil {
			return ""
		}
		return *c
	}

	// A leaked key flags the file by default; it is still published.
	keyFile := []byte("region=eu-west-1\naws_access_key_id=AKIAIOSFODNN7EXAMPLE\n")
	flagged := stack.upload(t, "settings.env", "text/plain", keyFile)
	// A pickle that would run os.system is rejected by default.
	evil := stack.upload(t, "model.pkl", "application/octet-stream", []byte("\x80\x02cos\nsystem\nq\x00X\x02\x00\x00\x00lsq\x01\x85q\x02Rq\x03."))
	runCycle(t, svc)

	asset, job := stack.assetFor(t, flagged)
	if asset.Status != model.AssetStatusAvailable {
		t.Fatalf("flagged file: %s", asset.Status)
	}
	if f := findings(asset.ID); len(f) != 1 || f[0].Code != "secret.aws_access_key" || f[0].Action != "flag" || f[0].Source != "secrets" {
		t.Fatalf("flagged findings: %+v", f)
	}
	if coverage(job.ID) != "FULL" {
		t.Fatalf("coverage: %q", coverage(job.ID))
	}
	rejected, _ := stack.assetFor(t, evil)
	if rejected.Status != model.AssetStatusRejected || rejected.RejectionReason != model.RejectContentCheck {
		t.Fatalf("unsafe pickle: %s %s", rejected.Status, rejected.RejectionReason)
	}
	if f := findings(rejected.ID); len(f) != 1 || f[0].Code != "model.unsafe_pickle" || f[0].Action != "reject" {
		t.Fatalf("pickle findings: %+v", f)
	}

	// The tenant makes secrets reject.
	if err := checks.PutPolicy(ctx, stack.env.tenant, model.ContentPolicy{FindingActions: map[string]string{"secret": "reject"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	strict := stack.upload(t, "again.env", "text/plain", []byte("token AKIAIOSFODNN7EXAMPL2\n"))
	runCycle(t, svc)
	if a, _ := stack.assetFor(t, strict); a.Status != model.AssetStatusRejected {
		t.Fatalf("with secrets set to reject: %s", a.Status)
	}
	if _, err := stack.env.db.Pool.Exec(ctx, `DELETE FROM tenant_content_policies WHERE tenant_id = $1`, stack.env.tenant); err != nil {
		t.Fatal(err)
	}

	// New signatures know the published file: the next cycle re-scans it
	// and blocks it, with a finding that says why. It stays AVAILABLE in its
	// lifecycle (catalog and usage unchanged) but nothing is served.
	scanner.update("sigs-2", []byte("AKIAIOSFODNN7EXAMPLE"))
	stats := runCycle(t, svc)
	if stats.Infected < 1 {
		t.Fatalf("re-scan cycle: %+v", stats)
	}
	after, _ := stack.assetFor(t, flagged)
	if after.Status != model.AssetStatusAvailable || after.BlockedReason != model.RejectMalwareDetected {
		t.Fatalf("after the re-scan: %s blocked=%q", after.Status, after.BlockedReason)
	}
	var rescanFinding bool
	for _, f := range findings(asset.ID) {
		rescanFinding = rescanFinding || (f.Code == "malware.detected_on_rescan" && f.Action == "reject" && f.Source == "rescan")
	}
	if !rescanFinding {
		t.Fatalf("no re-scan finding: %+v", findings(asset.ID))
	}
	var scanResult string
	stack.env.db.Pool.QueryRow(ctx, `SELECT scan_result FROM processing_jobs WHERE id = $1`, job.ID).Scan(&scanResult)
	if scanResult != model.ScanInfected {
		t.Fatalf("job scan result after the re-scan: %s", scanResult)
	}
	// Nothing left to re-scan with these signatures.
	if again := runCycle(t, svc); again.Rescanned != 0 {
		t.Fatalf("re-scanned twice: %+v", again)
	}

	// An archived asset cannot be read, so it waits for newer signatures
	// until it is restored instead of failing a re-scan every hour.
	cold := stack.upload(t, "cold.txt", "text/plain", []byte("nothing to see here\n"))
	runCycle(t, svc)
	coldAsset, coldJob := stack.assetFor(t, cold)
	if _, err := stack.env.db.Pool.Exec(ctx, `UPDATE assets SET storage_tier = 'ARCHIVE' WHERE id = $1`, coldAsset.ID); err != nil {
		t.Fatal(err)
	}
	scanner.update("sigs-3", []byte("AKIAIOSFODNN7EXAMPLE"))
	if archived := runCycle(t, svc); archived.Rescanned != 0 {
		t.Fatalf("re-scanned an archived asset: %+v", archived)
	}
	var claimed *time.Time
	stack.env.db.Pool.QueryRow(ctx, `SELECT rescanned_at FROM processing_jobs WHERE id = $1`, coldJob.ID).Scan(&claimed)
	if claimed != nil {
		t.Fatalf("an archived asset was claimed for a re-scan at %s", claimed)
	}
	if _, err := stack.env.db.Pool.Exec(ctx, `UPDATE assets SET readable_until = now() + interval '1 day' WHERE id = $1`, coldAsset.ID); err != nil {
		t.Fatal(err)
	}
	if restored := runCycle(t, svc); restored.Rescanned != 1 {
		t.Fatalf("a restored asset with old signatures: %+v", restored)
	}

	// Blast radius: the asset and its downloads, by its bytes.
	radius, err := checks.BlastRadius(ctx, stack.env.tenant, asset.ID, 100)
	if err != nil || radius.ChecksumSHA256 != asset.ChecksumSHA256 || len(radius.Assets) != 1 || radius.Assets[0].AssetID != asset.ID {
		t.Fatalf("blast radius: %+v %v", radius, err)
	}
}

// The API: findings, blast radius and the organization's content policy,
// each behind its permission.
func TestHTTPContentChecks(t *testing.T) {
	s := openSecurityStack(t, nil)
	acme := s.team(t, "checks-a")
	globex := s.team(t, "checks-b")
	owner := s.signIn(t, acme.ownerUser.Email, acme.ownerPW).AccessToken
	dev := s.signIn(t, acme.devUser.Email, acme.devPW).AccessToken
	auditor := s.signIn(t, acme.auditorUser.Email, acme.auditorPW).AccessToken
	outsider := s.signIn(t, globex.ownerUser.Email, globex.ownerPW).AccessToken
	asset := s.availableAsset(t, acme.developer, "plain.txt", []byte("nothing secret here\n"))

	var f struct {
		Findings []model.ContentFinding `json:"findings"`
	}
	s.expect(t, dev, "", http.MethodGet, "/api/v1/assets/"+asset.ID+"/findings", "", http.StatusOK, &f)
	if f.Findings == nil || len(f.Findings) != 0 {
		t.Fatalf("findings: %+v", f)
	}
	s.expect(t, outsider, "", http.MethodGet, "/api/v1/assets/"+asset.ID+"/findings", "", http.StatusNotFound, nil)

	var radius model.BlastRadius
	s.expect(t, auditor, "", http.MethodGet, "/api/v1/assets/"+asset.ID+"/blast-radius", "", http.StatusOK, &radius)
	if radius.ChecksumSHA256 != asset.ChecksumSHA256 || len(radius.Assets) != 1 {
		t.Fatalf("blast radius: %+v", radius)
	}
	s.expect(t, dev, "", http.MethodGet, "/api/v1/assets/"+asset.ID+"/blast-radius", "", http.StatusForbidden, nil)

	// A block is cleared only by someone who may release quarantine, with a
	// reason, once; the clearing is audited with the block it lifted.
	if err := postgres.NewContentCheckRepository(s.db).BlockAsset(context.Background(), acme.org.ID, asset.ID, model.RejectMalwareDetected, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	unblock := "/api/v1/assets/" + asset.ID + "/unblock"
	s.expect(t, dev, "", http.MethodPost, unblock, `{"reason":"false positive"}`, http.StatusForbidden, nil)
	s.expect(t, outsider, "", http.MethodPost, unblock, `{"reason":"false positive"}`, http.StatusNotFound, nil)
	s.expect(t, owner, "", http.MethodPost, unblock, `{"reason":"  "}`, http.StatusBadRequest, nil)
	s.expect(t, owner, "", http.MethodPost, unblock, `{"reason":"false positive, checked by hand"}`, http.StatusOK, nil)
	s.expect(t, owner, "", http.MethodPost, unblock, `{"reason":"again"}`, http.StatusConflict, nil)
	cleared := 0
	for _, ev := range s.auditEvents(t, acme.org.ID, "asset.unblock") {
		if ev.Details["blocked_reason"] == model.RejectMalwareDetected {
			cleared++
		}
	}
	if cleared != 1 {
		t.Fatalf("the clearing is audited once with the block it lifted: %d", cleared)
	}

	var policy struct {
		Policy struct {
			DeniedTypes    []string          `json:"denied_types"`
			FindingActions map[string]string `json:"finding_actions"`
		} `json:"policy"`
		Families []string `json:"finding_families"`
	}
	s.expect(t, dev, "", http.MethodGet, "/api/v1/content-policy", "", http.StatusOK, &policy)
	if len(policy.Families) == 0 || policy.Policy.FindingActions == nil {
		t.Fatalf("policy: %+v", policy)
	}
	body := `{"denied_types":["application/x-tar"],"finding_actions":{"secret":"reject","model.pickle_imports":"allow"}}`
	s.expect(t, dev, "", http.MethodPut, "/api/v1/content-policy", body, http.StatusForbidden, nil)
	s.expect(t, owner, "", http.MethodPut, "/api/v1/content-policy", body, http.StatusOK, nil)
	s.expect(t, dev, "", http.MethodGet, "/api/v1/content-policy", "", http.StatusOK, &policy)
	if policy.Policy.FindingActions["secret"] != "reject" || policy.Policy.DeniedTypes[0] != "application/x-tar" {
		t.Fatalf("stored policy: %+v", policy)
	}
	s.expect(t, owner, "", http.MethodPut, "/api/v1/content-policy", `{"finding_actions":{"malware":"allow"}}`, http.StatusBadRequest, nil)
	s.expect(t, owner, "", http.MethodPut, "/api/v1/content-policy", `{"denied_types":["not a type"]}`, http.StatusBadRequest, nil)
}
