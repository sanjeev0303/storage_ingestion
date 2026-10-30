package tiering

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

const gb = 1 << 30

// EstimateInput describes work to price: an ingest, a download, a
// training epoch, keeping data for years, or a restore from archive.
type EstimateInput struct {
	ConnectionID string `json:"connection_id"`
	Operation    string `json:"operation"`
	Files        int64  `json:"files"`
	Bytes        int64  `json:"bytes"`
	// Requests overrides Files as the reads an epoch makes (a packed
	// snapshot reads a pack per request).
	Requests int64              `json:"requests"`
	Tier     model.StorageTier  `json:"tier"`
	Years    float64            `json:"years"`
	Epochs   int                `json:"epochs"`
	Speed    model.RestoreSpeed `json:"speed"`
}

// Estimate is what work would cost at the provider's list prices.
type Estimate struct {
	Operation    string                    `json:"operation"`
	Provider     model.StorageProviderType `json:"provider"`
	Requests     int64                     `json:"requests"`
	Bytes        int64                     `json:"bytes"`
	StorageUSD   float64                   `json:"storage_usd"`
	RequestUSD   float64                   `json:"request_usd"`
	EgressUSD    float64                   `json:"egress_usd"`
	RetrievalUSD float64                   `json:"retrieval_usd"`
	TotalUSD     float64                   `json:"total_usd"`
	Hours        float64                   `json:"hours"`
	Note         string                    `json:"note"`
}

func cents(v float64) float64 { return math.Round(v*100) / 100 }

// estimate prices work against a price book. It is plain arithmetic:
// estimates, not bills.
func estimate(in EstimateInput, book model.PriceBook) Estimate {
	tier := in.Tier
	if tier == "" {
		tier = model.TierStandard
	}
	gbs := float64(in.Bytes) / gb
	out := Estimate{Operation: in.Operation, Provider: book.Provider, Bytes: in.Bytes, Note: book.Note}
	switch in.Operation {
	case "ingest":
		// One PUT per file, and one per 64 MiB part of a large one.
		out.Requests = in.Files + in.Bytes/(64<<20)
		out.RequestUSD = float64(out.Requests) / 1000 * book.WritePer1000
		out.StorageUSD = gbs * book.StorageGBMonth[tier]
		out.Note += "; storage is for the first month"
	case "download":
		out.Requests = in.Files
		out.RequestUSD = float64(out.Requests) / 1000 * book.ReadPer1000
		out.EgressUSD = gbs * book.EgressGB
		out.RetrievalUSD = gbs * book.RetrievalGB[tier]
	case "epoch":
		epochs := max(in.Epochs, 1)
		reads := in.Requests
		if reads == 0 {
			reads = in.Files
		}
		out.Requests = reads * int64(epochs)
		out.RequestUSD = float64(out.Requests) / 1000 * book.ReadPer1000
		out.RetrievalUSD = gbs * book.RetrievalGB[tier] * float64(epochs)
		out.Note += "; read in the provider's region, so no egress"
	case "retain":
		years := in.Years
		if years <= 0 {
			years = 1
		}
		months := years * 12
		if minDays := book.MinDays[tier]; float64(minDays)/30 > months {
			months = float64(minDays) / 30
		}
		out.StorageUSD = gbs * book.StorageGBMonth[tier] * months
	case "restore":
		out.Requests = in.Files
		out.RequestUSD = float64(out.Requests) / 1000 * book.WritePer1000
		out.RetrievalUSD = gbs * book.RestoreGB[in.Speed]
		out.Hours = book.RestoreHours[in.Speed]
	}
	out.StorageUSD, out.RequestUSD, out.EgressUSD, out.RetrievalUSD =
		cents(out.StorageUSD), cents(out.RequestUSD), cents(out.EgressUSD), cents(out.RetrievalUSD)
	out.TotalUSD = cents(out.StorageUSD + out.RequestUSD + out.EgressUSD + out.RetrievalUSD)
	return out
}

var operations = map[string]bool{"ingest": true, "download": true, "epoch": true, "retain": true, "restore": true}

