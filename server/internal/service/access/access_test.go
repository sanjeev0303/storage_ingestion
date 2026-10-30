package access

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// memStore is enough of the access store for the policy and detection
// logic.
type memStore struct {
	Store
	policy  model.AccessPolicy
	month   int64
	day     int64
	added   int64
	alerts  []model.SecurityAlert
	revoked int
	recent  []postgres.ReadCount
	burst   []postgres.ReadCount
	hourly  map[time.Time]int64
	first   time.Time
	ips     map[bool][]string // recent?
}

func (m *memStore) Policy(context.Context, string) (model.AccessPolicy, error) { return m.policy, nil }
func (m *memStore) Egress(context.Context, string, string, time.Time) (int64, int64, error) {
	return m.month, m.day, nil
}
func (m *memStore) AddEgress(_ context.Context, _, _ string, b int64, _ time.Time) error {
	m.added += b
	return nil
}
func (m *memStore) AddAlert(_ context.Context, a model.SecurityAlert) (bool, error) {
	for _, x := range m.alerts {
		if x.DedupKey == a.DedupKey {
			return false, nil
		}
	}
	m.alerts = append(m.alerts, a)
	return true, nil
}
func (m *memStore) RevokeLinks(context.Context, string, postgres.LinkFilter, string, time.Time) (int64, error) {
	m.revoked++
	return 4, nil
}
func (m *memStore) RecentReads(_ context.Context, from time.Time) ([]postgres.ReadCount, error) {
	if time.Since(from) < 10*time.Minute {
		return m.burst, nil
	}
	return m.recent, nil
}
func (m *memStore) HourlyReads(context.Context, string, string, time.Time, time.Time) (map[time.Time]int64, time.Time, error) {
	return m.hourly, m.first, nil
}
func (m *memStore) SourceIPs(_ context.Context, _, _ string, from, _ time.Time) ([]string, error) {
	return m.ips[time.Since(from) < 2*time.Hour], nil
}

