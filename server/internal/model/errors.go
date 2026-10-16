package model

import "errors"

var (
	ErrUploadNotFound      = errors.New("upload not found")
	ErrAssetNotFound       = errors.New("asset not found")
	ErrInvalidUploadState  = errors.New("invalid upload state")
	ErrInvalidAssetState   = errors.New("invalid asset state")
	ErrUploadAlreadyExists = errors.New("upload already exists")
	ErrIdempotencyConflict = errors.New("idempotency key conflict")
)