// Estimate prices work on one of the organization's connections.
func (s *Service) Estimate(ctx context.Context, p model.Principal, in EstimateInput) (Estimate, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermStorageRead, "costs.estimate", "organization", p.TenantID); err != nil {
		return Estimate{}, err
	}
	switch {
	case !operations[in.Operation]:
		return Estimate{}, invalid("operation is ingest, download, epoch, retain or restore")
	case in.Files < 0 || in.Bytes < 0 || in.Requests < 0 || in.Epochs < 0 || in.Years < 0 || in.Years > 100:
		return Estimate{}, invalid("counts are not negative, and years at most 100")
	case in.Tier != "" && !in.Tier.Valid():
		return Estimate{}, invalid("tier is STANDARD, INFREQUENT, ARCHIVE or DEEP_ARCHIVE")
	}
	if in.Operation == "restore" && in.Speed == "" {
		in.Speed = model.RestoreStandard
	}
	provider, err := s.deps.Store.ProviderOf(ctx, p.TenantID, in.ConnectionID)
	if err != nil {
		return Estimate{}, model.ErrConnectionNotFound
	}
	return estimate(in, model.PricesFor(provider)), nil
}

// Prices are the list prices estimates use, per provider.
func Prices() []model.PriceBook {
	var out []model.PriceBook
	for _, p := range []model.StorageProviderType{model.ProviderAWSS3, model.ProviderGCS, model.ProviderAzureBlob, model.ProviderR2, model.ProviderPlatform} {
		out = append(out, model.PricesFor(p))
	}
	return out
}

// StoredLine is what one connection keeps in one tier, and its monthly cost.
type StoredLine struct {
	ConnectionID string                    `json:"connection_id"`
	Provider     model.StorageProviderType `json:"provider"`
	Tier         model.StorageTier         `json:"tier"`
	Objects      int64                     `json:"objects"`
	Bytes        int64                     `json:"bytes"`
	MonthlyUSD   float64                   `json:"monthly_usd"`
}

func priced(stored []postgres.StoredBytes) ([]StoredLine, float64, model.StorageProviderType) {
	lines := []StoredLine{}
	var total float64
	var main model.StorageProviderType = model.ProviderPlatform
	var mainBytes int64
	byProvider := map[model.StorageProviderType]int64{}
	for _, b := range stored {
		book := model.PricesFor(b.Provider)
		usd := float64(b.Bytes) / gb * book.StorageGBMonth[b.Tier]
		lines = append(lines, StoredLine{ConnectionID: b.ConnectionID, Provider: b.Provider, Tier: b.Tier, Objects: b.Objects,
			Bytes: b.Bytes, MonthlyUSD: cents(usd)})
		total += usd
		byProvider[b.Provider] += b.Bytes
		if byProvider[b.Provider] > mainBytes {
			main, mainBytes = b.Provider, byProvider[b.Provider]
		}
	}
	return lines, total, main
}

// DatasetCosts is a dataset's storage now, and what keeping it would cost.
type DatasetCosts struct {
	Stored     []StoredLine `json:"stored"`
	MonthlyUSD float64      `json:"monthly_usd"`
	Years      float64      `json:"years"`
	// RetainUSD keeps it as it is for Years; ArchivedUSD if it all moved
	// to the archive tier (where the provider has one).
	RetainUSD   float64 `json:"retain_usd"`
	ArchivedUSD float64 `json:"archived_usd"`
	Note        string  `json:"note"`
}

func (s *Service) DatasetCosts(ctx context.Context, p model.Principal, datasetID string, years float64) (DatasetCosts, error) {
	if _, err := s.deps.Datasets.Get(ctx, p, datasetID); err != nil {
		return DatasetCosts{}, err
	}
	if years <= 0 {
		years = 10
	}
	if years > 100 {
		return DatasetCosts{}, invalid("years is at most 100")
	}
	stored, err := s.deps.Store.Stored(ctx, p.TenantID, datasetID)
	if err != nil {
		return DatasetCosts{}, err
	}
	lines, monthly, provider := priced(stored)
	out := DatasetCosts{Stored: lines, MonthlyUSD: cents(monthly), Years: years, Note: model.PricesFor(provider).Note}
	for _, b := range stored {
		book := model.PricesFor(b.Provider)
		out.RetainUSD += estimate(EstimateInput{Operation: "retain", Bytes: b.Bytes, Tier: b.Tier, Years: years}, book).StorageUSD
		cold := model.TierArchive
		if _, ok := book.StorageGBMonth[cold]; !ok {
			cold = model.TierInfrequent
		}
		out.ArchivedUSD += estimate(EstimateInput{Operation: "retain", Bytes: b.Bytes, Tier: cold, Years: years}, book).StorageUSD
	}
	out.RetainUSD, out.ArchivedUSD = cents(out.RetainUSD), cents(out.ArchivedUSD)
	return out, nil
}

