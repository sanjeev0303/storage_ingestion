package model

import (
	"errors"
	"net/netip"
	"slices"
	"time"
)

// Asset sensitivity, from least to most restricted.
const (
	ClassPublic       = "PUBLIC"
	ClassInternal     = "INTERNAL"
	ClassConfidential = "CONFIDENTIAL"
	ClassRestricted   = "RESTRICTED"
)

var classRank = map[string]int{"": 0, ClassPublic: 0, ClassInternal: 1, ClassConfidential: 2, ClassRestricted: 3}

func ValidClassification(c string) bool {
	_, ok := classRank[c]
	return ok && c != ""
}

// ClassForFindings is the class content findings call for: RESTRICTED
// for credentials, CONFIDENTIAL for personal data an external engine
// found; "" when they call for none.
func ClassForFindings(codes []string) string {
	class := ""
	for _, code := range codes {
		switch {
		case len(code) > 7 && code[:7] == "secret.":
			return ClassRestricted
		case len(code) > 13 && code[:13] == "external.pii.", len(code) > 13 && code[:13] == "external.phi.":
			class = ClassConfidential
		}
	}
	return class
}

// MoreRestricted reports whether a is more restricted than b.
func MoreRestricted(a, b string) bool { return classRank[a] > classRank[b] }

var (
	ErrAPITokenNotFound  = errors.New("API token not found")
	ErrInvalidAPIToken   = errors.New("invalid API token")
	ErrLinksDisabled     = errors.New("share links are turned off for this organization")
	ErrLinkClassified    = errors.New("the asset's classification does not allow share links")
	ErrEgressBudget      = errors.New("the organization's download budget is used up")
	ErrInvalidPolicyBody = errors.New("invalid access policy")
	ErrAlertNotFound     = errors.New("alert not found")
)

// APIToken is a member's credential for scripts and vendors. Its
// permissions are capped by the member's role when used; Datasets and
// Networks, when set, confine it.
type APIToken struct {
	ID          string     `json:"id"`
	TenantID    string     `json:"-"`
	UserID      string     `json:"user_id"`
	Name        string     `json:"name"`
	TokenHash   string     `json:"-"`
	Hint        string     `json:"hint"`
	Permissions Permission `json:"-"`
	Datasets    []string   `json:"datasets"`
	Networks    []string   `json:"networks"`
	AllowLinks  bool       `json:"allow_links"`
	ExpiresAt   time.Time  `json:"expires_at"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	LastUsedIP  string     `json:"last_used_ip,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
	RevokedBy   string     `json:"revoked_by,omitempty"`
}

// Usable reports whether the token may be used at now from ip ("" when
// unknown, which a network-bound token refuses).
func (t APIToken) Usable(now time.Time, ip string) bool {
	if t.RevokedAt != nil || !now.Before(t.ExpiresAt) {
		return false
	}
	if len(t.Networks) == 0 {
		return true
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	for _, n := range t.Networks {
		if prefix, err := netip.ParsePrefix(n); err == nil && prefix.Contains(addr.Unmap()) {
			return true
		}
	}
	return false
}

// Scope is what confines a principal beyond its permissions: the dataset
// namespaces it may read (nil: all), whether it may create share links,
// and the API token it came from.
type Scope struct {
	TokenID    string
	Datasets   []string
	NoLinks    bool
	Restricted bool // confined to some datasets
}

// Allows reports whether an asset in dataset is inside the scope.
func (s Scope) Allows(dataset string) bool {
	return !s.Restricted || slices.Contains(s.Datasets, dataset)
}

// AccessPolicy is an organization's rules for links, egress and anomalies.
type AccessPolicy struct {
	TenantID               string        `json:"-"`
	LinksEnabled           bool          `json:"links_enabled"`
	MaxLinkTTL             time.Duration `json:"-"`
	EgressMonthlyBytes     int64         `json:"egress_monthly_bytes"`
	EgressDailyMemberBytes int64         `json:"egress_daily_member_bytes"`
	EgressAction           string        `json:"egress_action"`
	MassLinkThreshold      int           `json:"mass_link_threshold"`
	AnomalyAction          string        `json:"anomaly_action"`
	UpdatedBy              string        `json:"updated_by,omitempty"`
	UpdatedAt              time.Time     `json:"updated_at,omitempty"`
}

// DefaultAccessPolicy applies until an organization sets its own.
func DefaultAccessPolicy(tenantID string) AccessPolicy {
	return AccessPolicy{TenantID: tenantID, LinksEnabled: true, EgressAction: "WARN", MassLinkThreshold: 300, AnomalyAction: "ALERT"}
}

func (p AccessPolicy) Validate() error {
	bad := func(s string) error { return errors.Join(ErrInvalidPolicyBody, errors.New(s)) }
	switch {
	case p.MaxLinkTTL < 0 || p.MaxLinkTTL > 30*24*time.Hour:
		return bad("max_link_ttl must be between 0 (no limit) and 720h")
	case p.EgressMonthlyBytes < 0 || p.EgressDailyMemberBytes < 0:
		return bad("budgets cannot be negative")
	case p.EgressAction != "WARN" && p.EgressAction != "BLOCK":
		return bad("egress_action must be WARN or BLOCK")
	case p.AnomalyAction != "ALERT" && p.AnomalyAction != "REVOKE_LINKS":
		return bad("anomaly_action must be ALERT or REVOKE_LINKS")
	case p.MassLinkThreshold < 10 || p.MassLinkThreshold > 100000:
		return bad("mass_link_threshold must be between 10 and 100000")
	}
	return nil
}

// Security alert kinds.
const (
	AlertReadSpike  = "READ_SPIKE"
	AlertMassLinks  = "MASS_LINKS"
	AlertNewNetwork = "NEW_NETWORK"
	AlertEgress     = "EGRESS_BUDGET"
)

type SecurityAlert struct {
	ID             string         `json:"id"`
	TenantID       string         `json:"-"`
	PrincipalID    string         `json:"principal_id"`
	Kind           string         `json:"kind"`
	Detail         string         `json:"detail"`
	Metrics        map[string]any `json:"metrics"`
	ActionTaken    string         `json:"action_taken,omitempty"`
	State          string         `json:"state"`
	DedupKey       string         `json:"-"`
	CreatedAt      time.Time      `json:"created_at"`
	AcknowledgedBy string         `json:"acknowledged_by,omitempty"`
	AcknowledgedAt *time.Time     `json:"acknowledged_at,omitempty"`
}

// ShareLink is a download token as the link inventory shows it.
type ShareLink struct {
	ID          string     `json:"id"`
	AssetID     string     `json:"asset_id"`
	FileName    string     `json:"file_name"`
	PrincipalID string     `json:"principal_id"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	Uses        int        `json:"uses"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
	RevokedBy   string     `json:"revoked_by,omitempty"`
}

// EgressUse is one member's downloads over a period.
type EgressUse struct {
	PrincipalID string `json:"principal_id"`
	Bytes       int64  `json:"bytes"`
	Reads       int64  `json:"reads"`
}
