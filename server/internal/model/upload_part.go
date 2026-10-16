package model

import (
	"errors"
	"strings"
	"time"
)

type UploadPartStatus string

const (
	UploadPartPending  UploadPartStatus = "PENDING"
	UploadPartUploaded UploadPartStatus = "UPLOADED"
)

// UploadPart stores metadata about one part of a multipart upload.
// It stores metadata, not the actual file bytes.
type UploadPart struct {
	TenantID       string
	UploadID       string
	PartNumber     int32
	SizeBytes      int64
	ETag           string
	ChecksumSHA256 string
	Status         UploadPartStatus
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

var ErrInvalidUploadPart = errors.New("invalid upload part")

func (p UploadPart) Validate() error {
	if strings.TrimSpace(p.TenantID) == "" || strings.TrimSpace(p.UploadID) == "" {
		return ErrInvalidUploadPart
	}

	// S3 multipart uploads support part umbers from 1 to 100000.
	if p.PartNumber < 1 || p.PartNumber > 10_000 {
		return ErrInvalidUploadPart
	}

	if p.SizeBytes < 0 {
		return ErrInvalidUploadPart
	}

	switch p.Status {
	case UploadPartPending:
		// A pending part may not have an ETag or uploaded size yet.
		return nil

	case UploadPartUploaded:
		// An uploaded part must have identifying metadata.
		if strings.TrimSpace(p.ETag) == "" || p.SizeBytes <= 0 {
			return ErrInvalidUploadPart
		}
		return nil
	default:
		return ErrInvalidUploadPart
	}
}