// Report is the organization's month: storage, activity, the budget, and
// what packing, deduplication and tiering saved.
type Report struct {
	Month        string            `json:"month"`
	Stored       []StoredLine      `json:"stored"`
	StorageUSD   float64           `json:"storage_usd"`
	Reads        int64             `json:"reads"`
	EgressBytes  int64             `json:"egress_bytes"`
	Writes       int64             `json:"writes"`
	RestoreBytes int64             `json:"restored_bytes"`
	RequestUSD   float64           `json:"request_usd"`
	EgressUSD    float64           `json:"egress_usd"`
	RestoreUSD   float64           `json:"restore_usd"`
	TotalUSD     float64           `json:"total_usd"`
	Budget       *model.CostBudget `json:"budget,omitempty"`
	BudgetUsed   float64           `json:"budget_used,omitempty"`
	Savings      Savings           `json:"savings"`
	Note         string            `json:"note"`
}

// Savings are what packing, deduplication and tiering save.
type Savings struct {
	PackedFiles int64 `json:"packed_files"`
	Packs       int64 `json:"packs"`
	// ReadsSavedPerPass: reading every packed file once costs this many
	// fewer requests than reading them one by one.
	ReadsSavedPerPass int64   `json:"reads_saved_per_pass"`
	SharedCopies      int64   `json:"shared_copies"`
	SharedBytes       int64   `json:"shared_bytes"`
	DedupUSDMonth     float64 `json:"dedup_usd_month"`
	TieringUSDMonth   float64 `json:"tiering_usd_month"`
}

type spendCache struct {
	at                      time.Time
	storageUSD              float64
	reads, egress, restored int64
	provider                model.StorageProviderType
}

// monthStart is the first instant of now's month.
func monthStart(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func (s *Service) Report(ctx context.Context, p model.Principal) (Report, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermOrgRead, "costs.read", "organization", p.TenantID); err != nil {
		return Report{}, err
	}
	now := s.now()
	stored, err := s.deps.Store.Stored(ctx, p.TenantID, "")
	if err != nil {
		return Report{}, err
	}
	usage, err := s.deps.Store.Usage(ctx, p.TenantID, monthStart(now))
	if err != nil {
		return Report{}, err
	}
	saved, err := s.deps.Store.Savings(ctx, p.TenantID)
	if err != nil {
		return Report{}, err
	}
	lines, storageUSD, provider := priced(stored)
	book := model.PricesFor(provider)
	r := Report{Month: now.Format("2006-01"), Stored: lines, StorageUSD: cents(storageUSD), Reads: usage.Reads,
		EgressBytes: usage.EgressBytes, Writes: usage.Writes, RestoreBytes: usage.RestoredBytes,
		RequestUSD: cents(float64(usage.Reads)/1000*book.ReadPer1000 + float64(usage.Writes)/1000*book.WritePer1000),
		EgressUSD:  cents(float64(usage.EgressBytes) / gb * book.EgressGB),
		RestoreUSD: cents(float64(usage.RestoredBytes) / gb * book.RestoreGB[model.RestoreStandard]),
		Note:       book.Note + "; requests and egress are priced at the provider holding the most bytes"}
	r.TotalUSD = cents(r.StorageUSD + r.RequestUSD + r.EgressUSD + r.RestoreUSD)
	r.Savings = Savings{PackedFiles: saved.PackedFiles, Packs: saved.Packs, ReadsSavedPerPass: max(saved.PackedFiles-saved.Packs, 0),
		SharedCopies: saved.SharedCopies, SharedBytes: saved.SharedBytes,
		DedupUSDMonth: cents(float64(saved.SharedBytes) / gb * book.StorageGBMonth[model.TierStandard])}
	for _, l := range lines {
		lb := model.PricesFor(l.Provider)
		r.Savings.TieringUSDMonth += float64(l.Bytes) / gb * (lb.StorageGBMonth[model.TierStandard] - lb.StorageGBMonth[l.Tier])
	}
	r.Savings.TieringUSDMonth = cents(r.Savings.TieringUSDMonth)
	if b, ok, err := s.deps.Store.Budget(ctx, p.TenantID); err == nil && ok {
		r.Budget = &b
		if b.MonthlyUSD > 0 {
			r.BudgetUsed = math.Round(r.TotalUSD/b.MonthlyUSD*1000) / 1000
		}
	}
	return r, nil
}

