package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/request"
	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	memstorage "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/ratelimit"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/server"
	"github.com/sanjeev0303/object-storage-platform/internal/server/middleware"
	assetservice "github.com/sanjeev0303/object-storage-platform/internal/service/asset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
	"github.com/sanjeev0303/object-storage-platform/internal/service/catalog"
	datasetservice "github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/delivery"
	"github.com/sanjeev0303/object-storage-platform/internal/service/events"
	"github.com/sanjeev0303/object-storage-platform/internal/service/identity"
	"github.com/sanjeev0303/object-storage-platform/internal/service/ingestion"
	"github.com/sanjeev0303/object-storage-platform/internal/service/processing"
	"github.com/sanjeev0303/object-storage-platform/internal/service/quota"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

// Phase 8 against PostgreSQL: TEST_DATABASE_URL with every migration
// through 000012 applied. Accounts, organizations and their data are
// removed after each test. Events in the platform audit chain stay: the
// chain is append-only and shared, and removing events from its middle
// would break it.

// member is an organization member with the built-in role roleID, for
// tests that call services directly with a principal they construct.
func member(tenantID, userID, roleID string) model.Principal {
	role, _ := model.BuiltInRole(roleID)
	return model.Principal{TenantID: tenantID, UserID: userID, RoleID: role.ID, Permissions: role.Permissions}
}

// operator is the platform administrator the identity-admin CLI acts as.
var operator = model.Principal{UserID: "it-operator", Service: true, PlatformRole: model.PlatformAdmin}

type securityEnv struct {
	db        *postgres.DB
	sec       config.Security
	logs      *lockedBuffer
	log       *slog.Logger
	limiter   *ratelimit.Limiter
	auditRepo *postgres.AuditRepository
	audit     *audit.Service
	users     *postgres.IdentityRepository
	tenancy   *postgres.TenancyRepository
	quotaRepo *postgres.QuotaRepository
	quotas    *quota.Service
	identity  *identity.Service
	// run makes this test's emails and slugs unique; cleanup finds
	// everything the test created by it.
	run    string
	client identity.Client
}

// openSecurityEnv builds the identity, quota and audit services over the
// test database. settings override security environment variables for
// this test only.
func openSecurityEnv(t *testing.T, settings map[string]string) *securityEnv {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set; Phase 8 PostgreSQL tests were not run")
	}
	// The weakest Argon2id parameters the configuration accepts keep the
	// many sign-ins fast; generous administrative limits keep set-up from
	// tripping them.
	defaults := map[string]string{
		"AUTH_ARGON2_MEMORY_KIB": "19456", "AUTH_ARGON2_TIME": "2", "AUTH_ARGON2_THREADS": "1",
		"RATE_LIMIT_CREDENTIALS": "1000/1m", "RATE_LIMIT_ADMIN": "10000/1m", "RATE_LIMIT_MEMBERSHIP": "1000/1m",
	}
	for k, v := range defaults {
		t.Setenv(k, v)
	}
	for k, v := range settings {
		t.Setenv(k, v)
	}
	sec, err := config.LoadSecurity()
	if err != nil {
		t.Fatalf("security configuration: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := postgres.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(db.Close)

	logs := &lockedBuffer{}
	e := &securityEnv{
		db: db, sec: sec, logs: logs, log: slog.New(slog.NewJSONHandler(logs, nil)),
		limiter: ratelimit.New(), auditRepo: postgres.NewAuditRepository(db),
		users: postgres.NewIdentityRepository(db), tenancy: postgres.NewTenancyRepository(db),
		quotaRepo: postgres.NewQuotaRepository(db),
		run:       fmt.Sprintf("r%d", time.Now().UnixNano()),
		client:    identity.Client{IP: "192.0.2.10", UserAgent: "integration-test"},
	}
	e.audit = audit.NewService(e.auditRepo, e.log, nil, sec.Audit.Retention)
	e.quotas = quota.NewService(e.quotaRepo, sec.Quotas, e.audit, e.log)
	e.identity, err = identity.NewService(identity.Dependencies{
		Users: e.users, Tenancy: e.tenancy, Audit: e.audit, Limiter: e.limiter, Log: e.log,
	}, sec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.cleanup(t) })
	return e
}

