package memory

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// QuarantineRepository mirrors the PostgreSQL quarantine table. For tests.
type QuarantineRepository struct {
	mu      sync.Mutex
	records []model.QuarantineRecord
	nextID  int
}

var _ interfaces.QuarantineRepository = (*QuarantineRepository)(nil)

func NewQuarantineRepository() *QuarantineRepository {
	return &QuarantineRepository{}
}

func (r *QuarantineRepository) Hold(ctx context.Context, record model.QuarantineRecord) (model.QuarantineRecord, error) {
	if err := ctx.Err(); err != nil {
		return model.QuarantineRecord{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	for i, existing := range r.records {
		if existing.TenantID == record.TenantID && existing.AssetID == record.AssetID && existing.Status == model.QuarantineHeld {
			existing.Category = record.Category
			existing.Reason = record.Reason
			existing.ScanResult = record.ScanResult
			existing.ScanSignature = record.ScanSignature
			existing.UpdatedAt = record.CreatedAt
			r.records[i] = existing
			return existing, nil
		}
	}

	r.nextID++
	record.ID = fmt.Sprintf("quarantine-%d", r.nextID)
	record.Status = model.QuarantineHeld
	record.UpdatedAt = record.CreatedAt
	r.records = append(r.records, record)
	return record, nil
}

func (r *QuarantineRepository) Reject(ctx context.Context, record model.QuarantineRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	at := record.CreatedAt
	for i, existing := range r.records {
		if existing.TenantID != record.TenantID || existing.AssetID != record.AssetID {
			continue
		}
		switch existing.Status {
		case model.QuarantineRejected, model.QuarantinePurged:
			return nil
		case model.QuarantineHeld:
			existing.Status = model.QuarantineRejected
			existing.Category = record.Category
			existing.Reason = record.Reason
			existing.ScanResult = record.ScanResult
			existing.ScanSignature = record.ScanSignature
			existing.RetainUntil = record.RetainUntil
			existing.UpdatedAt = at
			existing.ResolvedAt = &at
			r.records[i] = existing
			return nil
		}
	}

	r.nextID++
	record.ID = fmt.Sprintf("quarantine-%d", r.nextID)
	record.Status = model.QuarantineRejected
	record.UpdatedAt = at
	record.ResolvedAt = &at
	r.records = append(r.records, record)
	return nil
}

func (r *QuarantineRepository) Release(ctx context.Context, tenantID, assetID string, at time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	for i, existing := range r.records {
		if existing.TenantID == tenantID && existing.AssetID == assetID && existing.Status == model.QuarantineHeld {
			existing.Status = model.QuarantineReleased
			existing.UpdatedAt = at
			existing.ResolvedAt = &at
			r.records[i] = existing
			return true, nil
		}
	}
	return false, nil
}

func (r *QuarantineRepository) DuePurge(ctx context.Context, now time.Time, limit int) ([]model.QuarantineRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	var due []model.QuarantineRecord
	for _, record := range r.records {
		if record.Status == model.QuarantineRejected && record.RetainUntil != nil && !record.RetainUntil.After(now) {
			due = append(due, record)
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].RetainUntil.Before(*due[j].RetainUntil) })
	if len(due) > limit {
		due = due[:limit]
	}
	return due, nil
}

func (r *QuarantineRepository) MarkPurged(ctx context.Context, id string, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	for i, record := range r.records {
		if record.ID == id && record.Status == model.QuarantineRejected {
			record.Status = model.QuarantinePurged
			record.UpdatedAt = at
			r.records[i] = record
		}
	}
	return nil
}

func (r *QuarantineRepository) ListForAsset(ctx context.Context, tenantID, assetID string) ([]model.QuarantineRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []model.QuarantineRecord
	for i := len(r.records) - 1; i >= 0; i-- {
		if r.records[i].TenantID == tenantID && r.records[i].AssetID == assetID {
			out = append(out, r.records[i])
		}
	}
	return out, nil
}

// ProcessingEventRepository keeps audit events in memory. For tests.
type ProcessingEventRepository struct {
	mu     sync.Mutex
	events []model.ProcessingEvent
}

var _ interfaces.ProcessingEventRepository = (*ProcessingEventRepository)(nil)

func NewProcessingEventRepository() *ProcessingEventRepository {
	return &ProcessingEventRepository{}
}

func (r *ProcessingEventRepository) Record(ctx context.Context, event model.ProcessingEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	event.ID = fmt.Sprintf("event-%d", len(r.events)+1)
	r.events = append(r.events, event)
	return nil
}

func (r *ProcessingEventRepository) ListForAsset(ctx context.Context, tenantID, assetID string) ([]model.ProcessingEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []model.ProcessingEvent
	for _, event := range r.events {
		if event.TenantID == tenantID && event.AssetID == assetID {
			out = append(out, event)
		}
	}
	return out, nil
}

// ContentPolicyRepository holds tenant policies set by tests.
type ContentPolicyRepository struct {
	mu       sync.Mutex
	policies map[string]model.ContentPolicy
}

var _ interfaces.ContentPolicyRepository = (*ContentPolicyRepository)(nil)

func NewContentPolicyRepository() *ContentPolicyRepository {
	return &ContentPolicyRepository{policies: make(map[string]model.ContentPolicy)}
}

func (r *ContentPolicyRepository) Set(tenantID string, policy model.ContentPolicy) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.policies[tenantID] = policy
}

func (r *ContentPolicyRepository) Get(ctx context.Context, tenantID string) (model.ContentPolicy, bool, error) {
	if err := ctx.Err(); err != nil {
		return model.ContentPolicy{}, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	policy, ok := r.policies[tenantID]
	return policy, ok, nil
}
