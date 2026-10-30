package model

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Adoption sources.
const (
	AdoptManifest    = "MANIFEST"
	AdoptS3Inventory = "S3_INVENTORY"
	AdoptMHL         = "MHL"
	AdoptBagIt       = "BAGIT"
	AdoptURLList     = "URL_LIST"
	AdoptSTAC        = "STAC"
)

// Adoption modes: what deleting an adopted asset does to its object.
const (
	AdoptionDetach = "DETACH"
	AdoptionManage = "MANAGE"
)

// Job and entry states.
const (
	AdoptionQueued    = "QUEUED"
	AdoptionReading   = "READING"
	AdoptionRunning   = "RUNNING"
	AdoptionCompleted = "COMPLETED"
	AdoptionFailed    = "FAILED"
	AdoptionCancelled = "CANCELLED"

	AdoptPending  = "PENDING"
	AdoptAdopted  = "ADOPTED"
	AdoptImported = "IMPORTED"
	AdoptRejected = "REJECTED"
	AdoptFailed   = "FAILED"
	AdoptSkipped  = "SKIPPED"
)

var (
	ErrAdoptionNotFound = errors.New("adoption job not found")
	ErrInvalidAdoption  = errors.New("invalid adoption")
)

// AdoptionJob reads one source into entries and adopts or imports each.
type AdoptionJob struct {
	ID            string     `json:"id"`
	TenantID      string     `json:"-"`
	ConnectionID  string     `json:"connection_id"`
	Dataset       string     `json:"dataset"`
	Source        string     `json:"source"`
	SourceRef     string     `json:"source_ref"`
	InlineSource  string     `json:"-"`
	Mode          string     `json:"mode"`
	RequireDigest bool       `json:"require_digest"`
	State         string     `json:"state"`
	Entries       int64      `json:"entries"`
	Adopted       int64      `json:"adopted"`
	Rejected      int64      `json:"rejected"`
	Failed        int64      `json:"failed"`
	Bytes         int64      `json:"bytes"`
	Error         string     `json:"error,omitempty"`
	CreatedBy     string     `json:"created_by"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
}

// AdoptionEntry is one object of a job, and what became of it.
type AdoptionEntry struct {
	JobID       string            `json:"-"`
	Line        int64             `json:"line"`
	TenantID    string            `json:"-"`
	ObjectKey   string            `json:"object_key,omitempty"`
	URL         string            `json:"url,omitempty"`
	Path        string            `json:"path"`
	SizeBytes   int64             `json:"size_bytes"`
	ContentType string            `json:"content_type,omitempty"`
	Expected    Digests           `json:"expected"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	State       string            `json:"state"`
	Code        string            `json:"code,omitempty"`
	Detail      string            `json:"detail,omitempty"`
	AssetID     string            `json:"asset_id,omitempty"`
	UploadID    string            `json:"upload_id,omitempty"`
	SHA256      string            `json:"sha256,omitempty"`
	Attempts    int               `json:"-"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// Digests maps an algorithm to a lower-case hex digest (C4: its "c4..." id).
type Digests map[string]string

// DigestAlgorithms are the algorithms an expected digest may use, with
// their hex length (0: C4, checked by its own form).
var DigestAlgorithms = map[string]int{
	"md5": 32, "sha1": 40, "sha256": 64, "sha512": 128, "crc32c": 8, "xxh64": 16, "xxh3": 16, "xxh128": 32, "c4": 0,
}

// Normalize lower-cases the digests and checks their form.
func (d Digests) Normalize() (Digests, error) {
	out := Digests{}
	for alg, value := range d {
		alg = strings.ToLower(strings.TrimSpace(alg))
		value = strings.TrimSpace(value)
		size, known := DigestAlgorithms[alg]
		if !known {
			return nil, fmt.Errorf("unknown digest algorithm %q", alg)
		}
		if alg == "c4" {
			if len(value) != 90 || !strings.HasPrefix(value, "c4") {
				return nil, fmt.Errorf("a C4 id is 90 characters starting with c4")
			}
			out[alg] = value
			continue
		}
		value = strings.ToLower(value)
		if _, err := hex.DecodeString(value); err != nil || len(value) != size {
			return nil, fmt.Errorf("%s must be %d hex digits", alg, size)
		}
		out[alg] = value
	}
	return out, nil
}

func ValidAdoptionSource(source string) bool {
	switch source {
	case AdoptManifest, AdoptS3Inventory, AdoptMHL, AdoptBagIt, AdoptURLList, AdoptSTAC:
		return true
	}
	return false
}

// Imports reports whether the source's objects are fetched from URLs
// rather than adopted where they are.
func AdoptionImports(source string) bool {
	return source == AdoptURLList || source == AdoptSTAC
}
