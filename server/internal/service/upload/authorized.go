package upload

import (
	"context"
	"errors"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/request"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

// The methods here are the ones the API calls: they take the
// authenticated principal, check its permission, take the tenant and
// owner from it (never from the request), and audit what happened. The
// methods they wrap stay for trusted internal callers, such as bulk
// ingestion, that have already authorized the request.

// CreateFor starts a single-request upload for p.
func (s *Service) CreateFor(ctx context.Context, p model.Principal, req request.CreateUploadRequest) (CreateResult, error) {
	if err := audit.Require(ctx, s.audit, p, model.PermUploadsWrite, "upload.create", "upload", ""); err != nil {
		return CreateResult{}, err
	}
	result, err := s.Create(ctx, CreateInput{TenantID: p.TenantID, OwnerID: p.UserID, Request: req})
	if err == nil && !result.Reused {
		s.record(ctx, p, "upload.create", result.Upload, map[string]any{"multipart": false})
	}
	return result, err
}

// CompleteFor completes p's own single-request upload. Someone else's
// upload, or another tenant's, is not found.
func (s *Service) CompleteFor(ctx context.Context, p model.Principal, uploadID string) (model.UploadStatus, error) {
	if err := audit.Require(ctx, s.audit, p, model.PermUploadsWrite, "upload.complete", "upload", uploadID); err != nil {
		return "", err
	}
	session, err := s.repository.GetByID(ctx, p.TenantID, uploadID)
	if err != nil {
		return "", err
	}
	if session.OwnerID != p.UserID || session.StorageUploadID != "" {
		return "", model.ErrUploadNotFound
	}
	status, err := s.Complete(ctx, p.TenantID, uploadID)
	if err == nil {
		s.record(ctx, p, "upload.complete", session, map[string]any{"status": string(status)})
	}
	return status, err
}

// GetFor returns one of p's tenant's upload sessions.
func (s *Service) GetFor(ctx context.Context, p model.Principal, uploadID string) (*model.UploadSession, error) {
	if err := p.Require(model.PermUploadsWrite); err != nil {
		if err := p.Require(model.PermAssetsRead); err != nil {
			return nil, err
		}
	}
	return s.repository.GetByID(ctx, p.TenantID, uploadID)
}

func (s *Service) record(ctx context.Context, p model.Principal, action string, session *model.UploadSession, details map[string]any) {
	if s.audit == nil || session == nil {
		return
	}
	e := audit.From(ctx, p, action, "upload", session.ID)
	details["size_bytes"] = session.ExpectedSize
	e.Details = details
	s.audit.Record(ctx, e)
}

// StartFor starts a multipart upload for p.
func (s *MultipartService) StartFor(ctx context.Context, p model.Principal, input StartMultipartInput) (MultipartResult, error) {
	if err := audit.Require(ctx, s.audit, p, model.PermUploadsWrite, "upload.create", "upload", ""); err != nil {
		return MultipartResult{}, err
	}
	input.TenantID, input.OwnerID = p.TenantID, p.UserID
	result, err := s.Start(ctx, input)
	if (err == nil || errors.Is(err, ErrMultipartStartPending)) && !result.Reused && result.Session.ID != "" {
		s.record(ctx, p, "upload.create", result.Session.ID, map[string]any{"multipart": true, "size_bytes": input.SizeBytes})
	}
	return result, err
}

// PresignPartsFor presigns parts of p's own multipart upload.
func (s *MultipartService) PresignPartsFor(ctx context.Context, p model.Principal, uploadID string, partNumbers []int32) ([]PartURL, error) {
	if err := audit.Require(ctx, s.audit, p, model.PermUploadsWrite, "upload.parts", "upload", uploadID); err != nil {
		return nil, err
	}
	return s.PresignParts(ctx, p.TenantID, p.UserID, uploadID, partNumbers)
}

// RecordPartsFor records parts of p's own multipart upload.
func (s *MultipartService) RecordPartsFor(ctx context.Context, p model.Principal, uploadID string, parts []RecordPartInput) error {
	if err := audit.Require(ctx, s.audit, p, model.PermUploadsWrite, "upload.parts", "upload", uploadID); err != nil {
		return err
	}
	return s.RecordParts(ctx, p.TenantID, p.UserID, uploadID, parts)
}

// CompleteFor completes p's own multipart upload.
func (s *MultipartService) CompleteFor(ctx context.Context, p model.Principal, uploadID string, input CompleteMultipartInput) (model.UploadSession, error) {
	if err := audit.Require(ctx, s.audit, p, model.PermUploadsWrite, "upload.complete", "upload", uploadID); err != nil {
		return model.UploadSession{}, err
	}
	session, err := s.Complete(ctx, p.TenantID, p.UserID, uploadID, input)
	if err == nil {
		s.record(ctx, p, "upload.complete", uploadID, map[string]any{"multipart": true, "status": string(session.Status)})
	}
	return session, err
}

// AbortFor aborts p's own multipart upload; its quota reservation is
// released with it.
func (s *MultipartService) AbortFor(ctx context.Context, p model.Principal, uploadID string) (model.UploadSession, error) {
	if err := audit.Require(ctx, s.audit, p, model.PermUploadsWrite, "upload.abort", "upload", uploadID); err != nil {
		return model.UploadSession{}, err
	}
	session, err := s.Abort(ctx, p.TenantID, p.UserID, uploadID)
	if err == nil {
		s.record(ctx, p, "upload.abort", uploadID, map[string]any{"status": string(session.Status)})
	}
	return session, err
}

func (s *MultipartService) record(ctx context.Context, p model.Principal, action, uploadID string, details map[string]any) {
	if s.audit == nil {
		return
	}
	e := audit.From(ctx, p, action, "upload", uploadID)
	e.Details = details
	s.audit.Record(ctx, e)
}
