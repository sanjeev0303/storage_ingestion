package storageconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/secretbox"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

// HealthStore keeps posture reports and staged credentials
// (postgres.StorageConnectionRepository).
type HealthStore interface {
	StageCredential(ctx context.Context, tenantID, connectionID string, sealed interfaces.SealedCredential, actor string, at time.Time) (model.StorageConnection, error)
	StagedCredential(ctx context.Context, tenantID, connectionID string) (interfaces.SealedCredential, error)
	PromoteCredential(ctx context.Context, tenantID, connectionID, actor string, keepUntil, at time.Time) (model.StorageConnection, error)
	RollbackCredential(ctx context.Context, tenantID, connectionID, actor string, at time.Time) (model.StorageConnection, error)
	DiscardStaged(ctx context.Context, tenantID, connectionID, actor string, at time.Time) (model.StorageConnection, error)
	ExpirePrevious(ctx context.Context, now time.Time) (int64, error)
	Posture(ctx context.Context, tenantID, connectionID string) (model.PostureReport, bool, error)
	PutPosture(ctx context.Context, tenantID string, report model.PostureReport) error
	PostureDue(ctx context.Context, before time.Time, limit int) ([]model.StorageConnection, error)
	DatasetRules(ctx context.Context, tenantID, dataset string) (model.PlacementRules, error)
}

// Alerter raises security alerts, deduplicated by key (the access store).
type Alerter interface {
	AddAlert(ctx context.Context, a model.SecurityAlert) (bool, error)
}

// rollbackWindow is how long a replaced credential is kept for a rollback.
const rollbackWindow = 7 * 24 * time.Hour

// ErrPostureOff: posture, staged credentials and drift are not set up in
// this process.
var ErrPostureOff = errors.New("storage posture is not configured")

// ─── Posture ──────────────────────────────────────────────────────────

// PostureFor reads a connection's latest posture report.
func (s *Service) PostureFor(ctx context.Context, p model.Principal, connectionID string) (model.PostureReport, bool, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermStorageRead, "storage_connection.posture.read", "storage_connection", connectionID); err != nil {
		return model.PostureReport{}, false, err
	}
	if s.deps.Health == nil {
		return model.PostureReport{}, false, ErrPostureOff
	}
	if _, err := s.ownConnection(ctx, p, connectionID); err != nil {
		return model.PostureReport{}, false, err
	}
	return s.deps.Health.Posture(ctx, p.TenantID, connectionID)
}

// CheckPosture checks a connection's bucket now. With probe, it also
// tries a write encrypted with a customer-provided key, which must be
// refused.
func (s *Service) CheckPosture(ctx context.Context, p model.Principal, connectionID string, probe bool) (model.PostureReport, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermStorageManage, "storage_connection.posture.check", "storage_connection", connectionID); err != nil {
		return model.PostureReport{}, err
	}
	if s.deps.Health == nil {
		return model.PostureReport{}, ErrPostureOff
	}
	if err := s.enabled(); err != nil {
		return model.PostureReport{}, err
	}
	c, err := s.ownConnection(ctx, p, connectionID)
	if err != nil {
		return model.PostureReport{}, err
	}
	client, closeClient, err := s.deps.Clients.BuildStored(ctx, c)
	if err != nil {
		return model.PostureReport{}, err
	}
	defer closeClient()
	report, err := s.posture(ctx, c, client, probe, nil)
	if err != nil {
		return model.PostureReport{}, err
	}
	s.record(ctx, p, "storage_connection.posture.check", c, map[string]any{"worst": report.Worst, "phi_safe": report.PHISafe})
	return report, nil
}