func newTestService(m *memStore) *Service {
	return NewService(m, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestLinkRules(t *testing.T) {
	m := &memStore{policy: model.DefaultAccessPolicy("t")}
	s := newTestService(m)
	p := model.Principal{TenantID: "t", UserID: "u"}
	if ttl, err := s.LinkTTL(context.Background(), p, model.Asset{}, true); err != nil || ttl != 0 {
		t.Fatalf("default: %v %v", ttl, err)
	}
	if _, err := s.LinkTTL(context.Background(), p, model.Asset{Classification: model.ClassRestricted}, true); !errors.Is(err, model.ErrLinkClassified) {
		t.Fatalf("restricted share link: %v", err)
	}
	if ttl, err := s.LinkTTL(context.Background(), p, model.Asset{Classification: model.ClassRestricted}, false); err != nil || ttl != 15*time.Minute {
		t.Fatalf("restricted presigned: %v %v", ttl, err)
	}
	m.policy.MaxLinkTTL = 30 * time.Minute
	if ttl, _ := s.LinkTTL(context.Background(), p, model.Asset{Classification: model.ClassConfidential}, false); ttl != 30*time.Minute {
		t.Fatalf("the shorter cap wins: %v", ttl)
	}
	vendor := p
	vendor.Scope = model.Scope{TokenID: "tok", NoLinks: true}
	if _, err := s.LinkTTL(context.Background(), vendor, model.Asset{}, true); !errors.Is(err, model.ErrLinksDisabled) {
		t.Fatalf("a token without links: %v", err)
	}
	m.policy.LinksEnabled = false
	if _, err := s.LinkTTL(context.Background(), p, model.Asset{}, true); !errors.Is(err, model.ErrLinksDisabled) {
		t.Fatalf("links off: %v", err)
	}
}

func TestEgressBudgets(t *testing.T) {
	m := &memStore{policy: model.DefaultAccessPolicy("t"), month: 900}
	m.policy.EgressMonthlyBytes = 1000
	s := newTestService(m)
	p := model.Principal{TenantID: "t", UserID: "u"}
	if err := s.Egress(context.Background(), p, 50); err != nil || m.added != 50 {
		t.Fatalf("within budget: %v %d", err, m.added)
	}
	// WARN lets it through and alerts once a day.
	if err := s.Egress(context.Background(), p, 500); err != nil || len(m.alerts) != 1 || m.alerts[0].ActionTaken != "warned" {
		t.Fatalf("warn: %v %+v", err, m.alerts)
	}
	_ = s.Egress(context.Background(), p, 500)
	if len(m.alerts) != 1 {
		t.Fatal("the budget alert repeated within the day")
	}
	m.policy.EgressAction, m.alerts = "BLOCK", nil
	before := m.added
	if err := s.Egress(context.Background(), p, 500); !errors.Is(err, model.ErrEgressBudget) || m.added != before {
		t.Fatalf("block: %v", err)
	}
}

func TestDetectReadAnomalies(t *testing.T) {
	now := time.Now().UTC()
	hourly := map[time.Time]int64{}
	for h := 2; h < 14*24; h += 3 {
		hourly[now.Add(-time.Duration(h)*time.Hour).Truncate(time.Hour)] = 4
	}
	m := &memStore{
		policy: model.DefaultAccessPolicy("t"),
		recent: []postgres.ReadCount{{TenantID: "t", PrincipalID: "u", Reads: 900, Links: 900}},
		burst:  []postgres.ReadCount{{TenantID: "t", PrincipalID: "u", Reads: 400, Links: 400}},
		hourly: hourly, first: now.Add(-60 * 24 * time.Hour),
		ips: map[bool][]string{true: {"198.51.100.7", "203.0.113.9"}, false: {"203.0.113.40"}},
	}
	m.policy.AnomalyAction = "REVOKE_LINKS"
	s := newTestService(m)
	stats, err := s.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]model.SecurityAlert{}
	for _, a := range m.alerts {
		kinds[a.Kind] = a
	}
	if stats.Alerts != 3 || kinds[model.AlertMassLinks].Kind == "" || kinds[model.AlertReadSpike].Kind == "" {
		t.Fatalf("alerts: %+v", m.alerts)
	}
	if n := kinds[model.AlertNewNetwork]; !strings.Contains(n.Detail, "198.51.100.0/24") || strings.Contains(n.Detail, "203.0.113.0/24") {
		t.Fatalf("new network: %+v", n)
	}
	if m.revoked != 3 || !strings.Contains(kinds[model.AlertReadSpike].ActionTaken, "revoked 4") {
		t.Fatalf("revoke links: %d %+v", m.revoked, kinds[model.AlertReadSpike])
	}
	// The same hour does not alert twice.
	again, _ := s.Detect(context.Background())
	if again.Alerts != 0 {
		t.Fatalf("repeated: %+v", again)
	}

	// A new member with no history, and an ordinary hour, are not anomalies.
	quiet := &memStore{policy: model.DefaultAccessPolicy("t"), recent: []postgres.ReadCount{{TenantID: "t", PrincipalID: "v", Reads: 60}},
		hourly: map[time.Time]int64{}, first: now.Add(-2 * 24 * time.Hour), ips: map[bool][]string{true: {"192.0.2.1"}}}
	if stats, _ := newTestService(quiet).Detect(context.Background()); stats.Alerts != 0 {
		t.Fatalf("a member without history alerted: %+v", quiet.alerts)
	}
}

func TestTokenAndModelRules(t *testing.T) {
	now := time.Now()
	tok := model.APIToken{ExpiresAt: now.Add(time.Hour), Networks: []string{"10.1.0.0/16"}}
	if !tok.Usable(now, "10.1.2.3") || tok.Usable(now, "10.2.0.1") || tok.Usable(now, "") {
		t.Fatal("network binding")
	}
	tok.Networks = nil
	if tok.Usable(now.Add(2*time.Hour), "1.2.3.4") {
		t.Fatal("an expired token was usable")
	}
	scope := model.Scope{Restricted: true, Datasets: []string{"scans"}}
	if !scope.Allows("scans") || scope.Allows("") || scope.Allows("other") || !(model.Scope{}).Allows("anything") {
		t.Fatal("dataset scope")
	}
	if model.ClassForFindings([]string{"format.invalid_bam", "secret.aws_access_key"}) != model.ClassRestricted ||
		model.ClassForFindings([]string{"external.pii.ssn"}) != model.ClassConfidential || model.ClassForFindings(nil) != "" {
		t.Fatal("classification from findings")
	}
}
