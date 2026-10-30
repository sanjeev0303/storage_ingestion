package evidence

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/access"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

// Control is one requirement of a framework, how the platform meets it,
// and what the organization's own state shows now.
type Control struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Platform string   `json:"platform"`
	Status   string   `json:"status"` // MET, PARTIAL, NOT_MET, ORGANIZATION (a process the organization runs)
	Evidence []string `json:"evidence"`
}

type Framework struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Controls []Control `json:"controls"`
	Note     string    `json:"note"`
}

var Frameworks = []struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}{
	{"nist-800-171", "NIST SP 800-171 Rev. 2 (as NIH requires for controlled-access genomic data)"},
	{"21-cfr-11", "FDA 21 CFR Part 11: electronic records and signatures"},
	{"sec-17a-4", "SEC Rule 17a-4(f): electronic recordkeeping for broker-dealers"},
}

func met(ok bool) string {
	if ok {
		return "MET"
	}
	return "NOT_MET"
}

func since(t *time.Time, now time.Time) string {
	if t == nil {
		return "never"
	}
	return fmt.Sprintf("%s (%d days ago)", t.Format("2006-01-02"), int(now.Sub(*t).Hours()/24))
}

// Compliance maps a framework's controls to the platform and to the
// organization's state now.
func (s *Service) Compliance(ctx context.Context, p model.Principal, framework string) (Framework, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermAuditRead, "compliance.read", "organization", p.TenantID); err != nil {
		return Framework{}, err
	}
	now := s.now()
	f, err := s.deps.Store.ControlFacts(ctx, p.TenantID, now)
	if err != nil {
		return Framework{}, err
	}
	v, err := s.deps.Chains.Verify(ctx, p.TenantID)
	if err != nil {
		return Framework{}, err
	}
	st, err := s.deps.Store.Settings(ctx, p.TenantID)
	if err != nil {
		return Framework{}, err
	}
	chainOK := v.BrokenAt == 0
	chainText := fmt.Sprintf("audit chain verified: %d entries, %s", v.Checked, map[bool]string{true: "intact", false: "BROKEN at " + fmt.Sprint(v.BrokenAt)}[chainOK])
	anchored := f.LastAnchor != nil && now.Sub(*f.LastAnchor) < 48*time.Hour
	reviewed := f.LastSignedReview != nil && now.Sub(*f.LastSignedReview) < 45*24*time.Hour
	ssoText := fmt.Sprintf("SSO %s; %d of %d active members sign in only through the identity provider (which enforces MFA)",
		map[bool]string{true: "enabled", false: "not enabled"}[f.SSOEnabled], f.SSOOnlyMembers, f.Members)
	tokensText := fmt.Sprintf("%d active API tokens, %d without an expiry", f.ActiveTokens, f.NonExpiringTokens)
	encText := fmt.Sprintf("%d active connections, %d with a customer-managed key, %d failing posture checks",
		f.Connections, f.ConnectionsCustomerKey, f.ConnectionsFailing)

	var out Framework
	switch framework {
	case "nist-800-171":
		out = Framework{ID: framework, Name: Frameworks[0].Name, Controls: []Control{
			{ID: "3.1.1", Title: "Limit system access to authorized users", Platform: "Organization membership, roles and scoped API tokens; every request is authorized against the caller's role.",
				Status: "MET", Evidence: []string{fmt.Sprintf("%d active members", f.Members), tokensText}},
			{ID: "3.1.5", Title: "Least privilege", Platform: "Custom roles from a permission list; tokens hold a subset of their member's permissions and can be confined to datasets.",
				Status: met(f.NonExpiringTokens == 0), Evidence: []string{tokensText}},
			{ID: "3.3.1", Title: "Create and retain audit logs", Platform: "Every action and every read is recorded in a hash-chained audit log per organization.",
				Status: met(chainOK), Evidence: []string{chainText}},
			{ID: "3.3.3", Title: "Review and update logged events", Platform: "Scheduled exception reviews with a reviewer's sign-off in the chain.",
				Status: met(reviewed), Evidence: []string{"last signed review: " + since(f.LastSignedReview, now), fmt.Sprintf("reviews every %d days", st.ReviewEveryDays)}},
			{ID: "3.3.8", Title: "Protect audit information", Platform: "Append-only chain; anchors signed and written to a locked bucket outside the database.",
				Status: met(chainOK && anchored), Evidence: []string{chainText, "last anchor: " + since(f.LastAnchor, now)}},
			{ID: "3.5.3", Title: "Multifactor authentication", Platform: "SSO through the organization's identity provider, which enforces MFA; members can be set to sign in only through it.",
				Status: map[bool]string{true: "MET", false: "PARTIAL"}[f.SSOEnabled && f.SSOOnlyMembers == f.Members], Evidence: []string{ssoText}},
			{ID: "3.8.9", Title: "Protect backups of CUI", Platform: "Verified replicas at a second connection, restore drills and export bundles (copy engine).",
				Status: "ORGANIZATION", Evidence: []string{"see each dataset's protection settings"}},
			{ID: "3.13.11", Title: "FIPS-validated cryptography for CUI", Platform: "TLS to every provider; SSE-KMS, GCS or Azure customer keys per connection.",
				Status: map[bool]string{true: "MET", false: "PARTIAL"}[f.Connections > 0 && f.ConnectionsCustomerKey == f.Connections], Evidence: []string{encText}},
			{ID: "3.13.16", Title: "Protect CUI at rest", Platform: "Storage posture checks (encryption, public access, versioning) on every connection.",
				Status: met(f.ConnectionsFailing == 0), Evidence: []string{encText}},
			{ID: "3.14.2", Title: "Protect against malicious code", Platform: "Every upload is malware-scanned before it can be read.",
				Status: "MET", Evidence: []string{fmt.Sprintf("%d objects available after scanning", f.ScansClean)}},
		}, Note: "The platform covers the data store; the organization attests to its endpoints, people and procedures."}
	case "21-cfr-11":
		out = Framework{ID: framework, Name: Frameworks[1].Name, Controls: []Control{
			{ID: "11.10(a)", Title: "Validation", Platform: "Validation and readiness reports state what was checked and what was not.", Status: "ORGANIZATION",
				Evidence: []string{"the organization validates the system for its intended use"}},
			{ID: "11.10(b)", Title: "Accurate and complete copies", Platform: "Snapshots, evidence packs and export bundles with SHA-256 manifests.", Status: "MET"},
			{ID: "11.10(c)", Title: "Protection of records", Platform: "Retention policies, legal holds and Object Lock.", Status: met(f.RetentionPolicies > 0 || f.LockedObjects > 0),
				Evidence: []string{fmt.Sprintf("%d retention policies, %d locked objects, %d active legal holds", f.RetentionPolicies, f.LockedObjects, f.HeldObjects)}},
			{ID: "11.10(d)", Title: "Limiting access", Platform: "Roles, permissions and tokens.", Status: "MET", Evidence: []string{tokensText}},
			{ID: "11.10(e)", Title: "Secure, time-stamped audit trails", Platform: "Hash-chained audit with anchors outside the database (RFC 3161 time stamps when a TSA is set).",
				Status: met(chainOK && anchored), Evidence: []string{chainText, "last anchor: " + since(f.LastAnchor, now)}},
			{ID: "11.10(k)", Title: "Controls over documentation", Platform: "Governance profile GxP vault; two-person approval of deletions and retention changes.",
				Status: met(f.TwoPerson), Evidence: []string{"governance profile " + f.GovernanceProfile, fmt.Sprintf("two-person rule: %v", f.TwoPerson)}},
			{ID: "11.50, 11.70", Title: "Signature manifestations and linking", Platform: "Review sign-offs name the signer, time and meaning and are bound to the report hash in the chain.",
				Status: met(reviewed), Evidence: []string{"last signed review: " + since(f.LastSignedReview, now)}},
		}, Note: "Part 11 also requires procedures and training the organization keeps."}
	case "sec-17a-4":
		out = Framework{ID: framework, Name: Frameworks[2].Name, Controls: []Control{
			{ID: "(f)(2)(i)(A)", Title: "Audit-trail alternative to WORM", Platform: "The SEC 17a-4 audit trail profile: no deletion within retention, every change and deletion recorded with the prior version's hash, recreate as of any time.",
				Status: met(f.GovernanceProfile == "SEC_17A4_AUDIT_TRAIL" || f.LockedObjects > 0), Evidence: []string{"governance profile " + f.GovernanceProfile, chainText}},
			{ID: "(f)(2)(i)(B)", Title: "Non-rewriteable, non-erasable (WORM)", Platform: "Object Lock COMPLIANCE on records' versions.",
				Status: met(f.LockedObjects > 0), Evidence: []string{fmt.Sprintf("%d locked objects", f.LockedObjects)}},
			{ID: "(f)(2)(ii)", Title: "Verify the quality and accuracy of recording", Platform: "SHA-256 of every file at upload, re-checked on read and by scrubs.", Status: "MET"},
			{ID: "(f)(2)(iii)", Title: "Serialize and time-date records", Platform: "Versioned objects and snapshots with manifest hashes; chain entries are sequenced and time-stamped.", Status: "MET"},
			{ID: "(f)(2)(iv)", Title: "Download and produce records", Platform: "Evidence packs, export bundles and load files for any snapshot; regulator access by a logged, time-limited token.", Status: "MET"},
			{ID: "(f)(3)(vii)", Title: "Designated executive officer or third party undertaking", Platform: "The undertaking letter is generated for signature.", Status: "ORGANIZATION",
				Evidence: []string{"GET /api/v1/compliance/sec-17a-4/undertaking"}},
		}, Note: "Six-year retention (two years readily accessible) is set by the sec-17a-4 retention template."}
	default:
		return Framework{}, model.ErrUnknownFramework
	}
	return out, nil
}

