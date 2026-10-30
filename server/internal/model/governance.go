package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Object Lock modes. GOVERNANCE can be lifted by an identity allowed to
// bypass it; COMPLIANCE by nobody until it ends.
const (
	LockGovernance = "GOVERNANCE"
	LockCompliance = "COMPLIANCE"
)

// ScopeQuery is a hold by filter: it covers every asset that matches,
// including assets created after it was placed.
const ScopeQuery = "QUERY"

var (
	ErrComplianceLocked  = errors.New("a COMPLIANCE policy cannot be shortened, downgraded or removed")
	ErrApprovalNotFound  = errors.New("approval not found")
	ErrApprovalDecided   = errors.New("the approval was already decided")
	ErrSelfApproval      = errors.New("a second member must approve; the requester cannot")
	ErrGovernanceInvalid = errors.New("invalid governance setting")
)

// ApprovalRequiredError: the change was not made; it waits as approval ID.
type ApprovalRequiredError struct{ ID string }

func (e *ApprovalRequiredError) Error() string {
	return "a second member's approval is required; requested as " + e.ID
}

// HoldQuery selects assets for a QUERY hold. Every field set must match.
type HoldQuery struct {
	Dataset           string            `json:"dataset,omitempty"`
	PathPrefix        string            `json:"path_prefix,omitempty"`
	ContentTypePrefix string            `json:"content_type_prefix,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
	CreatedAfter      *time.Time        `json:"created_after,omitempty"`
	CreatedBefore     *time.Time        `json:"created_before,omitempty"`
}

func (q HoldQuery) Validate() error {
	if q.Dataset == "" && q.PathPrefix == "" && q.ContentTypePrefix == "" && len(q.Metadata) == 0 && q.CreatedAfter == nil && q.CreatedBefore == nil {
		return errors.New("a query hold needs at least one condition")
	}
	if q.Dataset != "" && !ValidDataset(q.Dataset) {
		return errors.New("invalid dataset name")
	}
	if len(q.PathPrefix) > 1024 || len(q.ContentTypePrefix) > 100 || len(q.Metadata) > 20 {
		return errors.New("the query is too long")
	}
	for k, v := range q.Metadata {
		if k == "" || len(k) > 100 || len(v) > 500 {
			return errors.New("metadata conditions need a key of 1-100 characters and a value of at most 500")
		}
	}
	if q.CreatedAfter != nil && q.CreatedBefore != nil && !q.CreatedAfter.Before(*q.CreatedBefore) {
		return errors.New("created_after must be before created_before")
	}
	return nil
}

func (q HoldQuery) JSON() []byte {
	out, _ := json.Marshal(q)
	return out
}

// Governance profiles.
const (
	ProfileStandard  = "STANDARD"
	ProfileGxPVault  = "GXP_VAULT"
	ProfileSEC17a4AT = "SEC_17A4_AUDIT_TRAIL"
)

// GovernanceSettings is how an organization governs its records.
type GovernanceSettings struct {
	TenantID string `json:"-"`
	Profile  string `json:"profile"`
	// TwoPerson: shortening retention, releasing a hold, bulk deletes and
	// weakening these settings wait for a second member's approval.
	TwoPerson bool `json:"two_person"`
	// ApproveDeletes: every asset deletion by a member needs approval.
	ApproveDeletes bool `json:"approve_deletes"`
	// RetentionFloor: no policy keeps records shorter than this.
	RetentionFloor time.Duration `json:"-"`
	UpdatedBy      string        `json:"updated_by,omitempty"`
	UpdatedAt      time.Time     `json:"updated_at,omitempty"`
}

// sixYears is SEC Rule 17a-4(a)'s period for most broker-dealer records.
const sixYears = 6 * 365 * 24 * time.Hour

// ApplyProfile sets what a profile requires; a profile can only add to
// what the organization chose.
func (g GovernanceSettings) ApplyProfile() GovernanceSettings {
	switch g.Profile {
	case ProfileGxPVault:
		// 21 CFR Part 11 and EU Annex 11: no record changes or deletions by
		// one person, every action attributable and explained.
		g.TwoPerson, g.ApproveDeletes = true, true
	case ProfileSEC17a4AT:
		// Rule 17a-4(f)(2)(i)(A), the audit-trail alternative to WORM:
		// originals kept and every change recorded, for six years.
		g.TwoPerson = true
		g.RetentionFloor = max(g.RetentionFloor, sixYears)
	}
	return g
}

// Weaker reports whether next loosens any rule of g.
func (g GovernanceSettings) Weaker(next GovernanceSettings) bool {
	rank := map[string]int{ProfileStandard: 0, ProfileGxPVault: 1, ProfileSEC17a4AT: 1}
	return (g.TwoPerson && !next.TwoPerson) || (g.ApproveDeletes && !next.ApproveDeletes) ||
		next.RetentionFloor < g.RetentionFloor || (g.Profile != next.Profile && rank[next.Profile] <= rank[g.Profile])
}

func (g GovernanceSettings) Validate() error {
	switch g.Profile {
	case ProfileStandard, ProfileGxPVault, ProfileSEC17a4AT:
	default:
		return fmt.Errorf("%w: profile must be STANDARD, GXP_VAULT or SEC_17A4_AUDIT_TRAIL", ErrGovernanceInvalid)
	}
	if g.RetentionFloor < 0 || g.RetentionFloor > 100*365*24*time.Hour {
		return fmt.Errorf("%w: the retention floor must be between 0 and 100 years", ErrGovernanceInvalid)
	}
	return nil
}

// Approval kinds and states.
const (
	ApprovalRetentionChange  = "RETENTION_CHANGE"
	ApprovalHoldRelease      = "HOLD_RELEASE"
	ApprovalAssetDelete      = "ASSET_DELETE"
	ApprovalGovernanceChange = "GOVERNANCE_CHANGE"

	ApprovalPending  = "PENDING"
	ApprovalExecuted = "EXECUTED"
	ApprovalRejected = "REJECTED"
	ApprovalExpired  = "EXPIRED"
	ApprovalFailed   = "FAILED"
)

// Approval is a change waiting for a second member.
type Approval struct {
	ID           string          `json:"id"`
	TenantID     string          `json:"-"`
	Kind         string          `json:"kind"`
	Summary      string          `json:"summary"`
	Payload      json.RawMessage `json:"payload"`
	State        string          `json:"state"`
	Reason       string          `json:"reason"`
	RequestedBy  string          `json:"requested_by"`
	RequestedAt  time.Time       `json:"requested_at"`
	ExpiresAt    time.Time       `json:"expires_at"`
	DecidedBy    string          `json:"decided_by,omitempty"`
	DecidedAt    *time.Time      `json:"decided_at,omitempty"`
	DecisionNote string          `json:"decision_note,omitempty"`
	Result       string          `json:"result,omitempty"`
}

// HoldCoverage is an asset a hold covers, with the lock last applied.
type HoldCoverage struct {
	Asset Asset
	Lock  *ObjectLock
}

// DispositionCertificate records one permanent deletion. SignedBody is
// the exact JSON that BodySHA256 and Signature (Ed25519, base64) cover;
// Body is the same, decoded, for reading.
type DispositionCertificate struct {
	ID         string          `json:"id"`
	TenantID   string          `json:"-"`
	DeletionID string          `json:"deletion_id"`
	AssetID    string          `json:"asset_id"`
	Body       json.RawMessage `json:"body"`
	SignedBody string          `json:"signed_body"`
	BodySHA256 string          `json:"body_sha256"`
	KeyID      string          `json:"key_id"`
	Signature  string          `json:"signature"`
	CreatedAt  time.Time       `json:"created_at"`
}

// RetentionTemplate is a starting point for a records schedule. The
// citations say where each period comes from; counsel confirms they fit.
type RetentionTemplate struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	Citation      string        `json:"citation"`
	MinRetention  time.Duration `json:"-"`
	RetainFromKey string        `json:"retain_from_key,omitempty"`
	LockMode      string        `json:"lock_mode"`
	Note          string        `json:"note"`
}

const day = 24 * time.Hour

var RetentionTemplates = []RetentionTemplate{
	{ID: "sec-17a-4", Name: "SEC Rule 17a-4 broker-dealer records", Citation: "17 CFR 240.17a-4(a), (b)",
		MinRetention: 6 * 365 * day, LockMode: LockCompliance,
		Note: "Six years for most records (three for some under 17a-4(b)); the first two easily accessible. COMPLIANCE meets the WORM option of 17a-4(f)(2)(i)(B); the audit-trail option is the SEC 17a-4 audit trail profile."},
	{ID: "cftc-1-31", Name: "CFTC Regulation 1.31 records", Citation: "17 CFR 1.31(b)",
		MinRetention: 5 * 365 * day, LockMode: LockGovernance,
		Note: "Five years from creation, readily accessible for the first two; oral communications one year."},
	{ID: "cms-imaging", Name: "Medical imaging (CMS minimum, minors from majority)", Citation: "42 CFR 482.26(d)(2); 42 CFR 482.24(b)(1)",
		MinRetention: 5 * 365 * day, RetainFromKey: "majority_date", LockMode: LockGovernance,
		Note: "At least five years; for minors the clock starts at the age of majority, from the majority_date metadata. Many states require longer."},
	{ID: "medicare-advantage", Name: "Medicare Advantage records", Citation: "42 CFR 422.504(d)",
		MinRetention: 10 * 365 * day, LockMode: LockGovernance, Note: "Ten years."},
	{ID: "hipaa-documentation", Name: "HIPAA policies and documentation", Citation: "45 CFR 164.316(b)(2)(i)",
		MinRetention: 6 * 365 * day, LockMode: LockGovernance,
		Note: "Six years from creation or from when last in effect, whichever is later; use retain_from_key for the latter."},
	{ID: "ofcom-tv", Name: "Ofcom television recordings", Citation: "Broadcasting Act 1990 s.11, 1996 s.20; Ofcom licence conditions",
		MinRetention: 90 * day, LockMode: LockGovernance, Note: "Ninety days, produced to Ofcom on request."},
	{ID: "ofcom-radio", Name: "Ofcom radio recordings", Citation: "Broadcasting Act 1990 s.95; Ofcom licence conditions",
		MinRetention: 42 * day, LockMode: LockGovernance, Note: "Forty-two days."},
	{ID: "nara-grs-6-1", Name: "NARA GRS 6.1 email of non-Capstone officials", Citation: "NARA General Records Schedule 6.1, item 011",
		MinRetention: 7 * 365 * day, LockMode: LockGovernance, Note: "Seven years; Capstone officials' email is permanent and transferred to NARA."},
	{ID: "mrc-research", Name: "UKRI MRC research data", Citation: "MRC Retention Framework for Research Data and Records (2023)",
		MinRetention: 10 * 365 * day, RetainFromKey: "study_end_date", LockMode: LockGovernance,
		Note: "At least ten years after the study ends (study_end_date); twenty for clinical and population studies (mrc-clinical)."},
	{ID: "mrc-clinical", Name: "UKRI MRC clinical and population research", Citation: "MRC Retention Framework for Research Data and Records (2023)",
		MinRetention: 20 * 365 * day, RetainFromKey: "study_end_date", LockMode: LockGovernance, Note: "Twenty years after the study ends."},
}

func TemplateByID(id string) (RetentionTemplate, bool) {
	for _, t := range RetentionTemplates {
		if t.ID == id {
			return t, true
		}
	}
	return RetentionTemplate{}, false
}

// MetadataDate reads a date (2006-01-02 or RFC 3339) from metadata.
func MetadataDate(metadata map[string]string, key string) (time.Time, bool) {
	raw, ok := metadata[key]
	if !ok {
		return time.Time{}, false
	}
	raw = strings.TrimSpace(raw)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