// posture runs the provider's checks and the platform's own, stores the
// report and raises alerts on what got worse. selfTest is the write
// probe's outcome when the caller just ran one.
func (s *Service) posture(ctx context.Context, c model.StorageConnection, client interfaces.StorageProvider, probe bool, selfTest *model.PostureCheck) (model.PostureReport, error) {
	report := model.PostureReport{ConnectionID: c.ID, CheckedAt: s.now()}
	timeout := s.cfg.TestTimeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	checkCtx, cancel := context.WithTimeout(ctx, 2*timeout)
	defer cancel()

	opts := model.PostureOptions{Prefixes: []string{c.StagingPrefix, c.ReadyPrefix}, ExpectedOwner: c.ExpectedOwner}
	if inspector, ok := client.(interfaces.PostureInspector); ok {
		facts, err := inspector.Posture(checkCtx, c.Bucket, opts)
		if err != nil {
			report.Checks = append(report.Checks, model.PostureCheck{Name: "bucket", Status: model.PostureFail,
				Detail: "the bucket's settings could not be read: " + categoryMessages[Categorize(err)]})
		}
		report.Checks = append(report.Checks, facts.Checks...)
		report.LifecycleHash, report.OwnerAccount = facts.LifecycleHash, facts.OwnerAccount
		report.Location, report.LocationVerified = facts.Location, facts.LocationVerified
	} else {
		report.Checks = append(report.Checks, model.PostureCheck{Name: "bucket", Status: model.PostureNotChecked,
			Detail: "this provider's settings cannot be read"})
	}

	if prober, ok := client.(interfaces.SSECProber); ok && probe {
		key := c.ReadyPrefix + "/" + c.TenantID + "/.connection-test/sse-c-" + s.newID()
		accepted, err := prober.ProbeSSEC(checkCtx, c.Bucket, key)
		check := model.PostureCheck{Name: model.CheckSSECDenied}
		switch {
		case err != nil:
			check.Status, check.Detail = model.PostureNotChecked, "the probe could not run: "+categoryMessages[Categorize(err)]
		case accepted:
			check.Status = model.PostureFail
			check.Detail = "the bucket accepted a write encrypted with a customer-provided key (SSE-C): stolen write keys could re-encrypt " +
				"objects and hold them to ransom. Block SSE-C in the bucket's encryption settings, or deny it in the bucket policy"
		default:
			check.Status, check.Detail = model.PosturePass, "a write with a customer-provided key (SSE-C) was refused"
		}
		replaceCheck(&report, check)
	}

	report.Checks = append(report.Checks, regionCheck(c, &report), customerKeyCheck(c))
	report.EncryptionKey = c.EncryptionKey
	report.Checks = append(report.Checks, guessable(c))
	if selfTest != nil {
		report.Checks = append(report.Checks, *selfTest)
	} else {
		report.Checks = append(report.Checks, lastSelfTest(c, report.CheckedAt))
	}

	previous, hadPrevious, err := s.deps.Health.Posture(ctx, c.TenantID, c.ID)
	if err != nil {
		return report, err
	}
	drift := model.PostureCheck{Name: model.CheckLifecycleDrift, Status: model.PosturePass, Detail: "first check of the lifecycle rules"}
	switch {
	case report.LifecycleHash == "":
		drift.Status, drift.Detail = model.PostureNotChecked, "the lifecycle rules could not be read"
	case hadPrevious && previous.LifecycleHash != "" && previous.LifecycleHash != report.LifecycleHash:
		drift.Status = model.PostureWarn
		drift.Detail = "the bucket's lifecycle rules changed since " + previous.CheckedAt.Format(time.RFC3339) + "; check who changed them"
	case hadPrevious && previous.LifecycleHash != "":
		drift.Detail = "unchanged since " + previous.CheckedAt.Format(time.RFC3339)
		report.LifecycleHash = previous.LifecycleHash
	}
	report.Checks = append(report.Checks, drift)
	report.Summarize()

	if err := s.deps.Health.PutPosture(ctx, c.TenantID, report); err != nil {
		return report, err
	}
	s.alertChanges(ctx, c, previous, hadPrevious, report, drift.Status == model.PostureWarn)
	return report, nil
}

func replaceCheck(r *model.PostureReport, check model.PostureCheck) {
	for i := range r.Checks {
		if r.Checks[i].Name == check.Name {
			r.Checks[i] = check
			return
		}
	}
	r.Checks = append(r.Checks, check)
}