// WriteComplianceEvidence writes a framework's evidence zip: the control
// mapping, members and roles, tokens, the chain's verification, anchors,
// signed reviews and the last 90 days of the audit chain.
func (s *Service) WriteComplianceEvidence(ctx context.Context, p model.Principal, framework string, w io.Writer) error {
	controls, err := s.Compliance(ctx, p, framework)
	if err != nil {
		return err
	}
	now := s.now()
	members, err := s.deps.Store.Members(ctx, p.TenantID)
	if err != nil {
		return err
	}
	tokens, err := s.deps.Store.Tokens(ctx, p.TenantID, now)
	if err != nil {
		return err
	}
	verification, err := s.deps.Chains.Verify(ctx, p.TenantID)
	if err != nil {
		return err
	}
	anchors, err := s.deps.Store.Anchors(ctx, p.TenantID, 100)
	if err != nil {
		return err
	}
	reviews, err := s.deps.Store.Reviews(ctx, p.TenantID, 20)
	if err != nil {
		return err
	}
	s.record(ctx, p, "compliance.evidence_export", "organization", p.TenantID, map[string]any{"framework": framework})
	z := zip.NewWriter(w)
	files := []struct {
		name string
		v    any
	}{{"controls.json", controls}, {"members.json", members}, {"api-tokens.json", tokens}, {"chain-verification.json", verification},
		{"anchors.json", anchors}, {"reviews.json", reviews}}
	for _, f := range files {
		out, err := z.Create(f.name)
		if err != nil {
			return err
		}
		if err := writeJSON(out, f.v); err != nil {
			return err
		}
	}
	out, err := z.Create("audit.jsonl")
	if err != nil {
		return err
	}
	after := int64(0)
	for i := 0; i < 20; i++ {
		page, err := s.deps.Store.Export(ctx, p.TenantID, after, now.AddDate(0, 0, -90), time.Time{}, 10000)
		if err != nil {
			return err
		}
		for _, e := range page {
			if err := writeLine(out, e); err != nil {
				return err
			}
		}
		if len(page) < 10000 {
			break
		}
		after = page[len(page)-1].Seq
	}
	readme, err := z.Create("README.md")
	if err != nil {
		return err
	}
	fmt.Fprintf(readme, "# %s evidence\n\nExported %s for organization %s.\n\n- controls.json: each control, how the platform meets it, and the organization's state now.\n- members.json and api-tokens.json: who has access (an access review).\n- chain-verification.json, anchors.json, audit.jsonl: the audit chain, its anchors and the last 90 days (check with osp audit-verify).\n- reviews.json: exception reviews and their sign-offs.\n",
		controls.Name, now.Format(time.RFC3339), p.TenantID)
	return z.Close()
}

