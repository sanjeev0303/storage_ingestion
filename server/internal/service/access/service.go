// Package access governs who reads what and how (docs/access.md): API
// tokens and vendor workspaces, share-link policy and inventory, egress
// budgets, sensitivity classification, and alerts on unusual reads.
package access

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
	"github.com/sanjeev0303/object-storage-platform/internal/service/identity"
)

// Store is the access repository (postgres.AccessRepository).
type Store interface {
	CreateToken(ctx context.Context, t model.APIToken) error
	ListTokens(ctx context.Context, tenantID, userID string) ([]model.APIToken, error)
	GetToken(ctx context.Context, tenantID, id string) (model.APIToken, error)
	RevokeToken(ctx context.Context, tenantID, id, by string, at time.Time) error
	Policy(ctx context.Context, tenantID string) (model.AccessPolicy, error)
	PutPolicy(ctx context.Context, p model.AccessPolicy) error
	AddEgress(ctx context.Context, tenantID, principalID string, bytes int64, at time.Time) error
	Egress(ctx context.Context, tenantID, principalID string, at time.Time) (int64, int64, error)
	EgressByMember(ctx context.Context, tenantID string, from time.Time) ([]model.EgressUse, error)
	Links(ctx context.Context, tenantID, principalID string, active bool, now time.Time, limit int) ([]model.ShareLink, error)
	RevokeLinks(ctx context.Context, tenantID string, f postgres.LinkFilter, by string, at time.Time) (int64, error)
	LinkUsed(ctx context.Context, tokenHash string, at time.Time) error
	SetClassification(ctx context.Context, tenantID, assetID, class string, raiseOnly bool) error
	AddAlert(ctx context.Context, a model.SecurityAlert) (bool, error)
	Alerts(ctx context.Context, tenantID, state string, limit int) ([]model.SecurityAlert, error)
	Acknowledge(ctx context.Context, tenantID, id, by string, at time.Time) (model.SecurityAlert, error)
	RecentReads(ctx context.Context, from time.Time) ([]postgres.ReadCount, error)
	HourlyReads(ctx context.Context, tenantID, principalID string, from, to time.Time) (map[time.Time]int64, time.Time, error)
	SourceIPs(ctx context.Context, tenantID, principalID string, from, to time.Time) ([]string, error)
}

type Service struct {
	store Store
	audit interfaces.AuditRecorder
	log   *slog.Logger
	now   func() time.Time
}

