package model

import (
	"errors"
	"time"
)

// Data quality: column profiles, drift, leakage and releases
// (docs/quality.md).

var (
	ErrProfileNotReady = errors.New("the version's column profile is not ready")
	ErrReleaseTaken    = errors.New("that release label is already used in this dataset")
	ErrNoRelease       = errors.New("no release with that label")
)

// QualitySettings are a dataset's quality settings.
type QualitySettings struct {
	DatasetID        string    `json:"dataset_id"`
	TenantID         string    `json:"-"`
	ProfileSnapshots bool      `json:"profile_snapshots"`
	MaxPSI           float64   `json:"max_psi"`
	MaxNullChange    float64   `json:"max_null_change"`
	MaxRowChange     float64   `json:"max_row_change"`
	SubjectKey       string    `json:"subject_key"`
	SubjectPattern   string    `json:"subject_pattern"`
	UpdatedBy        string    `json:"updated_by,omitempty"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func DefaultQualitySettings(datasetID string) QualitySettings {
	return QualitySettings{DatasetID: datasetID, ProfileSnapshots: true, MaxPSI: 0.2, MaxNullChange: 0.1, MaxRowChange: 0.5}
}

// Column kinds.
const (
	KindNumber = "number"
	KindString = "string"
	KindBool   = "bool"
	KindTime   = "time"
	KindNested = "nested"
	KindMixed  = "mixed"
	KindEmpty  = "empty"
)

// ValueCount is a value and how often it appears.
type ValueCount struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

// ColumnProfile describes one column of a table.
type ColumnProfile struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Count int64  `json:"count"` // values present
	Nulls int64  `json:"nulls"`
	// Distinct is exact up to DistinctCap; past it DistinctCapped is set.
	Distinct       int64 `json:"distinct"`
	DistinctCapped bool  `json:"distinct_capped,omitempty"`
	// Numbers and times (seconds since 1970).
	Min    *float64 `json:"min,omitempty"`
	Max    *float64 `json:"max,omitempty"`
	Mean   *float64 `json:"mean,omitempty"`
	StdDev *float64 `json:"stddev,omitempty"`
	// Sample is a reservoir of values, for drift.
	Sample []float64 `json:"sample,omitempty"`
	// Strings and booleans: the most common values.
	Top       []ValueCount `json:"top,omitempty"`
	MinLength *int64       `json:"min_length,omitempty"`
	MaxLength *int64       `json:"max_length,omitempty"`
}

// TableProfile is the profile of the files that make one table: a
// directory of parts, or one file at the root.
type TableProfile struct {
	Table   string          `json:"table"`
	Format  string          `json:"format"`
	Files   int64           `json:"files"`
	Rows    int64           `json:"rows"`
	Partial bool            `json:"partial,omitempty"` // stopped at the row limit
	Columns []ColumnProfile `json:"columns"`
}

// SkippedFile is a member the profiler did not read, and why.
type SkippedFile struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// VersionProfile is a version's column profile.
type VersionProfile struct {
	VersionID     string         `json:"version_id"`
	TenantID      string         `json:"-"`
	DatasetID     string         `json:"dataset_id"`
	State         string         `json:"state"`
	Tables        []TableProfile `json:"tables"`
	Skipped       []SkippedFile  `json:"skipped"`
	FilesProfiled int            `json:"files_profiled"`
	BytesRead     int64          `json:"bytes_read"`
	// OtherFiles counts members that are not tables (images, text).
	OtherFiles        int64        `json:"other_files"`
	BaselineVersionID string       `json:"baseline_version_id,omitempty"`
	Drift             *DriftReport `json:"drift,omitempty"`
	Drifted           bool         `json:"drifted"`
	Error             string       `json:"error,omitempty"`
	Attempts          int          `json:"-"`
	CreatedAt         time.Time    `json:"created_at"`
	FinishedAt        *time.Time   `json:"finished_at,omitempty"`
}

// Profile states.
const (
	ProfilePending = "PENDING"
	ProfileRunning = "RUNNING"
	ProfileDone    = "DONE"
	ProfileFailed  = "FAILED"
)

// DriftReport compares a version's profile with a baseline's.
type DriftReport struct {
	BaselineVersionID string       `json:"baseline_version_id"`
	VersionID         string       `json:"version_id"`
	Thresholds        DriftLimits  `json:"thresholds"`
	Tables            []TableDrift `json:"tables"`
	// Breaches counts columns and tables past a threshold.
	Breaches int `json:"breaches"`
	// NotCompared lists tables only one side has.
	NotCompared []string `json:"not_compared"`
}

type DriftLimits struct {
	MaxPSI        float64 `json:"max_psi"`
	MaxNullChange float64 `json:"max_null_change"`
	MaxRowChange  float64 `json:"max_row_change"`
}

type TableDrift struct {
	Table          string        `json:"table"`
	RowsBefore     int64         `json:"rows_before"`
	RowsAfter      int64         `json:"rows_after"`
	RowChange      float64       `json:"row_change"` // relative, signed
	ColumnsAdded   []string      `json:"columns_added"`
	ColumnsRemoved []string      `json:"columns_removed"`
	Columns        []ColumnDrift `json:"columns"`
	Breached       []string      `json:"breached"`
}

type ColumnDrift struct {
	Name           string   `json:"name"`
	KindBefore     string   `json:"kind_before"`
	KindAfter      string   `json:"kind_after"`
	PSI            *float64 `json:"psi,omitempty"`
	NullRateBefore float64  `json:"null_rate_before"`
	NullRateAfter  float64  `json:"null_rate_after"`
	MeanBefore     *float64 `json:"mean_before,omitempty"`
	MeanAfter      *float64 `json:"mean_after,omitempty"`
	// NotCompared says why the distribution was not compared.
	NotCompared string   `json:"not_compared,omitempty"`
	Breached    []string `json:"breached"`
}

// Run input roles.
const (
	RoleTrain      = "TRAIN"
	RoleValidation = "VALIDATION"
	RoleTest       = "TEST"
)

// RunInput is a version a run read, with what it read it for.
type RunInput struct {
	VersionID string
	DatasetID string
	Role      string
}

// QualityMember is what the subject and leakage checks read of a member.
type QualityMember struct {
	Path     string
	Split    string
	Checksum string
	Subject  string // the asked-for metadata key's value
}

// Leakage methods and states.
const (
	LeakExactHash = "EXACT_HASH"
	LeakSubject   = "SUBJECT"
	LeakNgram     = "NGRAM"

	LeakClean      = "CLEAN"
	LeakLeaked     = "LEAKED"
	LeakNotChecked = "NOT_CHECKED"
)

// LeakageExample is one evaluation member found in the training set.
type LeakageExample struct {
	EvalPath  string `json:"eval_path"`
	TrainPath string `json:"train_path,omitempty"`
	Key       string `json:"key"` // the shared SHA-256, subject or n-gram share
}

// LeakageCheck is one comparison of an evaluation version with a training
// version.
type LeakageCheck struct {
	ID             string           `json:"id"`
	TenantID       string           `json:"-"`
	RunID          string           `json:"run_id,omitempty"`
	TrainVersionID string           `json:"train_version_id"`
	EvalVersionID  string           `json:"eval_version_id"`
	Method         string           `json:"method"`
	State          string           `json:"state"`
	Overlap        int64            `json:"overlap"`
	EvalMembers    int64            `json:"eval_members"`
	Examples       []LeakageExample `json:"examples"`
	Detail         string           `json:"detail"`
	Parameters     map[string]any   `json:"parameters"`
	CreatedBy      string           `json:"created_by"`
	CreatedAt      time.Time        `json:"created_at"`
}

// Release is a labelled version.
type Release struct {
	Label      string    `json:"label"`
	VersionID  string    `json:"version_id"`
	Version    int       `json:"version"`
	Notes      string    `json:"notes"`
	Members    int64     `json:"members"`
	Bytes      int64     `json:"bytes"`
	ReleasedBy string    `json:"released_by"`
	ReleasedAt time.Time `json:"released_at"`
}