// Undertaking drafts the SEC 17a-4(f)(3)(vii) undertaking letter for a
// designated executive officer (DEO) or third party (D3P), to sign.
func (s *Service) Undertaking(ctx context.Context, p model.Principal, role, name, title, firm string) (string, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermAuditRead, "compliance.undertaking", "organization", p.TenantID); err != nil {
		return "", err
	}
	role = strings.ToUpper(role)
	if (role != "DEO" && role != "D3P") || strings.TrimSpace(name) == "" || strings.TrimSpace(firm) == "" {
		return "", invalid("role is DEO or D3P, with a name and a firm")
	}
	who := "the undersigned, a designated executive officer of " + firm
	if role == "D3P" {
		who = "the undersigned designated third party, on behalf of " + firm
	}
	return fmt.Sprintf(`UNDERTAKING UNDER SEC RULE 17a-4(f)(3)(vii)

Date: %s

To: the Securities and Exchange Commission and the examining authority of %s

I, %s (%s), %s, undertake to furnish promptly to the Commission, its designees or representatives, any self-regulatory organization of which %s is a member, or any State securities regulator having jurisdiction over it, upon reasonable request, such information as is deemed necessary by their staff to download information kept on the electronic recordkeeping system of %s (the object storage and dataset platform, organization %s) to any acceptable medium under Rule 17a-4.

I further undertake to take reasonable steps to provide access to information contained on that system, including, as appropriate, arrangements for the downloading of any record required to be maintained and preserved under Rules 17a-3 and 17a-4 in a format acceptable to the Commission's staff or such other representatives. Records are produced as evidence packs (manifests with SHA-256, chain of custody and load files) and audit chain exports, and regulators may be given logged, time-limited read access.

Signature: ______________________

%s, %s
`, s.now().Format("2 January 2006"), firm, name, title, who, firm, firm, p.TenantID, name, title), nil
}

