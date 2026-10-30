package model

import (
	"errors"
	"time"
)

// StorageTier is where an asset's bytes sit in its provider's storage
// classes (docs/tiering.md). Each provider maps them to its own names.
type StorageTier string

const (
	TierStandard    StorageTier = "STANDARD"
	TierInfrequent  StorageTier = "INFREQUENT"
	TierArchive     StorageTier = "ARCHIVE"
	TierDeepArchive StorageTier = "DEEP_ARCHIVE"
)

// tierOrder ranks tiers from hot to cold.
var tierOrder = map[StorageTier]int{TierStandard: 0, TierInfrequent: 1, TierArchive: 2, TierDeepArchive: 3}

func (t StorageTier) Valid() bool { _, ok := tierOrder[t]; return ok }

// Colder reports whether t is a colder tier than other.
func (t StorageTier) Colder(other StorageTier) bool { return tierOrder[t] > tierOrder[other] }

// RestoreSpeed is how fast an archived object is made readable, at a
// price: FAST in minutes, STANDARD in hours, BULK within a day or two.
type RestoreSpeed string

const (
	RestoreFast     RestoreSpeed = "FAST"
	RestoreStandard RestoreSpeed = "STANDARD"
	RestoreBulk     RestoreSpeed = "BULK"
)

func (s RestoreSpeed) Valid() bool {
	return s == RestoreFast || s == RestoreStandard || s == RestoreBulk
}

// TierRule moves assets to a colder tier once they are old enough and have
// not been read for a while. Assets a legal hold, an Object Lock, a pack,
// shared bytes or an in-use snapshot pin are skipped.
type TierRule struct {
	ID         string      `json:"id"`
	TenantID   string      `json:"-"`
	DatasetID  string      `json:"dataset_id,omitempty"`
	Name       string      `json:"name"`
	Tier       StorageTier `json:"tier"`
	MinAgeDays int         `json:"min_age_days"`
	UnreadDays int         `json:"unread_days"`
	Enabled    bool        `json:"enabled"`
	CreatedBy  string      `json:"created_by"`
	CreatedAt  time.Time   `json:"created_at"`
	UpdatedAt  time.Time   `json:"updated_at"`
}

// TierMove is one move between tiers, or one that failed.
type TierMove struct {
	ID        string      `json:"id"`
	TenantID  string      `json:"-"`
	AssetID   string      `json:"asset_id"`
	RuleID    string      `json:"rule_id,omitempty"`
	From      StorageTier `json:"from_tier"`
	To        StorageTier `json:"to_tier"`
	SizeBytes int64       `json:"size_bytes"`
	State     string      `json:"state"`
	Reason    string      `json:"reason,omitempty"`
	CreatedAt time.Time   `json:"created_at"`
}

// TierPack is a pack a rule may move whole: its object, tier and files.
type TierPack struct {
	ID, TenantID, ConnectionID, Bucket, ObjectKey, ETag, VersionID string
	Bytes                                                          int64
	Files                                                          int64
	Tier                                                           StorageTier
	Provider                                                       StorageProviderType
}

// TierCandidate is an asset a rule may move, with where it is stored.
type TierCandidate struct {
	Asset    Asset
	Provider StorageProviderType
}

// ArchiveRestore is a temporary readable copy of an archived asset.
type ArchiveRestore struct {
	ID            string       `json:"id"`
	TenantID      string       `json:"-"`
	AssetID       string       `json:"asset_id"`
	VersionID     string       `json:"version_id,omitempty"`
	State         string       `json:"state"` // PENDING, RESTORING, READY, FAILED, EXPIRED
	Days          int          `json:"days"`
	Speed         RestoreSpeed `json:"speed"`
	Error         string       `json:"error,omitempty"`
	RequestedBy   string       `json:"requested_by"`
	CreatedAt     time.Time    `json:"created_at"`
	UpdatedAt     time.Time    `json:"updated_at"`
	ReadyAt       *time.Time   `json:"ready_at,omitempty"`
	ReadableUntil *time.Time   `json:"readable_until,omitempty"`
}

// CostBudget caps an organization's estimated monthly spend and reads.
// ALERT raises a COST_BUDGET alert; BLOCK also refuses downloads past it.
type CostBudget struct {
	TenantID        string    `json:"-"`
	MonthlyUSD      float64   `json:"monthly_usd"`
	MonthlyRequests int64     `json:"monthly_requests"`
	Action          string    `json:"action"`
	UpdatedBy       string    `json:"updated_by,omitempty"`
	UpdatedAt       time.Time `json:"updated_at"`
}

const AlertCostBudget = "COST_BUDGET"

var (
	// ErrRestoreRequired: the asset is archived and must be restored
	// before it can be read.
	ErrRestoreRequired  = errors.New("the object is archived: restore it before reading")
	ErrCostBudget       = errors.New("the organization's monthly cost budget is used up")
	ErrTierUnsupported  = errors.New("the storage connection cannot move objects between tiers")
	ErrInvalidTiering   = errors.New("invalid tiering request")
	ErrRestoreNotFound  = errors.New("restore not found")
	ErrTierRuleNotFound = errors.New("tier rule not found")
)

