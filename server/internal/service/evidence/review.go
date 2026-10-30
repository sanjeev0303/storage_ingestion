package evidence

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

// ─── Audit export ─────────────────────────────────────────────────────

// Export pages the caller's chain, oldest first after afterSeq, with
// exactly what each hash covers (docs/evidence.md), for a SIEM or an
// inspector.
func (s *Service) Export(ctx context.Context, p model.Principal, afterSeq int64, from, to time.Time, limit int) ([]postgres.ExportedEvent, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermAuditRead, "audit.export", "organization", p.TenantID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 10000 {
		limit = 10000
	}
	out, err := s.deps.Store.Export(ctx, p.TenantID, afterSeq, from, to, limit)
	if out == nil {
		out = []postgres.ExportedEvent{}
	}
	return out, err
}

func cefEscape(v string, header bool) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	if header {
		return strings.ReplaceAll(v, "|", `\|`)
	}
	v = strings.ReplaceAll(v, "=", `\=`)
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(v)
}

// CEF formats an exported event as an ArcSight Common Event Format line.
func CEF(e postgres.ExportedEvent) string {
	severity := 3
	switch e.Outcome {
	case "DENIED":
		severity = 6
	case "FAILED":
		severity = 5
	}
	at, _ := time.Parse(time.RFC3339Nano, e.CreatedAt)
	return fmt.Sprintf("CEF:0|OSP|object-storage-platform|1|%s|%s|%d|rt=%d suser=%s act=%s outcome=%s src=%s cs1Label=resource cs1=%s cs2Label=chainSeq cs2=%d cs3Label=hash cs3=%s",
		cefEscape(e.Action, true), cefEscape(e.Action, true), severity, at.UnixMilli(), cefEscape(e.ActorID, false),
		cefEscape(e.Action, false), cefEscape(e.Outcome, false), cefEscape(e.SourceIP, false),
		cefEscape(e.ResourceType+":"+e.ResourceID, false), e.Seq, e.Hash)
}

// ─── Reviews ──────────────────────────────────────────────────────────

// What each review category looks for in the chain.
var reviewActions = map[string][]string{
	"DELETION": {"asset.delete", "asset.bulk_delete", "catalog.object_delete", "dataset.expire_versions", "dataset.archive",
		"organization.delete"},
	"RETENTION_CHANGE": {"retention.policy_set", "retention.hold_place", "retention.hold_release", "retention.change_requested",
		"governance.update", "governance.approval_decide", "tiering.rule", "tiering.rule_delete", "evidence.settings"},
	"PERMISSION_CHANGE": {"member.role_change", "member.remove", "member.suspend", "member.reactivate", "member.join",
		"invitation.create", "invitation.accept", "role.create", "role.update", "role.delete", "api_token.create", "api_token.revoke",
		"sso.update", "sso.delete", "scim.token_issue", "scim.token_revoke", "access_policy.update", "compliance.regulator_access"},
}

// Generate builds the exception review of a period for an organization.
func (s *Service) Generate(ctx context.Context, tenantID string, from, to time.Time, by string) (model.AuditReview, error) {
	st, err := s.deps.Store.Settings(ctx, tenantID)
	if err != nil {
		return model.AuditReview{}, err
	}
	var findings []model.ReviewFinding
	for kind, actions := range reviewActions {
		events, err := s.deps.Store.Actions(ctx, tenantID, actions, from, to, 2000)
		if err != nil {
			return model.AuditReview{}, err
		}
		for _, e := range events {
			findings = append(findings, model.ReviewFinding{Kind: kind, At: e.CreatedAt, Actor: e.ActorID, AuditSeq: e.Seq,
				Subject: e.ResourceType + ":" + e.ResourceID, Detail: describe(e)})
		}
	}
	failed, err := s.deps.Store.FailedChecks(ctx, tenantID, from, to, 2000)
	if err != nil {
		return model.AuditReview{}, err
	}
	reads, err := s.deps.Store.BulkReads(ctx, tenantID, st.Timezone, st.WorkStartHour, st.WorkEndHour, st.BulkReadThreshold, from, to)
	if err != nil {
		return model.AuditReview{}, err
	}
	findings = append(append(findings, failed...), reads...)
	sort.SliceStable(findings, func(i, j int) bool { return findings[i].At.Before(findings[j].At) })
	if findings == nil {
		findings = []model.ReviewFinding{}
	}
	counts := map[string]int{"DELETION": 0, "RETENTION_CHANGE": 0, "PERMISSION_CHANGE": 0, "FAILED_HASH": 0, "AFTER_HOURS_READS": 0}
	for _, f := range findings {
		counts[f.Kind]++
	}
	v := model.AuditReview{ID: newID(), TenantID: tenantID, PeriodStart: from, PeriodEnd: to, Findings: findings, Counts: counts,
		State: "OPEN", CreatedBy: by, CreatedAt: s.now()}
	raw, err := json.Marshal(struct {
		Start, End time.Time
		Findings   []model.ReviewFinding
	}{from, to, findings})
	if err != nil {
		return model.AuditReview{}, err
	}
	sum := sha256.Sum256(raw)
	v.ReportSHA256 = hex.EncodeToString(sum[:])
	return v, s.deps.Store.PutReview(ctx, v)
}

