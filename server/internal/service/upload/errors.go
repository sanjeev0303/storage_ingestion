package upload

import "errors"

var (
	ErrInvalidRequest       = errors.New("invalid upload request")
	ErrUploadTooLarge       = errors.New("upload exceeds the permitted size")
	ErrUnsupportedMediaType = errors.New("unsupported media type")
	ErrIdempotencyConflict  = errors.New("idempotency key reused with different input")
	// ErrSessionEnded: the key names a session that can no longer take
	// its bytes. The SDKs read "not active" as: start again under a new key.
	ErrSessionEnded = errors.New("upload session is not active: it has ended; start again with a new idempotency key")
)