// alertChanges raises a security alert when a connection's posture gets
// worse, and when its lifecycle rules change: stolen keys have been used
// to add rules that delete data within days.
func (s *Service) alertChanges(ctx context.Context, c model.StorageConnection, previous model.PostureReport, hadPrevious bool, now model.PostureReport, lifecycleChanged bool) {
	if s.deps.Alerts == nil {
		return
	}
	day := now.CheckedAt.Format("2006-01-02")
	raise := func(kind, detail, key string) {
		_, err := s.deps.Alerts.AddAlert(ctx, model.SecurityAlert{
			ID: s.newID(), TenantID: c.TenantID, PrincipalID: "connection:" + c.ID, Kind: kind, Detail: detail,
			Metrics: map[string]any{"connection_id": c.ID, "worst": now.Worst, "phi_safe": now.PHISafe},
			State:   "OPEN", DedupKey: key, CreatedAt: now.CheckedAt,
		})
		if err != nil {
			s.deps.Log.Warn("posture alert not recorded", "connection_id", c.ID, "error", err)
		}
	}
	if lifecycleChanged {
		raise(model.AlertLifecycleChanged, fmt.Sprintf("the lifecycle rules of connection %s (%s) changed", c.Name, c.Bucket),
			"lifecycle:"+c.ID+":"+now.LifecycleHash)
	}
	worse := now.Worst == model.PostureFail && (!hadPrevious || previous.Worst != model.PostureFail)
	lostPHI := hadPrevious && previous.PHISafe && !now.PHISafe
	if worse || lostPHI {
		var failing []string
		for _, check := range now.Checks {
			if check.Status == model.PostureFail {
				failing = append(failing, check.Name)
			}
		}
		detail := fmt.Sprintf("connection %s (%s) fails posture checks: %s", c.Name, c.Bucket, strings.Join(failing, ", "))
		if lostPHI && !worse {
			detail = fmt.Sprintf("connection %s (%s) is no longer safe for PHI", c.Name, c.Bucket)
		}
		raise(model.AlertPosture, detail, "posture:"+c.ID+":"+day)
	}
}

// regionCheck compares where the provider says the bucket is with the
// connection's own setting. An unverified location keeps the setting.
func regionCheck(c model.StorageConnection, r *model.PostureReport) model.PostureCheck {
	declared := strings.ToLower(c.Region)
	check := model.PostureCheck{Name: model.CheckRegion}
	switch {
	case !r.LocationVerified:
		r.Location = declared
		check.Status = model.PostureNotChecked
		check.Detail = "the provider did not say where the bucket is"
		if declared != "" {
			check.Detail += "; the connection declares " + declared
		}
	case declared == "" || declared == r.Location || (c.Provider == model.ProviderR2 && declared == "auto"):
		check.Status, check.Detail = model.PosturePass, "the bucket is in "+r.Location
	default:
		check.Status = model.PostureFail
		check.Detail = "the bucket is in " + r.Location + ", not " + declared + " as the connection says"
	}
	return check
}

func customerKeyCheck(c model.StorageConnection) model.PostureCheck {
	if c.EncryptionKey == "" {
		return model.PostureCheck{Name: model.CheckCustomerKey, Status: model.PostureNotChecked,
			Detail: "no customer-managed key is set; writes use the bucket's default encryption"}
	}
	return model.PostureCheck{Name: model.CheckCustomerKey, Status: model.PosturePass,
		Detail: "every write is encrypted with " + c.EncryptionKey + "; the write probe uses it too"}
}

// Buckets in a global namespace (AWS, GCS) with names made only of words
// are easy to guess: strangers' requests reach them, and a deleted one
// can be re-registered by anyone.
var nameToken = regexp.MustCompile(`[a-z0-9]+`)

func guessable(c model.StorageConnection) model.PostureCheck {
	check := model.PostureCheck{Name: model.CheckGuessableName, Status: model.PosturePass}
	if c.Provider != model.ProviderAWSS3 && c.Provider != model.ProviderGCS {
		check.Detail = "bucket names on this provider are scoped to the account"
		return check
	}
	for _, token := range nameToken.FindAllString(strings.ToLower(c.Bucket), -1) {
		digits := strings.IndexFunc(token, func(r rune) bool { return r >= '0' && r <= '9' }) >= 0
		letters := strings.IndexFunc(token, func(r rune) bool { return r >= 'a' && r <= 'z' }) >= 0
		if len(token) >= 8 && digits && (letters || len(token) >= 10) {
			check.Detail = "the name has a part that is hard to guess"
			return check
		}
	}
	check.Status = model.PostureWarn
	check.Detail = "the name is easy to guess: strangers' requests can reach it, and if the bucket is ever deleted anyone can " +
		"re-create it. Pin the bucket owner, and give new buckets a random part"
	return check
}

