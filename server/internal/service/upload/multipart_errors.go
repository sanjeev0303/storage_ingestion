package upload

import "errors"

var (
	ErrMultipartUploadNotFound = errors.New("multipart upload not found")
	ErrMultipartNotStarted     = errors.New("multipart upload has not started")
	ErrMultipartAlreadyStarted = errors.New("multipart upload has already started")
	ErrMultipartNotActive      = errors.New("multipart upload is not active")
	ErrInvalidPart             = errors.New("invalid multipart part")
	ErrInvalidCompletion       = errors.New("invalid multipart completion request")
	ErrMultipartStartPending   = errors.New("multipart upload initialization is pending reconciliation")
	ErrObjectSizeMismatch      = errors.New("uploaded object size does not match expected size")
	ErrPartSizeMismatch        = errors.New("recorded part sizes do not match expected object size")
	// ErrOperationPending: the request was accepted but the provider's
	// outcome is not yet known; reconciliation will finish it.
	ErrOperationPending = errors.New("multipart operation accepted; outcome pending reconciliation")
)