// PriceBook is a provider's list prices, in US dollars, used for
// estimates only: real bills depend on region, contract and discounts.
type PriceBook struct {
	Provider StorageProviderType `json:"provider"`
	// Per GB-month, by tier; a tier missing is one the provider lacks.
	StorageGBMonth map[StorageTier]float64 `json:"storage_gb_month"`
	// Per 1,000 requests: reads (GET) and writes (PUT, COPY, LIST).
	ReadPer1000  float64 `json:"read_per_1000"`
	WritePer1000 float64 `json:"write_per_1000"`
	// Internet egress per GB.
	EgressGB float64 `json:"egress_gb"`
	// Retrieval per GB read from a cold tier, and per restore speed.
	RetrievalGB map[StorageTier]float64  `json:"retrieval_gb"`
	RestoreGB   map[RestoreSpeed]float64 `json:"restore_gb,omitempty"`
	// Minimum days a tier bills for, even if the object leaves sooner.
	MinDays map[StorageTier]int `json:"min_days"`
	// Hours a restore takes, by speed (archive tiers only).
	RestoreHours map[RestoreSpeed]float64 `json:"restore_hours,omitempty"`
	Note         string                   `json:"note"`
}

var awsPrices = PriceBook{
	StorageGBMonth: map[StorageTier]float64{TierStandard: 0.023, TierInfrequent: 0.0125, TierArchive: 0.0036, TierDeepArchive: 0.00099},
	ReadPer1000:    0.0004, WritePer1000: 0.005, EgressGB: 0.09,
	RetrievalGB:  map[StorageTier]float64{TierInfrequent: 0.01, TierArchive: 0.01, TierDeepArchive: 0.02},
	RestoreGB:    map[RestoreSpeed]float64{RestoreFast: 0.03, RestoreStandard: 0.01, RestoreBulk: 0.0025},
	MinDays:      map[StorageTier]int{TierInfrequent: 30, TierArchive: 90, TierDeepArchive: 180},
	RestoreHours: map[RestoreSpeed]float64{RestoreFast: 0.1, RestoreStandard: 5, RestoreBulk: 12},
	Note:         "Amazon S3 list prices (US East); your region and agreements change them",
}

// PriceBooks holds each provider's list prices. A self-hosted or
// S3-compatible store is estimated at Amazon S3's.
var PriceBooks = map[StorageProviderType]PriceBook{
	ProviderAWSS3: awsPrices,
	ProviderGCS: {
		StorageGBMonth: map[StorageTier]float64{TierStandard: 0.020, TierInfrequent: 0.010, TierArchive: 0.004, TierDeepArchive: 0.0012},
		ReadPer1000:    0.0004, WritePer1000: 0.005, EgressGB: 0.12,
		RetrievalGB: map[StorageTier]float64{TierInfrequent: 0.01, TierArchive: 0.02, TierDeepArchive: 0.05},
		MinDays:     map[StorageTier]int{TierInfrequent: 30, TierArchive: 90, TierDeepArchive: 365},
		Note:        "Google Cloud Storage list prices (Nearline, Coldline, Archive; US); archived objects read without a restore",
	},
	ProviderAzureBlob: {
		StorageGBMonth: map[StorageTier]float64{TierStandard: 0.018, TierInfrequent: 0.010, TierArchive: 0.0036, TierDeepArchive: 0.00099},
		ReadPer1000:    0.0004, WritePer1000: 0.0055, EgressGB: 0.087,
		RetrievalGB:  map[StorageTier]float64{TierInfrequent: 0.01, TierArchive: 0.03, TierDeepArchive: 0.02},
		RestoreGB:    map[RestoreSpeed]float64{RestoreFast: 0.10, RestoreStandard: 0.02, RestoreBulk: 0.02},
		MinDays:      map[StorageTier]int{TierInfrequent: 30, TierArchive: 90, TierDeepArchive: 180},
		RestoreHours: map[RestoreSpeed]float64{RestoreFast: 1, RestoreStandard: 15, RestoreBulk: 15},
		Note:         "Azure Blob list prices (Hot, Cool, Cold, Archive; East US); only Archive needs rehydrating",
	},
	ProviderR2: {
		StorageGBMonth: map[StorageTier]float64{TierStandard: 0.015, TierInfrequent: 0.010},
		ReadPer1000:    0.00036, WritePer1000: 0.0045, EgressGB: 0,
		RetrievalGB: map[StorageTier]float64{TierInfrequent: 0.01},
		MinDays:     map[StorageTier]int{TierInfrequent: 30},
		Note:        "Cloudflare R2 list prices; no egress fees; no archive tier",
	},
}

// PricesFor is the price book for a provider.
func PricesFor(p StorageProviderType) PriceBook {
	book, ok := PriceBooks[p]
	if !ok {
		book = awsPrices
		book.Note = "estimated at Amazon S3 list prices: a self-hosted or S3-compatible store bills as its operator does"
	}
	book.Provider = p
	return book
}