// email is a unique address in this test's domain.
func (e *securityEnv) email(name string) string {
	return name + "@" + e.run + ".example.test"
}

// account provisions an active account as an administrator does: created
// PENDING, then activated by its owner with the one-time token.
func (e *securityEnv) account(t *testing.T, name string) (model.User, string) {
	t.Helper()
	ctx := context.Background()
	_, activation, err := e.identity.CreateUser(ctx, operator, e.email(name), name, "")
	if err != nil {
		t.Fatalf("create account %s: %v", name, err)
	}
	pw := "correct horse " + name + " " + e.run
	user, err := e.identity.Activate(ctx, activation.Token, pw, e.client)
	if err != nil {
		t.Fatalf("activate %s: %v", name, err)
	}
	return user, pw
}

// organization creates an organization with owner as its first owner.
func (e *securityEnv) organization(t *testing.T, name string, owner model.User) model.Organization {
	t.Helper()
	org, err := e.identity.CreateOrganization(context.Background(), operator, name, name+"-"+e.run, owner.ID)
	if err != nil {
		t.Fatalf("create organization %s: %v", name, err)
	}
	return org
}

// as is user's current principal in org, resolved like a request's.
func (e *securityEnv) as(t *testing.T, orgID string, user model.User) model.Principal {
	t.Helper()
	p, err := e.identity.PrincipalFor(context.Background(), orgID, user.ID)
	if err != nil {
		t.Fatalf("principal of %s in %s: %v", user.Email, orgID, err)
	}
	return p
}

// join adds user to inviter's organization with roleID: an invitation,
// accepted by the user signed in.
func (e *securityEnv) join(t *testing.T, inviter model.Principal, user model.User, roleID string) model.Principal {
	t.Helper()
	ctx := context.Background()
	inv, err := e.identity.Invite(ctx, inviter, user.Email, roleID)
	if err != nil {
		t.Fatalf("invite %s as %s: %v", user.Email, roleID, err)
	}
	if _, err := e.identity.AcceptInvitation(ctx, inv.Token, &model.Principal{UserID: user.ID}, nil, e.client); err != nil {
		t.Fatalf("accept invitation: %v", err)
	}
	return e.as(t, inviter.TenantID, user)
}

// team is an organization with an owner and a member of each other
// built-in role.
type team struct {
	org                                model.Organization
	owner, admin, developer, auditor   model.Principal
	ownerUser, adminUser, devUser      model.User
	auditorUser                        model.User
	ownerPW, adminPW, devPW, auditorPW string
}

func (e *securityEnv) team(t *testing.T, name string) team {
	t.Helper()
	var tm team
	tm.ownerUser, tm.ownerPW = e.account(t, name+"-owner")
	tm.org = e.organization(t, name, tm.ownerUser)
	tm.owner = e.as(t, tm.org.ID, tm.ownerUser)
	tm.adminUser, tm.adminPW = e.account(t, name+"-admin")
	tm.admin = e.join(t, tm.owner, tm.adminUser, model.RoleAdmin)
	tm.devUser, tm.devPW = e.account(t, name+"-dev")
	tm.developer = e.join(t, tm.owner, tm.devUser, model.RoleDeveloper)
	tm.auditorUser, tm.auditorPW = e.account(t, name+"-auditor")
	tm.auditor = e.join(t, tm.owner, tm.auditorUser, model.RoleAuditor)
	return tm
}

func (e *securityEnv) login(t *testing.T, email, pw string) identity.Tokens {
	t.Helper()
	tokens, err := e.identity.Login(context.Background(), email, pw, e.client)
	if err != nil {
		t.Fatalf("login %s: %v", email, err)
	}
	return tokens
}