func (s *Service) Budget(ctx context.Context, p model.Principal) (model.CostBudget, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermOrgRead, "costs.read", "organization", p.TenantID); err != nil {
		return model.CostBudget{}, err
	}
	b, _, err := s.deps.Store.Budget(ctx, p.TenantID)
	return b, err
}

func (s *Service) SetBudget(ctx context.Context, p model.Principal, in model.CostBudget) (model.CostBudget, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermOrgManage, "costs.budget", "organization", p.TenantID); err != nil {
		return model.CostBudget{}, err
	}
	if in.Action == "" {
		in.Action = "ALERT"
	}
	if in.MonthlyUSD < 0 || in.MonthlyUSD > 1e9 || in.MonthlyRequests < 0 || (in.Action != "ALERT" && in.Action != "BLOCK") {
		return model.CostBudget{}, invalid("monthly_usd and monthly_requests are not negative (0 is no limit), and action is ALERT or BLOCK")
	}
	in.TenantID, in.UpdatedBy, in.UpdatedAt = p.TenantID, p.UserID, s.now()
	in.MonthlyUSD = cents(in.MonthlyUSD)
	if err := s.deps.Store.SetBudget(ctx, in); err != nil {
		return model.CostBudget{}, err
	}
	s.mu.Lock()
	delete(s.spend, p.TenantID)
	s.mu.Unlock()
	s.record(ctx, p, "costs.budget", "organization", p.TenantID,
		map[string]any{"monthly_usd": in.MonthlyUSD, "monthly_requests": in.MonthlyRequests, "action": in.Action})
	return in, nil
}

// Spend checks a read of bytes against the organization's cost budget
// before it is served. Past the budget it raises a COST_BUDGET alert (once
// a day) and, with BLOCK, refuses (model.ErrCostBudget). The month's
// figures are read at most once a minute and counted on in between.
func (s *Service) Spend(ctx context.Context, p model.Principal, bytes int64) error {
	budget, ok, err := s.deps.Store.Budget(ctx, p.TenantID)
	if err != nil || !ok || (budget.MonthlyUSD == 0 && budget.MonthlyRequests == 0) {
		return err
	}
	now := s.now()
	s.mu.Lock()
	c, cached := s.spend[p.TenantID]
	s.mu.Unlock()
	if !cached || now.Sub(c.at) > time.Minute || c.at.Month() != now.Month() {
		stored, err := s.deps.Store.Stored(ctx, p.TenantID, "")
		if err != nil {
			return err
		}
		usage, err := s.deps.Store.Usage(ctx, p.TenantID, monthStart(now))
		if err != nil {
			return err
		}
		_, storageUSD, provider := priced(stored)
		c = spendCache{at: now, storageUSD: storageUSD, reads: usage.Reads, egress: usage.EgressBytes, restored: usage.RestoredBytes, provider: provider}
	}
	c.reads++
	c.egress += bytes
	s.mu.Lock()
	s.spend[p.TenantID] = c
	s.mu.Unlock()

	book := model.PricesFor(c.provider)
	spent := c.storageUSD + float64(c.reads)/1000*book.ReadPer1000 + float64(c.egress)/gb*book.EgressGB +
		float64(c.restored)/gb*book.RestoreGB[model.RestoreStandard]
	over := ""
	switch {
	case budget.MonthlyUSD > 0 && spent > budget.MonthlyUSD:
		over = fmt.Sprintf("an estimated $%.2f this month, past the budget of $%.2f", spent, budget.MonthlyUSD)
	case budget.MonthlyRequests > 0 && c.reads > budget.MonthlyRequests:
		over = fmt.Sprintf("%d reads this month, past the budget of %d", c.reads, budget.MonthlyRequests)
	}
	if over == "" {
		return nil
	}
	action := "warned"
	if budget.Action == "BLOCK" {
		action = "refused"
	}
	if s.deps.Alerts != nil {
		_, _ = s.deps.Alerts.AddAlert(ctx, model.SecurityAlert{ID: newID(), TenantID: p.TenantID, PrincipalID: p.UserID,
			Kind: model.AlertCostBudget, Detail: "reads reached " + over, ActionTaken: action,
			Metrics:  map[string]any{"estimated_usd": cents(spent), "reads": c.reads, "egress_bytes": c.egress},
			DedupKey: "cost-budget:" + now.Format("2006-01-02"), CreatedAt: now})
	}
	if budget.Action == "BLOCK" {
		return model.ErrCostBudget
	}
	return nil
}
