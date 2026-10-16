package upload

import "errors"

var (
	ErrInvalidRequest       = errors.New("invalid upload request")
	ErrUploadTooLarge       = errors.New("upload exceeds the permitted size")
	ErrUnsupportedMediaType = errors.New("unsupported media type")
	ErrIdempotencyConflict  = errors.New("idempotency key reused with different input")
)
