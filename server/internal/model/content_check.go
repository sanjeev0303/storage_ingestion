package model

import (
	"errors"
	"strings"
	"time"
)

// Finding actions. A flagged asset is still published, with the finding on
// its record; a rejected one is not. "allow" in a tenant's overrides drops
// the finding altogether.
const (
	FindingAllow  = "allow"
	FindingFlag   = "flag"
	FindingReject = "reject"
)

// RejectContentCheck is the rejection reason when a content check's
// finding rejects the upload; the finding itself says which.
const RejectContentCheck = "content_check_failed"

// ContentFinding is one thing a content check found in an upload's bytes.
// Code is "<family>.<what>", such as "secret.aws_access_key".
type ContentFinding struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"-"`
	AssetID   string    `json:"asset_id"`
	JobID     string    `json:"job_id,omitempty"`
	Source    string    `json:"source"`
	Code      string    `json:"code"`
	Action    string    `json:"action"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
}

// StaleScan is a published asset whose CLEAN verdict came from older
// signatures, claimed for a re-scan.
type StaleScan struct {
	TenantID   string
	AssetID    string
	JobID      string
	OldVersion string
}

// BlastRadius is where one asset's bytes went: the assets holding the
// same bytes, the snapshots that pin them and who got download links.
type BlastRadius struct {
	ChecksumSHA256 string          `json:"checksum_sha256"`
	SizeBytes      int64           `json:"size_bytes"`
	Assets         []BlastAsset    `json:"assets"`
	Snapshots      []BlastSnapshot `json:"snapshots"`
	Downloads      []BlastDownload `json:"downloads"`
}

type BlastAsset struct {
	AssetID  string `json:"asset_id"`
	ObjectID string `json:"object_id"`
	Version  int    `json:"version"`
	FileName string `json:"file_name"`
	Status   string `json:"status"`
	// BlockedReason is set when a re-scan blocked the asset's reads.
	BlockedReason string    `json:"blocked_reason,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

type BlastSnapshot struct {
	DatasetID   string `json:"dataset_id"`
	DatasetName string `json:"dataset_name"`
	Version     int    `json:"version"`
	Members     int64  `json:"members"`
}

type BlastDownload struct {
	ActorType string    `json:"actor_type"`
	ActorID   string    `json:"actor_id"`
	Links     int64     `json:"links"`
	First     time.Time `json:"first"`
	Last      time.Time `json:"last"`
}

var ErrInvalidFindingActions = errors.New("invalid finding actions")

// defaultFindingActions are the actions per code family, and per code
// where one differs from its family.
var defaultFindingActions = map[string]string{
	"active_content":            FindingFlag,
	"active_content.pdf_launch": FindingReject,
	"model":                     FindingReject,
	"model.pickle_imports":      FindingFlag, // needs the model's own code; not dangerous by itself
	"model.npy_object_array":    FindingFlag, // its pickle's imports are judged on their own
	"secret":                    FindingFlag,
	"format":                    FindingReject,
	"external":                  FindingFlag,
	"malware":                   FindingReject,
}

// FindingFamilies are the code families a tenant may set an action for.
var FindingFamilies = []string{"active_content", "model", "secret", "format", "external", "malware"}

// FindingAction decides what a finding does: the tenant's override for
// the code, then for its family, then the default. Malware always rejects.
func FindingAction(code, suggested string, overrides map[string]string) string {
	family, _, _ := strings.Cut(code, ".")
	if family == "malware" {
		return FindingReject
	}
	if a, ok := overrides[code]; ok {
		return a
	}
	if a, ok := overrides[family]; ok {
		return a
	}
	// An external engine's own verdict stands unless the tenant overrides it.
	if suggested == FindingFlag || suggested == FindingReject {
		return suggested
	}
	if a, ok := defaultFindingActions[code]; ok {
		return a
	}
	if a, ok := defaultFindingActions[family]; ok {
		return a
	}
	return FindingFlag
}

// ValidateFindingActions checks a tenant's overrides: keys are a family or
// "<family>.<what>", values allow, flag or reject, and malware is never
// loosened.
func ValidateFindingActions(actions map[string]string) error {
	for code, action := range actions {
		family, rest, dotted := strings.Cut(code, ".")
		known := false
		for _, f := range FindingFamilies {
			if f == family {
				known = true
			}
		}
		if !known || (dotted && rest == "") || len(code) > 100 {
			return errors.Join(ErrInvalidFindingActions, errors.New("unknown finding code "+code))
		}
		if action != FindingAllow && action != FindingFlag && action != FindingReject {
			return errors.Join(ErrInvalidFindingActions, errors.New(code+": action must be allow, flag or reject"))
		}
		if family == "malware" && action != FindingReject {
			return errors.Join(ErrInvalidFindingActions, errors.New("malware findings always reject"))
		}
	}
	return nil
}