// RegulatorAccess issues a read-only, time-limited API token for a
// regulator or examiner. Its creation and every read it makes are
// audited.
func (s *Service) RegulatorAccess(ctx context.Context, p model.Principal, name string, days int, datasets []string) (model.APIToken, string, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermOrgManage, "compliance.regulator_access", "organization", p.TenantID); err != nil {
		return model.APIToken{}, "", err
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 80 || days < 1 || days > 30 {
		return model.APIToken{}, "", invalid("a regulator's name of 1-80 characters, for 1 to 30 days")
	}
	t, secret, err := s.deps.Access.CreateTokenFor(ctx, p, access.TokenInput{Name: "Regulator: " + name,
		Permissions: []string{"datasets.read", "assets.read", "assets.download", "audit.read"}, Datasets: datasets,
		ExpiresIn: time.Duration(days) * 24 * time.Hour})
	if err != nil {
		return model.APIToken{}, "", err
	}
	s.record(ctx, p, "compliance.regulator_access", "api_token", t.ID, map[string]any{"name": name, "days": days, "datasets": datasets})
	return t, secret, nil
}

// AsOf lists a namespace's objects as they stood at a moment: each
// object's version then (recreate as of time T).
func (s *Service) AsOf(ctx context.Context, p model.Principal, namespace string, at time.Time) ([]postgres.AsOfRow, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermAssetsRead, "catalog.as_of", "catalog", namespace); err != nil {
		return nil, err
	}
	if at.IsZero() || at.After(s.now()) {
		return nil, invalid("as_of is a time in the past")
	}
	out, err := s.deps.Store.AsOf(ctx, p.TenantID, namespace, at, 100000)
	if out == nil {
		out = []postgres.AsOfRow{}
	}
	return out, err
}