func NewService(store Store, recorder interfaces.AuditRecorder, log *slog.Logger) *Service {
	return &Service{store: store, audit: recorder, log: log, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) record(ctx context.Context, p model.Principal, action, kind, id string, details map[string]any) {
	if s.audit == nil {
		return
	}
	e := audit.From(ctx, p, action, kind, id)
	e.Details = details
	s.audit.Record(ctx, e)
}

// ─── API tokens ──────────────────────────────────────────────────────

// TokenInput describes a new token. Datasets confine it to dataset
// namespaces: a vendor workspace.
type TokenInput struct {
	Name        string
	Permissions []string
	Datasets    []string
	Networks    []string
	ExpiresIn   time.Duration
	AllowLinks  bool
}

const maxTokenLife = 365 * 24 * time.Hour

// CreateTokenFor issues a token in the caller's name. It is created from a
// session only (a token cannot mint tokens), its permissions are a subset
// of the caller's, and the raw token is returned once.
func (s *Service) CreateTokenFor(ctx context.Context, p model.Principal, in TokenInput) (model.APIToken, string, error) {
	if err := audit.Require(ctx, s.audit, p, model.PermOrgRead, "api_token.create", "api_token", ""); err != nil {
		return model.APIToken{}, "", err
	}
	bad := func(detail string) (model.APIToken, string, error) {
		return model.APIToken{}, "", fmt.Errorf("%w: %s", model.ErrInvalidAPIToken, detail)
	}
	if p.Scope.TokenID != "" {
		return model.APIToken{}, "", model.ErrForbidden
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 100 {
		return bad("a name of 1-100 characters")
	}
	perms, err := model.ParsePermissions(in.Permissions)
	if err != nil || perms == 0 {
		return bad("permissions must name at least one permission")
	}
	if !p.Permissions.Has(perms) {
		return model.APIToken{}, "", model.ErrEscalation
	}
	if in.ExpiresIn == 0 {
		in.ExpiresIn = 90 * 24 * time.Hour
	}
	if in.ExpiresIn < time.Hour || in.ExpiresIn > maxTokenLife {
		return bad("expires_in must be between 1h and 8760h")
	}
	for _, d := range in.Datasets {
		if !model.ValidDataset(d) {
			return bad("invalid dataset " + d)
		}
	}
	for i, n := range in.Networks {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(n))
		if err != nil {
			addr, err2 := netip.ParseAddr(strings.TrimSpace(n))
			if err2 != nil {
				return bad("networks are CIDRs such as 203.0.113.0/24")
			}
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		in.Networks[i] = prefix.Masked().String()
	}
	if len(in.Datasets) > 50 || len(in.Networks) > 20 {
		return bad("at most 50 datasets and 20 networks")
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return model.APIToken{}, "", err
	}
	raw := identity.APITokenPrefix + base64.RawURLEncoding.EncodeToString(secret[:])
	now := s.now()
	t := model.APIToken{ID: event.NewID(), TenantID: p.TenantID, UserID: p.UserID, Name: in.Name,
		TokenHash: identity.HashAPIToken(raw), Hint: "…" + raw[len(raw)-4:], Permissions: perms,
		Datasets: in.Datasets, Networks: in.Networks, AllowLinks: in.AllowLinks && len(in.Datasets) == 0,
		ExpiresAt: now.Add(in.ExpiresIn), CreatedAt: now}
	if err := s.store.CreateToken(ctx, t); err != nil {
		return model.APIToken{}, "", err
	}
	s.record(ctx, p, "api_token.create", "api_token", t.ID, map[string]any{"name": t.Name, "permissions": perms.Names(),
		"datasets": t.Datasets, "networks": t.Networks, "expires_at": t.ExpiresAt})
	return t, raw, nil
}

// TokensFor lists the caller's tokens, or every member's with
// members.manage.
func (s *Service) TokensFor(ctx context.Context, p model.Principal, all bool) ([]model.APIToken, error) {
	user := p.UserID
	if all {
		if err := audit.Require(ctx, s.audit, p, model.PermMembersManage, "api_token.read", "api_token", ""); err != nil {
			return nil, err
		}
		user = ""
	}
	return s.store.ListTokens(ctx, p.TenantID, user)
}

// RevokeTokenFor revokes one of the caller's tokens, or anyone's with
// members.manage.
func (s *Service) RevokeTokenFor(ctx context.Context, p model.Principal, id string) error {
	t, err := s.store.GetToken(ctx, p.TenantID, id)
	if err != nil {
		return err
	}
	if t.UserID != p.UserID {
		if err := audit.Require(ctx, s.audit, p, model.PermMembersManage, "api_token.revoke", "api_token", id); err != nil {
			return err
		}
	}
	if err := s.store.RevokeToken(ctx, p.TenantID, id, "user:"+p.UserID, s.now()); err != nil {
		return err
	}
	s.record(ctx, p, "api_token.revoke", "api_token", id, nil)
	return nil
}

// ─── Policy ──────────────────────────────────────────────────────────

func (s *Service) PolicyFor(ctx context.Context, p model.Principal) (model.AccessPolicy, error) {
	if err := audit.Require(ctx, s.audit, p, model.PermOrgRead, "access_policy.read", "organization", p.TenantID); err != nil {
		return model.AccessPolicy{}, err
	}
	return s.store.Policy(ctx, p.TenantID)
}

func (s *Service) SetPolicyFor(ctx context.Context, p model.Principal, policy model.AccessPolicy) (model.AccessPolicy, error) {
	if err := audit.Require(ctx, s.audit, p, model.PermOrgManage, "access_policy.update", "organization", p.TenantID); err != nil {
		return model.AccessPolicy{}, err
	}
	if err := policy.Validate(); err != nil {
		return model.AccessPolicy{}, err
	}
	policy.TenantID, policy.UpdatedBy, policy.UpdatedAt = p.TenantID, "user:"+p.UserID, s.now()
	if err := s.store.PutPolicy(ctx, policy); err != nil {
		return model.AccessPolicy{}, err
	}
	s.record(ctx, p, "access_policy.update", "organization", p.TenantID, map[string]any{"links_enabled": policy.LinksEnabled,
		"egress_action": policy.EgressAction, "anomaly_action": policy.AnomalyAction})
	return policy, nil
}

// ─── Links ───────────────────────────────────────────────────────────

// LinksFor lists share links: the caller's, or everyone's with
// assets.delete (who may revoke them).
func (s *Service) LinksFor(ctx context.Context, p model.Principal, all, active bool) ([]model.ShareLink, error) {
	if err := audit.Require(ctx, s.audit, p, model.PermAssetsRead, "share_link.read", "share_link", ""); err != nil {
		return nil, err
	}
	who := p.UserID
	if all {
		if err := p.Require(model.PermAssetsDelete); err != nil {
			return nil, err
		}
		who = ""
	}
	return s.store.Links(ctx, p.TenantID, who, active, s.now(), 1000)
}

// RevokeLinksFor revokes live links in bulk. Members revoke their own;
// assets.delete revokes anyone's.
func (s *Service) RevokeLinksFor(ctx context.Context, p model.Principal, f postgres.LinkFilter) (int64, error) {
	if err := audit.Require(ctx, s.audit, p, model.PermAssetsRead, "share_link.revoke", "share_link", ""); err != nil {
		return 0, err
	}
	if !p.Can(model.PermAssetsDelete) {
		if f.PrincipalID != "" && f.PrincipalID != p.UserID {
			return 0, model.ErrForbidden
		}
		f.PrincipalID = p.UserID
	}
	n, err := s.store.RevokeLinks(ctx, p.TenantID, f, "user:"+p.UserID, s.now())
	if err == nil {
		s.record(ctx, p, "share_link.revoke", "share_link", "", map[string]any{"revoked": n, "all": f.All,
			"principal_id": f.PrincipalID, "assets": len(f.AssetIDs), "links": len(f.IDs)})
	}
	return n, err
}

// ─── Classification ──────────────────────────────────────────────────

func (s *Service) ClassifyFor(ctx context.Context, p model.Principal, assetID, class string) error {
	if err := audit.Require(ctx, s.audit, p, model.PermCatalogWrite, "asset.classify", "asset", assetID); err != nil {
		return err
	}
	if !model.ValidClassification(class) {
		return fmt.Errorf("%w: classification must be PUBLIC, INTERNAL, CONFIDENTIAL or RESTRICTED", model.ErrInvalidPolicyBody)
	}
	if err := s.store.SetClassification(ctx, p.TenantID, assetID, class, false); err != nil {
		return err
	}
	s.record(ctx, p, "asset.classify", "asset", assetID, map[string]any{"classification": class})
	return nil
}

// ─── Delivery hooks ──────────────────────────────────────────────────

// Classification caps on download link lifetimes.
var classTTL = map[string]time.Duration{model.ClassConfidential: time.Hour, model.ClassRestricted: 15 * time.Minute}

// LinkTTL is the longest a link to the asset may live (0: no cap), or an
// error when the caller may not create this kind of link at all.
func (s *Service) LinkTTL(ctx context.Context, p model.Principal, asset model.Asset, shareable bool) (time.Duration, error) {
	policy, err := s.store.Policy(ctx, p.TenantID)
	if err != nil {
		return 0, err
	}
	if shareable {
		switch {
		case !policy.LinksEnabled, p.Scope.NoLinks:
			return 0, model.ErrLinksDisabled
		case asset.Classification == model.ClassRestricted:
			return 0, model.ErrLinkClassified
		}
	}
	limit := policy.MaxLinkTTL
	if c := classTTL[asset.Classification]; c > 0 && (limit == 0 || c < limit) {
		limit = c
	}
	return limit, nil
}

// Egress checks that handing out bytes keeps within the budgets, and
// counts them. Over budget, WARN alerts once a day and lets it through;
// BLOCK refuses.
func (s *Service) Egress(ctx context.Context, p model.Principal, bytes int64) error {
	policy, err := s.store.Policy(ctx, p.TenantID)
	if err != nil {
		return err
	}
	now := s.now()
	if policy.EgressMonthlyBytes > 0 || policy.EgressDailyMemberBytes > 0 {
		month, day, err := s.store.Egress(ctx, p.TenantID, p.UserID, now)
		if err != nil {
			return err
		}
		over := ""
		switch {
		case policy.EgressMonthlyBytes > 0 && month+bytes > policy.EgressMonthlyBytes:
			over = fmt.Sprintf("the organization's monthly budget of %d bytes (used %d)", policy.EgressMonthlyBytes, month)
		case policy.EgressDailyMemberBytes > 0 && day+bytes > policy.EgressDailyMemberBytes:
			over = fmt.Sprintf("the daily budget of %d bytes per member (used %d)", policy.EgressDailyMemberBytes, day)
		}
		if over != "" {
			action := "warned"
			if policy.EgressAction == "BLOCK" {
				action = "refused"
			}
			s.alert(ctx, model.SecurityAlert{TenantID: p.TenantID, PrincipalID: p.UserID, Kind: model.AlertEgress,
				Detail: "downloads past " + over, ActionTaken: action,
				Metrics:  map[string]any{"month_bytes": month, "day_bytes": day, "requested": bytes},
				DedupKey: fmt.Sprintf("egress:%s:%s", p.UserID, now.Format("2006-01-02"))})
			if policy.EgressAction == "BLOCK" {
				return model.ErrEgressBudget
			}
		}
	}
	return s.store.AddEgress(ctx, p.TenantID, p.UserID, bytes, now)
}

// LinkUsed counts a download through a share link.
func (s *Service) LinkUsed(ctx context.Context, tokenHash string) {
	_ = s.store.LinkUsed(ctx, tokenHash, s.now())
}

// ─── Alerts and egress reports ───────────────────────────────────────

func (s *Service) alert(ctx context.Context, a model.SecurityAlert) bool {
	a.ID, a.CreatedAt = event.NewID(), s.now()
	added, err := s.store.AddAlert(ctx, a)
	if err != nil {
		s.log.Warn("security alert not recorded", "kind", a.Kind, "tenant_id", a.TenantID, "error", err)
	}
	return added
}

func (s *Service) AlertsFor(ctx context.Context, p model.Principal, state string) ([]model.SecurityAlert, error) {
	if err := audit.Require(ctx, s.audit, p, model.PermAuditRead, "security_alert.read", "security_alert", ""); err != nil {
		return nil, err
	}
	return s.store.Alerts(ctx, p.TenantID, state, 500)
}

func (s *Service) AcknowledgeFor(ctx context.Context, p model.Principal, id string) (model.SecurityAlert, error) {
	if err := audit.Require(ctx, s.audit, p, model.PermAuditRead, "security_alert.acknowledge", "security_alert", id); err != nil {
		return model.SecurityAlert{}, err
	}
	return s.store.Acknowledge(ctx, p.TenantID, id, "user:"+p.UserID, s.now())
}

// EgressReport is this month's downloads against the budgets.
type EgressReport struct {
	MonthBytes  int64             `json:"month_bytes"`
	MonthBudget int64             `json:"month_budget"`
	DailyBudget int64             `json:"daily_member_budget"`
	Action      string            `json:"action"`
	Members     []model.EgressUse `json:"members"`
}

func (s *Service) EgressFor(ctx context.Context, p model.Principal) (EgressReport, error) {
	if err := audit.Require(ctx, s.audit, p, model.PermOrgRead, "egress.read", "organization", p.TenantID); err != nil {
		return EgressReport{}, err
	}
	policy, err := s.store.Policy(ctx, p.TenantID)
	if err != nil {
		return EgressReport{}, err
	}
	now := s.now()
	members, err := s.store.EgressByMember(ctx, p.TenantID, time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		return EgressReport{}, err
	}
	r := EgressReport{MonthBudget: policy.EgressMonthlyBytes, DailyBudget: policy.EgressDailyMemberBytes, Action: policy.EgressAction,
		Members: append([]model.EgressUse{}, members...)}
	for _, m := range members {
		r.MonthBytes += m.Bytes
	}
	return r, nil
}

// ─── Anomaly detection ───────────────────────────────────────────────

// DetectStats counts one detection pass.
type DetectStats struct {
	Members int `json:"members"`
	Alerts  int `json:"alerts"`
}

const (
	massLinkWindow  = 5 * time.Minute
	spikeWindow     = time.Hour
	baselineDays    = 14
	networkDays     = 30
	minHistoryDays  = 7
	minSpikeReads   = 50
	spikeDeviations = 4.0
)

// Detect looks at the last hour's reads of every member who read, against
// each one's own history: many links in a few minutes, an hour far above
// their usual hours, or reads from a network never seen for them. Each
// finding is one alert per member and hour; with the REVOKE_LINKS policy
// the member's live share links are revoked.
func (s *Service) Detect(ctx context.Context) (DetectStats, error) {
	var stats DetectStats
	now := s.now()
	hour := now.Truncate(time.Hour)
	recent, err := s.store.RecentReads(ctx, now.Add(-spikeWindow))
	if err != nil {
		return stats, err
	}
	links, err := s.store.RecentReads(ctx, now.Add(-massLinkWindow))
	if err != nil {
		return stats, err
	}
	burst := map[string]int64{}
	for _, l := range links {
		burst[l.TenantID+"/"+l.PrincipalID] = l.Links
	}
	policies := map[string]model.AccessPolicy{}
	for _, m := range recent {
		if ctx.Err() != nil {
			break
		}
		stats.Members++
		policy, ok := policies[m.TenantID]
		if !ok {
			if policy, err = s.store.Policy(ctx, m.TenantID); err != nil {
				return stats, err
			}
			policies[m.TenantID] = policy
		}
		var found []model.SecurityAlert
		if n := burst[m.TenantID+"/"+m.PrincipalID]; n >= int64(policy.MassLinkThreshold) {
			found = append(found, model.SecurityAlert{Kind: model.AlertMassLinks,
				Detail:  fmt.Sprintf("%d download links in %s, over the threshold of %d", n, massLinkWindow, policy.MassLinkThreshold),
				Metrics: map[string]any{"links": n, "window_seconds": massLinkWindow.Seconds(), "threshold": policy.MassLinkThreshold}})
		}
		spike, err := s.spike(ctx, m, now)
		if err != nil {
			return stats, err
		}
		if spike != nil {
			found = append(found, *spike)
		}
		fresh, err := s.newNetworks(ctx, m, now)
		if err != nil {
			return stats, err
		}
		if fresh != nil {
			found = append(found, *fresh)
		}
		for _, a := range found {
			a.TenantID, a.PrincipalID = m.TenantID, m.PrincipalID
			a.DedupKey = fmt.Sprintf("%s:%s:%s", strings.ToLower(a.Kind), m.PrincipalID, hour.Format(time.RFC3339))
			if policy.AnomalyAction == "REVOKE_LINKS" {
				n, err := s.store.RevokeLinks(ctx, m.TenantID, postgres.LinkFilter{PrincipalID: m.PrincipalID}, "security:"+a.Kind, now)
				if err == nil {
					a.ActionTaken = fmt.Sprintf("revoked %d live share links", n)
				}
			}
			if s.alert(ctx, a) {
				stats.Alerts++
				s.log.Warn("read anomaly", "kind", a.Kind, "tenant_id", m.TenantID, "principal_id", m.PrincipalID, "detail", a.Detail)
			}
		}
	}
	return stats, nil
}

// spike compares the last hour with the member's hourly reads over the
// days before: far above their mean (in standard deviations), and above
// minSpikeReads, once they have a week of history.
func (s *Service) spike(ctx context.Context, m postgres.ReadCount, now time.Time) (*model.SecurityAlert, error) {
	if m.Reads < minSpikeReads {
		return nil, nil
	}
	from, to := now.Add(-baselineDays*24*time.Hour), now.Add(-spikeWindow)
	hours, first, err := s.store.HourlyReads(ctx, m.TenantID, m.PrincipalID, from, to)
	if err != nil || first.IsZero() || now.Sub(first) < minHistoryDays*24*time.Hour {
		return nil, err
	}
	span := to.Sub(maxTime(from, first)).Hours()
	if span < 24 {
		return nil, nil
	}
	n := math.Floor(span)
	var sum, sq float64
	for _, c := range hours {
		sum += float64(c)
	}
	mean := sum / n
	for _, c := range hours {
		sq += (float64(c) - mean) * (float64(c) - mean)
	}
	sq += (n - float64(len(hours))) * mean * mean // the quiet hours count too
	std := math.Sqrt(sq / n)
	limit := mean + spikeDeviations*math.Max(std, 1)
	if float64(m.Reads) <= limit {
		return nil, nil
	}
	return &model.SecurityAlert{Kind: model.AlertReadSpike,
		Detail: fmt.Sprintf("%d reads in the last hour; this member usually reads %.1f an hour (limit %.0f)", m.Reads, mean, limit),
		Metrics: map[string]any{"reads": m.Reads, "hourly_mean": math.Round(mean*10) / 10, "hourly_std": math.Round(std*10) / 10,
			"limit": math.Round(limit)}}, nil
}

// newNetworks finds networks (IPv4 /24, IPv6 /48) the member read from in
// the last hour and never in the month before.
func (s *Service) newNetworks(ctx context.Context, m postgres.ReadCount, now time.Time) (*model.SecurityAlert, error) {
	recent, err := s.store.SourceIPs(ctx, m.TenantID, m.PrincipalID, now.Add(-spikeWindow), now.Add(time.Minute))
	if err != nil || len(recent) == 0 {
		return nil, err
	}
	before, err := s.store.SourceIPs(ctx, m.TenantID, m.PrincipalID, now.Add(-networkDays*24*time.Hour), now.Add(-spikeWindow))
	if err != nil || len(before) == 0 {
		return nil, err // no history: nothing is new yet
	}
	_, first, err := s.store.HourlyReads(ctx, m.TenantID, m.PrincipalID, now, now)
	if err != nil || first.IsZero() || now.Sub(first) < minHistoryDays*24*time.Hour {
		return nil, err
	}
	known := map[string]bool{}
	for _, ip := range before {
		known[network(ip)] = true
	}
	var fresh []string
	for _, ip := range recent {
		if n := network(ip); n != "" && !known[n] {
			known[n] = true
			fresh = append(fresh, n)
		}
	}
	if len(fresh) == 0 {
		return nil, nil
	}
	sort.Strings(fresh)
	return &model.SecurityAlert{Kind: model.AlertNewNetwork,
		Detail:  "reads from a network not seen for this member in 30 days: " + strings.Join(fresh, ", "),
		Metrics: map[string]any{"networks": fresh, "known_networks": len(known) - len(fresh)}}, nil
}

func network(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	addr = addr.Unmap()
	bits := 24
	if addr.Is6() {
		bits = 48
	}
	prefix, err := addr.Prefix(bits)
	if err != nil {
		return ""
	}
	return prefix.String()
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// ErrNotConfigured: the API started without access governance.
var ErrNotConfigured = errors.New("access governance is not configured")
