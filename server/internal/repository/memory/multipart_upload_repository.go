package memory

import (
	"context"
	"sort"
	"sync"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// MultipartUploadRepository keeps part metadata in memory. For tests only.
type MultipartUploadRepository struct {
	mu    sync.Mutex
	parts map[string]map[int32]model.UploadPart // tenant/upload -> part number
}

var _ interfaces.MultipartUploadRepository = (*MultipartUploadRepository)(nil)

func NewMultipartUploadRepository() *MultipartUploadRepository {
	return &MultipartUploadRepository{parts: make(map[string]map[int32]model.UploadPart)}
}

func (r *MultipartUploadRepository) UpsertPart(ctx context.Context, tenantID string, part model.UploadPart) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := part.Validate(); err != nil {
		return err
	}
	if tenantID == "" || tenantID != part.TenantID {
		return model.ErrInvalidUploadPart
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	key := tenantID + "\x00" + part.UploadID
	if r.parts[key] == nil {
		r.parts[key] = make(map[int32]model.UploadPart)
	}
	r.parts[key][part.PartNumber] = part
	return nil
}

func (r *MultipartUploadRepository) ListParts(ctx context.Context, tenantID string, uploadID string) ([]model.UploadPart, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]model.UploadPart, 0)
	for _, p := range r.parts[tenantID+"\x00"+uploadID] {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PartNumber < out[j].PartNumber })
	return out, nil
}

func (r *MultipartUploadRepository) DeleteParts(ctx context.Context, tenantID string, uploadID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.parts, tenantID+"\x00"+uploadID)
	return nil
}