func describe(e model.AuditEvent) string {
	var parts []string
	keys := make([]string, 0, len(e.Details))
	for k := range e.Details {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, e.Details[k]))
		if len(parts) == 6 {
			break
		}
	}
	return strings.TrimSpace(e.Action + " " + strings.Join(parts, " "))
}

// CreateReview builds a review of a period now.
func (s *Service) CreateReview(ctx context.Context, p model.Principal, from, to time.Time) (model.AuditReview, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermAuditRead, "audit.review_create", "organization", p.TenantID); err != nil {
		return model.AuditReview{}, err
	}
	if to.IsZero() {
		to = s.now()
	}
	if from.IsZero() || !from.Before(to) || to.Sub(from) > 366*24*time.Hour {
		return model.AuditReview{}, invalid("a review covers from to to, at most a year")
	}
	return s.Generate(ctx, p.TenantID, from, to, p.UserID)
}

func (s *Service) Reviews(ctx context.Context, p model.Principal) ([]model.AuditReview, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermAuditRead, "audit.review_read", "organization", p.TenantID); err != nil {
		return nil, err
	}
	return s.deps.Store.Reviews(ctx, p.TenantID, 100)
}

func (s *Service) Review(ctx context.Context, p model.Principal, id string) (model.AuditReview, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermAuditRead, "audit.review_read", "audit_review", id); err != nil {
		return model.AuditReview{}, err
	}
	return s.deps.Store.Review(ctx, p.TenantID, id)
}

// Sign records a reviewer's sign-off. The sign-off is an entry in the
// audit chain naming the review and its report's SHA-256, so it is as
// tamper-evident as the events it covers.
func (s *Service) Sign(ctx context.Context, p model.Principal, id, note string) (model.AuditReview, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermAuditRead, "audit.review_signed", "audit_review", id); err != nil {
		return model.AuditReview{}, err
	}
	if len(note) > 4000 {
		return model.AuditReview{}, invalid("a note is at most 4000 characters")
	}
	v, err := s.deps.Store.Review(ctx, p.TenantID, id)
	if err != nil {
		return model.AuditReview{}, err
	}
	if err := s.deps.Store.SignReview(ctx, p.TenantID, id, p.UserID, note, s.now()); err != nil {
		return model.AuditReview{}, err
	}
	s.record(ctx, p, "audit.review_signed", "audit_review", id, map[string]any{"report_sha256": v.ReportSHA256,
		"period_start": v.PeriodStart.Format(time.RFC3339), "period_end": v.PeriodEnd.Format(time.RFC3339), "counts": v.Counts, "note": note})
	return s.deps.Store.Review(ctx, p.TenantID, id)
}

// ReviewExport is the review with every chain entry of its period, for
// an inspector: a zip of review.json, audit.jsonl and README.md.
func (s *Service) ReviewExport(ctx context.Context, p model.Principal, id string) ([]byte, error) {
	v, err := s.Review(ctx, p, id)
	if err != nil {
		return nil, err
	}
	var events []postgres.ExportedEvent
	after := int64(0)
	for {
		page, err := s.deps.Store.Export(ctx, p.TenantID, after, v.PeriodStart, v.PeriodEnd, 10000)
		if err != nil {
			return nil, err
		}
		events = append(events, page...)
		if len(page) < 10000 {
			break
		}
		after = page[len(page)-1].Seq
	}
	s.record(ctx, p, "audit.review_export", "audit_review", id, map[string]any{"events": len(events)})
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	review, _ := json.MarshalIndent(v, "", "  ")
	var lines bytes.Buffer
	for _, e := range events {
		line, _ := json.Marshal(e)
		lines.Write(append(line, '\n'))
	}
	readme := fmt.Sprintf(`# Audit review %s

Period %s to %s. State %s%s.

- review.json: the exceptions found (deletions, retention changes, failed hashes, after-hours bulk reads, permission changes), their counts, and the report's SHA-256 (%s).
- audit.jsonl: every audit chain entry of the period, oldest first, with exactly what each hash covers. Check it with `+"`osp audit-verify audit.jsonl`"+`: hash = SHA-256(previous hash, each field followed by 0x1f, details).
`, v.ID, v.PeriodStart.Format(time.RFC3339), v.PeriodEnd.Format(time.RFC3339), v.State, signedText(v), v.ReportSHA256)
	for name, data := range map[string][]byte{"review.json": review, "audit.jsonl": lines.Bytes(), "README.md": []byte(readme)} {
		w, err := z.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func signedText(v model.AuditReview) string {
	if v.SignedAt == nil {
		return ""
	}
	return fmt.Sprintf(", signed by %s at %s", v.SignedBy, v.SignedAt.Format(time.RFC3339))
}

// DueReviews builds each organization's review once its interval has
// passed since the last one (the first covers the interval before now).
func (s *Service) DueReviews(ctx context.Context) (int, error) {
	tenants, err := s.deps.Store.Tenants(ctx)
	if err != nil {
		return 0, err
	}
	now := s.now()
	n := 0
	for _, st := range tenants {
		if st.ReviewEveryDays == 0 {
			continue
		}
		every := time.Duration(st.ReviewEveryDays) * 24 * time.Hour
		last, err := s.deps.Store.LastReviewEnd(ctx, st.TenantID)
		if err != nil {
			return n, err
		}
		from := now.Add(-every)
		if last != nil {
			if now.Sub(*last) < every {
				continue
			}
			from = *last
		}
		if _, err := s.Generate(ctx, st.TenantID, from, now, "system"); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
