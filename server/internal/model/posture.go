package model

import (
	"errors"
	"time"
)

// Posture check outcomes. NOT_CHECKED is an outcome too: the provider has
// no way to answer, or the credentials may not ask.
const (
	PosturePass       = "PASS"
	PostureWarn       = "WARN"
	PostureFail       = "FAIL"
	PostureNotChecked = "NOT_CHECKED"
)

// Posture checks (docs/storage-posture.md).
const (
	CheckPublicAccessBlock = "public_access_block"
	CheckPublicPolicy      = "public_policy"
	CheckPublicACL         = "public_acl"
	CheckOwnership         = "object_ownership"
	CheckEncryption        = "default_encryption"
	CheckSSECDenied        = "sse_c_denied"
	CheckVersioning        = "versioning"
	CheckObjectLock        = "object_lock"
	CheckLifecycle         = "lifecycle_rules"
	CheckLifecycleDrift    = "lifecycle_drift"
	CheckIncompleteUploads = "incomplete_upload_expiry"
	CheckCredentialScope   = "credential_scope"
	CheckBucketOwner       = "bucket_owner"
	CheckGuessableName     = "guessable_name"
	CheckSelfTest          = "self_test"
)

// PostureCheck is one check's outcome. Detail says what was seen and,
// for anything but PASS, what to change; it never holds a secret.
type PostureCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// PostureFacts is what a provider reports about a bucket: its checks and
// the values later runs compare against.
type PostureFacts struct {
	Checks []PostureCheck
	// LifecycleHash fingerprints the bucket's lifecycle rules ("" when
	// they could not be read); a change between runs is reported.
	LifecycleHash string
	// OwnerAccount is the account the credentials belong to, when the
	// provider can tell (AWS).
	OwnerAccount string
	// EncryptionKey is the default KMS key, when one is set and readable.
	EncryptionKey string
	// Encrypted is true when objects are encrypted at rest by default.
	Encrypted bool
	// Location is where the bucket is, as the provider reports it;
	// LocationVerified is false when only the connection's own setting is
	// known.
	Location         string
	LocationVerified bool
}

// PostureOptions say what the posture checks look at: the platform's
// prefixes in the bucket, and the owner the bucket must belong to (an AWS
// account, or a Google project number).
type PostureOptions struct {
	Prefixes      []string
	ExpectedOwner string
}

// PostureReport is a connection's latest posture.
type PostureReport struct {
	ConnectionID  string         `json:"connection_id"`
	CheckedAt     time.Time      `json:"checked_at"`
	Worst         string         `json:"worst"`
	PHISafe       bool           `json:"phi_safe"`
	Checks        []PostureCheck `json:"checks"`
	LifecycleHash string         `json:"-"`
	OwnerAccount  string         `json:"owner_account,omitempty"`
	// Location is where the bucket is; LocationVerified whether the
	// provider said so. EncryptionKey is the key writes use.
	Location         string `json:"location,omitempty"`
	LocationVerified bool   `json:"location_verified"`
	EncryptionKey    string `json:"encryption_key,omitempty"`
}

// phiChecks must pass for a connection to hold PHI: nothing public,
// encrypted at rest, versioned, and SSE-C refused.
var phiChecks = []string{CheckPublicAccessBlock, CheckPublicPolicy, CheckPublicACL, CheckEncryption, CheckVersioning, CheckSSECDenied}

// Summarize sets Worst and PHISafe from the checks.
func (r *PostureReport) Summarize() {
	r.Worst, r.PHISafe = PosturePass, true
	for _, c := range r.Checks {
		switch c.Status {
		case PostureFail:
			r.Worst = PostureFail
		case PostureWarn:
			if r.Worst == PosturePass {
				r.Worst = PostureWarn
			}
		}
	}
	for _, name := range phiChecks {
		if c, ok := r.Check(name); !ok || c.Status != PosturePass {
			r.PHISafe = false
		}
	}
}

// Check finds a check by name.
func (r PostureReport) Check(name string) (PostureCheck, bool) {
	for _, c := range r.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return PostureCheck{}, false
}

// Checksum modes: how request checksums are sent to an S3-family
// provider. AUTO is WHEN_SUPPORTED for AWS and WHEN_REQUIRED for others,
// which do not all accept the newer CRC checksums.
const (
	ChecksumAuto          = "AUTO"
	ChecksumWhenSupported = "WHEN_SUPPORTED"
	ChecksumWhenRequired  = "WHEN_REQUIRED"
)

func ValidChecksumMode(m string) bool {
	return m == ChecksumAuto || m == ChecksumWhenSupported || m == ChecksumWhenRequired
}

// Drift kinds: what happened to an object outside the platform.
const (
	DriftOverwritten = "OVERWRITTEN"
	DriftDeleted     = "DELETED"
	DriftNewVersion  = "NEW_VERSION"
	DriftUnreadable  = "UNREADABLE"
	DriftUnknown     = "UNKNOWN_OBJECT"
)

// DriftFinding is an object on a connection that changed, or appeared,
// outside the platform.
type DriftFinding struct {
	ID           string     `json:"id"`
	TenantID     string     `json:"-"`
	ConnectionID string     `json:"connection_id"`
	AssetID      string     `json:"asset_id,omitempty"`
	ObjectKey    string     `json:"object_key"`
	Kind         string     `json:"kind"`
	Detail       string     `json:"detail"`
	DetectedAt   time.Time  `json:"detected_at"`
	ResolvedAt   *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy   string     `json:"resolved_by,omitempty"`
}

// PlacementRules are what a dataset asks of the connection storing it.
type PlacementRules struct {
	DataClass string
	// Residency lists the locations the data may be stored in (empty is
	// anywhere); RequireCustomerKey refuses connections that do not
	// encrypt every write with a customer-managed key.
	Residency          []string
	RequireCustomerKey bool
}

// Any reports whether the rules limit placement at all.
func (r PlacementRules) Any() bool {
	return r.DataClass == DataPHI || len(r.Residency) > 0 || r.RequireCustomerKey
}

// Regulated data classes a dataset can be marked as holding.
const (
	DataPHI = "PHI"
	DataPCI = "PCI"
	DataPII = "PII"
)

func ValidDataClass(c string) bool { return c == "" || c == DataPHI || c == DataPCI || c == DataPII }

// More security alert kinds, from posture and drift checks.
const (
	AlertPosture          = "STORAGE_POSTURE"
	AlertLifecycleChanged = "LIFECYCLE_CHANGED"
	AlertOutOfBand        = "OUT_OF_BAND_WRITE"
)

var (
	// ErrPlacementRefused: the dataset's data class or rules do not allow
	// this connection.
	ErrPlacementRefused     = errors.New("this dataset's data may not be stored on this connection")
	ErrNoStagedCredential   = errors.New("no staged credential")
	ErrDriftNotFound        = errors.New("drift finding not found")
	ErrNoPreviousCredential = errors.New("no credential to roll back to")
)