func lastSelfTest(c model.StorageConnection, now time.Time) model.PostureCheck {
	check := model.PostureCheck{Name: model.CheckSelfTest}
	switch {
	case c.LastTestOKAt != nil && now.Sub(*c.LastTestOKAt) <= 30*24*time.Hour:
		check.Status, check.Detail = model.PosturePass, "the last connection test passed at "+c.LastTestOKAt.Format(time.RFC3339)
	case c.LastTestOKAt != nil:
		check.Status, check.Detail = model.PostureWarn, "no passing connection test in 30 days; run one with the write probe"
	default:
		check.Status, check.Detail = model.PostureWarn, "the connection has never passed a test"
	}
	return check
}

// PostureStats counts one scheduled pass.
type PostureStats struct {
	Checked, Failing, Errors int
	ExpiredCredentials       int64
}

// CheckDue checks every connection whose posture is older than the
// interval, and erases replaced credentials past their rollback window.
// The cleanup worker calls it on a schedule.
func (s *Service) CheckDue(ctx context.Context, interval time.Duration, limit int) (PostureStats, error) {
	var stats PostureStats
	if s.deps.Health == nil || s.enabled() != nil {
		return stats, nil
	}
	expired, err := s.deps.Health.ExpirePrevious(ctx, s.now())
	if err != nil {
		return stats, err
	}
	stats.ExpiredCredentials = expired
	due, err := s.deps.Health.PostureDue(ctx, s.now().Add(-interval), limit)
	if err != nil {
		return stats, err
	}
	for _, c := range due {
		client, closeClient, err := s.deps.Clients.BuildStored(ctx, c)
		if err != nil {
			stats.Errors++
			s.deps.Log.Warn("posture check skipped", "connection_id", c.ID, "category", Categorize(err))
			continue
		}
		report, err := s.posture(ctx, c, client, false, nil)
		closeClient()
		if err != nil {
			stats.Errors++
			s.deps.Log.Warn("posture check failed", "connection_id", c.ID, "error", err)
			continue
		}
		stats.Checked++
		if report.Worst == model.PostureFail {
			stats.Failing++
		}
	}
	return stats, nil
}

// ─── Placement ────────────────────────────────────────────────────────

// CheckDatasetPlacement checks a dataset's rules against the connection
// an upload into it is about to use.
func (s *Service) CheckDatasetPlacement(ctx context.Context, p model.Principal, dataset, connectionID string) error {
	if s.deps.Health == nil || dataset == "" {
		return nil
	}
	rules, err := s.deps.Health.DatasetRules(ctx, p.TenantID, dataset)
	if err != nil {
		return err
	}
	return s.CheckPlacement(ctx, p, rules, connectionID)
}

// CheckPlacement checks a dataset's rules against a connection
// (connectionID "" is the organization's default), and records the
// decision in the audit log whenever there are rules to check:
//
//   - PHI only on a connection whose last posture check found it safe:
//     nothing public, encrypted, versioned and SSE-C refused. The
//     platform's own storage is the operator's and is held to that by them.
//   - Residency: the connection's bucket must be in an allowed location,
//     as the provider reported it; a location only declared, never
//     verified, does not count. The platform's own storage counts as being
//     where the operator configured it.
//   - A customer-managed key: the connection must encrypt every write
//     with one.
func (s *Service) CheckPlacement(ctx context.Context, p model.Principal, rules model.PlacementRules, connectionID string) error {
	if s.deps.Health == nil || !rules.Any() {
		return nil
	}
	if connectionID == "" {
		id, err := s.deps.Connections.DefaultConnection(ctx, p.TenantID)
		if err != nil {
			return err
		}
		connectionID = id
	}
	reason, err := s.placementRefusal(ctx, p.TenantID, rules, connectionID)
	if err != nil {
		return err
	}
	if s.deps.Audit != nil {
		e := audit.From(ctx, p, "placement.check", "storage_connection", connectionID)
		e.Details = map[string]any{"data_class": rules.DataClass, "residency": rules.Residency,
			"require_customer_key": rules.RequireCustomerKey, "allowed": reason == ""}
		if reason != "" {
			e.Outcome, e.Details["reason"] = model.AuditDenied, reason
		}
		s.deps.Audit.Record(ctx, e)
	}
	if reason != "" {
		return fmt.Errorf("%w: %s", model.ErrPlacementRefused, reason)
	}
	return nil
}