// auditEvents returns chain's events with action, oldest first.
func (e *securityEnv) auditEvents(t *testing.T, chain, action string) []model.AuditEvent {
	t.Helper()
	rows, err := e.db.Pool.Query(context.Background(), `
		SELECT id, seq, actor_type, actor_id, outcome, reason, resource_id, details::text
		FROM audit_events WHERE chain = $1 AND action = $2 ORDER BY seq
	`, chain, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []model.AuditEvent
	for rows.Next() {
		var ev model.AuditEvent
		var details string
		if err := rows.Scan(&ev.ID, &ev.Seq, &ev.ActorType, &ev.ActorID, &ev.Outcome, &ev.Reason, &ev.ResourceID, &details); err != nil {
			t.Fatal(err)
		}
		ev.Action, ev.Chain = action, chain
		if err := json.Unmarshal([]byte(details), &ev.Details); err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

// uploads is the single-request upload service, quota-guarded and
// audited, over the test database and in-memory storage.
func (e *securityEnv) uploads() *uploadservice.Service {
	return uploadservice.NewService(postgres.NewUploadRepository(e.db), memstorage.NewObjectStorage(), "security-it", 1<<30, time.Hour).
		Guard(e.quotas, e.audit)
}

// orgIDs lists the organizations this test created.
func (e *securityEnv) orgIDs(ctx context.Context) []string {
	rows, err := e.db.Pool.Query(ctx, `SELECT id FROM organizations WHERE slug LIKE '%-' || $1`, e.run)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// tenantTables hold tenant data, children before the tables they
// reference.
var tenantTables = []string{
	"validation_findings", "validation_reports", "validation_jobs", "lineage_edges", "lineage_nodes",
	"dataset_manifest_imports", "dataset_version_members", "dataset_versions", "dataset_members", "validation_profiles",
	"download_tokens", "delivery_audit_events", "deletion_operations", "lifecycle_events", "legal_holds",
	"retention_policies", "orphan_candidates", "quarantine_records", "processing_events", "processing_jobs",
	"ingestion_entries", "ingestion_jobs", "datasets", "ingestion_tenant_usage", "assets", "reconciliation_findings",
	"reconciliation_tasks", "upload_parts", "upload_sessions", "outbox_events", "event_inbox", "dead_letters",
	"event_discrepancies", "tenant_content_policies", "tenant_quotas", "tenant_usage",
}

// cleanTenantData removes the tenants' data. Catalog objects point at
// their current versions and assets at their objects, so the pointers go
// first and the objects last.
func cleanTenantData(ctx context.Context, db *postgres.DB, tenants []string) error {
	if _, err := db.Pool.Exec(ctx, `
		UPDATE catalog_objects SET current_asset_id = NULL, current_version = NULL WHERE tenant_id = ANY($1)
	`, tenants); err != nil {
		return fmt.Errorf("clear current versions: %w", err)
	}
	for _, table := range tenantTables {
		var err error
		if table == "upload_parts" {
			_, err = db.Pool.Exec(ctx, `DELETE FROM upload_parts WHERE upload_id IN (SELECT id FROM upload_sessions WHERE tenant_id = ANY($1))`, tenants)
		} else {
			_, err = db.Pool.Exec(ctx, `DELETE FROM `+table+` WHERE tenant_id = ANY($1)`, tenants)
		}
		if err != nil {
			return fmt.Errorf("clean %s: %w", table, err)
		}
	}
	if _, err := db.Pool.Exec(ctx, `DELETE FROM catalog_objects WHERE tenant_id = ANY($1)`, tenants); err != nil {
		return fmt.Errorf("clean catalog_objects: %w", err)
	}
	// Storage connections last: everything above may be bound to them, and
	// an organization's default points at one.
	if _, err := db.Pool.Exec(ctx, `UPDATE organizations SET default_storage_connection_id = 'platform' WHERE id = ANY($1)`, tenants); err != nil {
		return fmt.Errorf("reset default connections: %w", err)
	}
	if _, err := db.Pool.Exec(ctx, `DELETE FROM inventory_scans WHERE storage_connection_id IN (SELECT id FROM storage_connections WHERE tenant_id = ANY($1))`, tenants); err != nil {
		return fmt.Errorf("clean inventory scans: %w", err)
	}
	if _, err := db.Pool.Exec(ctx, `DELETE FROM storage_connections WHERE tenant_id = ANY($1)`, tenants); err != nil {
		return fmt.Errorf("clean storage_connections: %w", err)
	}
	return nil
}

func (e *securityEnv) cleanup(t *testing.T) {
	ctx := context.Background()
	orgs := e.orgIDs(ctx)
	if err := cleanTenantData(ctx, e.db, orgs); err != nil {
		t.Errorf("clean tenant data: %v", err)
	}
	domain := "%@" + e.run + ".example.test"
	for _, q := range []string{
		`DELETE FROM invitations WHERE org_id = ANY($1)`,
		`DELETE FROM memberships WHERE org_id = ANY($1)`,
		`DELETE FROM org_roles WHERE org_id = ANY($1)`,
		`DELETE FROM organizations WHERE id = ANY($1)`,
	} {
		if _, err := e.db.Pool.Exec(ctx, q, orgs); err != nil {
			t.Errorf("clean organizations: %v", err)
		}
	}
	for _, q := range []string{
		`DELETE FROM auth_credential_tokens WHERE user_id IN (SELECT id FROM users WHERE email LIKE $1)`,
		`DELETE FROM auth_sessions WHERE user_id IN (SELECT id FROM users WHERE email LIKE $1)`,
		`DELETE FROM memberships WHERE user_id IN (SELECT id FROM users WHERE email LIKE $1)`,
		`DELETE FROM users WHERE email LIKE $1`,
	} {
		if _, err := e.db.Pool.Exec(ctx, q, domain); err != nil {
			t.Errorf("clean accounts: %v", err)
		}
	}
	// The organizations' audit chains go whole, through the one door the
	// append-only trigger leaves open.
	err := withPurge(ctx, e.db, func(tx pgx.Tx) error {
		for _, q := range []string{
			`DELETE FROM audit_events WHERE chain = ANY($1)`,
			`DELETE FROM audit_chain_heads WHERE chain = ANY($1)`,
			`DELETE FROM audit_checkpoints WHERE chain = ANY($1)`,
		} {
			if _, err := tx.Exec(ctx, q, orgs); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Errorf("clean audit chains: %v", err)
	}
}

// ─── The API over HTTP ────────────────────────────────────────────────

// securityStack is the whole API through its real router: real sign-in,
// organizations and permissions, with uploads, ingestion, delivery and
// quarantine on the ingestion test environment's in-memory storage.
type securityStack struct {
	*securityEnv
	ie         *ingestEnv
	delivery   *delivery.Service
	lifecycle  *assetservice.Service
	catalog    *catalog.Service
	reconciler *catalog.Reconciler
	catalogDB  *postgres.CatalogRepository
	dead       *postgres.DeadLetterRepository
	api        *httptest.Server
	// datasets is the API's dataset service; worker builds the worker's.
	datasets  *datasetservice.Service
	datasetDB *postgres.DatasetRepository
}

// datasetConfig is the dataset limits tests run with: the defaults, with
// small batches so a few members make several.
func datasetConfig(t *testing.T) config.Datasets {
	t.Helper()
	cfg, err := config.LoadDatasets()
	if err != nil {
		t.Fatal(err)
	}
	cfg.ImportBatch, cfg.ValidationBatch, cfg.MaxBulkMembers = 4, 3, 100
	cfg.BackoffBase, cfg.BackoffMax = time.Millisecond, 5*time.Millisecond
	return cfg
}

// newDatasetService builds a dataset service over the stack's database and
// in-memory storage; change adjusts its limits.
func (s *securityStack) newDatasetService(t *testing.T, change func(*config.Datasets)) *datasetservice.Service {
	t.Helper()
	cfg := datasetConfig(t)
	if change != nil {
		change(&cfg)
	}
	svc, err := datasetservice.NewService(datasetservice.Dependencies{
		Repo: s.datasetDB, Manifests: s.ie.storage, ManifestBucket: s.ie.bucket,
		Content: s.ie.storage, ContentBucket: s.ie.bucket, Quotas: s.quotas, Audit: s.audit, Log: s.log,
	}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func openSecurityStack(t *testing.T, settings map[string]string) *securityStack {
	t.Helper()
	e := openSecurityEnv(t, settings)
	ie := openIngestEnv(t)
	ie.single.Guard(e.quotas, e.audit)
	ie.multi.Guard(e.quotas, e.audit)

	deliverer, err := delivery.NewService(ie.assets, ie.storage, postgres.NewDeliveryAuditRepository(e.db),
		postgres.NewDownloadTokenRepository(e.db), delivery.Config{
			Bucket: ie.bucket, Prefix: "published", DefaultTTL: time.Minute, MaxTTL: 5 * time.Minute,
		}, e.log)
	if err != nil {
		t.Fatal(err)
	}
	deliverer.Guard(e.identity.PrincipalFor, e.audit)

	ingest, err := ingestion.NewService(ingestion.Dependencies{
		Repo: ie.repo, Uploads: ie.single, Multipart: ie.multi, Sessions: ie.uploads, Assets: ie.assets, Jobs: ie.processingEnv.jobs,
		Requeue: func(ctx context.Context, tenantID, assetID, actor string) error {
			_, err := ie.proc.Requeue(ctx, tenantID, assetID, actor)
			return err
		},
		Manifests: ie.storage, Bucket: ie.bucket, Log: e.log, Metrics: ie.metrics, Audit: e.audit,
	}, ie.cfg)
	if err != nil {
		t.Fatal(err)
	}

	dead := postgres.NewDeadLetterRepository(e.db)
	eventAdmin := events.NewAdmin(postgres.NewOutboxRepository(e.db), postgres.NewInboxRepository(e.db), dead, events.Checks{
		Uploads: ie.uploads, Assets: ie.assets, Jobs: ie.processingEnv.jobs,
		Deletions: postgres.NewDeletionRepository(e.db), Holds: postgres.NewLegalHoldRepository(e.db), Ingestion: ie.repo,
	})

	rates := map[string]ratelimit.Rule{}
	for name, rule := range e.sec.Rates {
		rates[name] = ratelimit.Rule{Requests: rule.Requests, Window: rule.Window}
	}
	// Deletions are due at once: no grace period, so a test can run them.
	lifecycle, err := assetservice.NewService(assetservice.Dependencies{
		Assets: ie.assets, Uploads: ie.uploads, Jobs: ie.processingEnv.jobs,
		Quarantine: postgres.NewQuarantineRepository(e.db), Policies: postgres.NewRetentionPolicyRepository(e.db),
		Holds: postgres.NewLegalHoldRepository(e.db), Deletions: postgres.NewDeletionRepository(e.db),
		Events: postgres.NewLifecycleEventRepository(e.db), Cursors: postgres.NewCursorRepository(e.db), Storage: ie.storage,
	}, assetservice.Config{
		Owner: "it-lifecycle", QuarantineBucket: ie.bucket, DeliveryBucket: ie.bucket, DeliveryPrefix: "published",
		Platform:  assetservice.PlatformRetention{DefaultQuarantineRetention: time.Hour, QuarantineRetentionFloor: time.Hour},
		BatchSize: 50, Concurrency: 2, Lease: time.Minute, RecheckInterval: time.Minute, MaxDeletionsPerRun: 50,
		Retry: retry.Policy{BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond, MaxAttempts: 3},
	}, e.log)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle.Guard(e.audit)
	catalogDB := postgres.NewCatalogRepository(e.db)
	catalogService, err := catalog.NewService(catalog.Dependencies{
		Repo: catalogDB,
		Delete: func(ctx context.Context, p model.Principal, assetID string) (bool, error) {
			result, err := lifecycle.RequestDeletion(ctx, p, assetID)
			return result.Created, err
		},
		Audit: e.audit, Log: e.log,
	}, catalog.Config{DefaultPageSize: 100, MaxPageSize: 1000, QueryTimeout: 5 * time.Second, SlowQuery: time.Second, MaxMetadataFilters: 4})
	if err != nil {
		t.Fatal(err)
	}
	reconciler := catalog.NewReconciler(catalogDB, catalogService, ie.storage, ie.bucket, "published", e.audit, nil, e.log)

	s := &securityStack{securityEnv: e, ie: ie, delivery: deliverer, lifecycle: lifecycle, catalog: catalogService,
		reconciler: reconciler, catalogDB: catalogDB, dead: dead, datasetDB: postgres.NewDatasetRepository(e.db)}
	s.datasets = s.newDatasetService(t, nil)
	s.api = httptest.NewServer(server.NewRouter(server.Routes{
		Resolver: middleware.BearerResolver[identity.Identity](e.identity),
		Identity: e.identity, Quotas: e.quotas, Audit: e.audit,
		DeadLetters: events.NewGuardedAdmin(eventAdmin, dead, e.audit),
		Uploads:     ie.single, Multipart: ie.multi, Delivery: deliverer, Ingestion: ingest, Lifecycle: lifecycle,
		Catalog: catalogService, Datasets: s.datasets,
		StorageCapabilities: func(r *http.Request) (interfaces.StorageCapabilities, error) {
			return ie.storage.Capabilities(r.Context(), ie.bucket)
		},
		Quarantine: processing.NewQuarantine(ie.assets, ie.processingEnv.jobs, postgres.NewQuarantineRepository(e.db),
			postgres.NewProcessingEventRepository(e.db), e.audit),
		ProxyEnabled: true, Limiter: e.limiter, Rates: rates, Log: e.log,
	}))
	t.Cleanup(s.api.Close)
	return s
}

// httpResult is an API answer.
type httpResult struct {
	status int
	header http.Header
	body   []byte
}

func (r httpResult) String() string { return fmt.Sprintf("HTTP %d: %s", r.status, r.body) }

// call sends a request with a bearer token and an organization; either
// may be empty. A JSON body is assumed unless headers say otherwise.
func (s *securityStack) call(t *testing.T, token, org, method, path, body string, headers ...map[string]string) httpResult {
	t.Helper()
	req, err := http.NewRequest(method, s.api.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if org != "" {
		req.Header.Set("X-Organization-ID", org)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, h := range headers {
		for k, v := range h {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return httpResult{status: resp.StatusCode, header: resp.Header, body: data}
}

// expect is call that fails the test unless the status is want, decoding
// the body into out when given.
func (s *securityStack) expect(t *testing.T, token, org, method, path, body string, want int, out any) httpResult {
	t.Helper()
	r := s.call(t, token, org, method, path, body)
	if r.status != want {
		t.Fatalf("%s %s: %s, want %d", method, path, r, want)
	}
	if out != nil {
		if err := json.Unmarshal(r.body, out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, path, err, r.body)
		}
	}
	return r
}

// signIn logs in over HTTP.
func (s *securityStack) signIn(t *testing.T, email, pw string) response.TokensResponse {
	t.Helper()
	var tokens response.TokensResponse
	body := fmt.Sprintf(`{"email":%q,"password":%q}`, email, pw)
	s.expect(t, "", "", http.MethodPost, "/api/v1/auth/login", body, http.StatusOK, &tokens)
	return tokens
}

// availableAsset uploads data as p over the services and runs it through
// processing, returning the published asset.
func (s *securityStack) availableAsset(t *testing.T, p model.Principal, name string, data []byte) model.Asset {
	t.Helper()
	ctx := context.Background()
	upload := s.upload(t, p, name, data)
	if _, err := s.ie.single.CompleteFor(ctx, p, upload.ID); err != nil {
		t.Fatalf("complete %s: %v", name, err)
	}
	if _, err := s.ie.proc.Intake(ctx, p.TenantID, upload.ID); err != nil {
		t.Fatalf("process %s: %v", name, err)
	}
	asset, err := s.ie.assets.GetByUpload(ctx, p.TenantID, upload.ID)
	if err != nil {
		t.Fatalf("asset of %s: %v", name, err)
	}
	return *asset
}

// upload starts a single-request upload as p and puts its bytes.
func (s *securityStack) upload(t *testing.T, p model.Principal, name string, data []byte) *model.UploadSession {
	t.Helper()
	ctx := context.Background()
	result, err := s.ie.single.CreateFor(ctx, p, uploadOf(name, data))
	if err != nil {
		t.Fatalf("upload %s: %v", name, err)
	}
	s.ie.storage.PutObject(result.Upload.ObjectKey, data)
	return result.Upload
}

// uploadRequest is a text upload of size bytes; name is also its
// idempotency key.
func uploadRequest(name string, size int64) request.CreateUploadRequest {
	return request.CreateUploadRequest{FileName: name + ".txt", ContentType: "text/plain", SizeBytes: size, IdempotencyKey: name}
}

// uploadOf is uploadRequest for data, with its checksum, which
// processing requires.
func uploadOf(name string, data []byte) request.CreateUploadRequest {
	req := uploadRequest(name, int64(len(data)))
	sum := sha256.Sum256(data)
	req.ChecksumSHA256 = hex.EncodeToString(sum[:])
	return req
}

// withPurge runs fn in a transaction allowed to delete audit events, as
// only the retention purge is.
func withPurge(ctx context.Context, db *postgres.DB, fn func(pgx.Tx) error) error {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SET LOCAL storage.audit_purge = 'on'`); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