// placementRefusal says why rules refuse a connection ("" when they do not).
func (s *Service) placementRefusal(ctx context.Context, tenantID string, rules model.PlacementRules, connectionID string) (string, error) {
	c, err := s.deps.Connections.GetConnection(ctx, tenantID, connectionID)
	if err != nil {
		return "", err
	}
	platform := model.IsPlatformConnection(connectionID)
	var report model.PostureReport
	checked := false
	if !platform {
		if report, checked, err = s.deps.Health.Posture(ctx, tenantID, connectionID); err != nil {
			return "", err
		}
	}

	if rules.DataClass == model.DataPHI && !platform {
		if !checked {
			return "PHI needs a connection whose posture has been checked; run its connection test", nil
		}
		if !report.PHISafe {
			var failing []string
			for _, name := range []string{model.CheckPublicAccessBlock, model.CheckPublicPolicy, model.CheckPublicACL,
				model.CheckEncryption, model.CheckVersioning, model.CheckSSECDenied} {
				if check, ok := report.Check(name); !ok || check.Status != model.PosturePass {
					failing = append(failing, name)
				}
			}
			return "the connection is not safe for PHI (" + strings.Join(failing, ", ") + ")", nil
		}
	}

	if len(rules.Residency) > 0 {
		location, verified := strings.ToLower(c.Region), platform
		if checked && report.Location != "" {
			location, verified = report.Location, report.LocationVerified
		}
		switch {
		case !verified:
			return "the connection's location has not been verified with the provider, so residency cannot be shown; test the connection " +
				"with credentials that may read the bucket's location", nil
		case !model.LocationAllowed(location, rules.Residency):
			return fmt.Sprintf("the connection's bucket is in %s, outside %s", location, strings.Join(rules.Residency, ", ")), nil
		}
	}

	if rules.RequireCustomerKey && c.EncryptionKey == "" {
		return "the dataset requires a customer-managed key and the connection does not encrypt writes with one", nil
	}
	return "", nil
}

// ─── Two credentials ──────────────────────────────────────────────────

// StageCredentials tests new credentials, writing included, and keeps
// them next to the current ones without using them yet.
func (s *Service) StageCredentials(ctx context.Context, p model.Principal, connectionID string, secrets model.StorageSecrets) (model.StorageConnection, TestResult, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermStorageManage, "storage_connection.credentials.stage", "storage_connection", connectionID); err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	if s.deps.Health == nil {
		return model.StorageConnection{}, TestResult{}, ErrPostureOff
	}
	if err := s.enabled(); err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	c, err := s.ownConnection(ctx, p, connectionID)
	if err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	if len(c.AuthMethod.SecretFields()) == 0 {
		return model.StorageConnection{}, TestResult{}, fmt.Errorf("%w: %s connections have no stored credentials", ErrInvalidRequest, c.AuthMethod)
	}
	if err := secrets.ValidateFor(c.AuthMethod); err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	result, err := s.runTest(ctx, c, secrets, true)
	if err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	if !result.OK {
		s.record(ctx, p, "storage_connection.credentials.stage", c, map[string]any{"outcome": "refused", "category": result.Category})
		return c, result, ErrTestFailed
	}
	version := max(c.CredentialVersion, c.StagedCredentialVersion) + 1
	sealed, err := s.seal(c, version, secrets)
	if err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	stored, err := s.deps.Health.StageCredential(ctx, p.TenantID, connectionID, sealed, p.UserID, s.now())
	if err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	s.record(ctx, p, "storage_connection.credentials.stage", stored, map[string]any{"staged": version})
	return stored, result, nil
}

// PromoteCredentials tests the staged credentials once more, writing
// included, then makes them current. The replaced ones are kept for a
// rollback for seven days.
func (s *Service) PromoteCredentials(ctx context.Context, p model.Principal, connectionID string) (model.StorageConnection, TestResult, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermStorageManage, "storage_connection.credentials.promote", "storage_connection", connectionID); err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	if s.deps.Health == nil {
		return model.StorageConnection{}, TestResult{}, ErrPostureOff
	}
	if err := s.enabled(); err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	c, err := s.ownConnection(ctx, p, connectionID)
	if err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	sealed, err := s.deps.Health.StagedCredential(ctx, p.TenantID, connectionID)
	if err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	secrets, err := s.open(c, sealed)
	if err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	// The pre-flight: keys can be revoked between staging and promotion.
	result, err := s.runTest(ctx, c, secrets, true)
	if err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	if !result.OK {
		s.record(ctx, p, "storage_connection.credentials.promote", c, map[string]any{"outcome": "refused", "category": result.Category})
		return c, result, ErrTestFailed
	}
	stored, err := s.deps.Health.PromoteCredential(ctx, p.TenantID, connectionID, p.UserID, s.now().Add(rollbackWindow), s.now())
	if err != nil {
		return model.StorageConnection{}, TestResult{}, err
	}
	s.deps.Clients.Invalidate(connectionID)
	s.record(ctx, p, "storage_connection.credentials.promote", stored, map[string]any{"rotation": stored.CredentialVersion})
	return stored, result, nil
}

// RollbackCredentials puts back the credentials the last promotion
// replaced, within the rollback window.
func (s *Service) RollbackCredentials(ctx context.Context, p model.Principal, connectionID string) (model.StorageConnection, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermStorageManage, "storage_connection.credentials.rollback", "storage_connection", connectionID); err != nil {
		return model.StorageConnection{}, err
	}
	if s.deps.Health == nil {
		return model.StorageConnection{}, ErrPostureOff
	}
	if _, err := s.ownConnection(ctx, p, connectionID); err != nil {
		return model.StorageConnection{}, err
	}
	stored, err := s.deps.Health.RollbackCredential(ctx, p.TenantID, connectionID, p.UserID, s.now())
	if err != nil {
		return model.StorageConnection{}, err
	}
	s.deps.Clients.Invalidate(connectionID)
	s.record(ctx, p, "storage_connection.credentials.rollback", stored, map[string]any{"rotation": stored.CredentialVersion})
	return stored, nil
}

// DiscardStagedCredentials drops staged credentials unused.
func (s *Service) DiscardStagedCredentials(ctx context.Context, p model.Principal, connectionID string) (model.StorageConnection, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermStorageManage, "storage_connection.credentials.discard", "storage_connection", connectionID); err != nil {
		return model.StorageConnection{}, err
	}
	if s.deps.Health == nil {
		return model.StorageConnection{}, ErrPostureOff
	}
	if _, err := s.ownConnection(ctx, p, connectionID); err != nil {
		return model.StorageConnection{}, err
	}
	stored, err := s.deps.Health.DiscardStaged(ctx, p.TenantID, connectionID, p.UserID, s.now())
	if err != nil {
		return model.StorageConnection{}, err
	}
	s.record(ctx, p, "storage_connection.credentials.discard", stored, nil)
	return stored, nil
}

// open decrypts a sealed credential of c.
func (s *Service) open(c model.StorageConnection, sealed interfaces.SealedCredential) (model.StorageSecrets, error) {
	var secrets model.StorageSecrets
	plain, err := s.deps.Sealer.Open(secretbox.Sealed{KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext},
		c.CredentialAAD(sealed.Version))
	if err != nil {
		return secrets, fmt.Errorf("%w: the staged credentials could not be decrypted", model.ErrCredentialMissing)
	}
	err = json.Unmarshal(plain, &secrets)
	clear(plain)
	if err != nil {
		return secrets, fmt.Errorf("%w: the staged credentials are unreadable", model.ErrCredentialMissing)
	}
	return secrets, nil
}
