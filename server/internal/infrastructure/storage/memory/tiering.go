package memory

import (
	"context"
	"fmt"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

var _ interfaces.TierStorage = (*ObjectStorage)(nil)

// tierState is what the memory store keeps per key for tiering.
type tierState struct {
	tier      model.StorageTier
	restoring bool
	until     *time.Time
}

// SetTier moves an object between tiers, from the bytes with etag only.
func (s *ObjectStorage) SetTier(_ context.Context, _ string, key, wantETag string, size int64, tier model.StorageTier) (interfaces.ObjectMetadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[key]
	if !ok {
		return interfaces.ObjectMetadata{}, fmt.Errorf("set tier: %w", interfaces.ErrObjectNotFound)
	}
	if etag(data) != wantETag || int64(len(data)) != size {
		return interfaces.ObjectMetadata{}, fmt.Errorf("set tier: %w", interfaces.ErrPreconditionFailed)
	}
	if s.tiers == nil {
		s.tiers = map[string]tierState{}
	}
	s.tiers[key] = tierState{tier: tier}
	s.TierCalls++
	return interfaces.ObjectMetadata{SizeBytes: size, ETag: wantETag, VersionID: s.versions[key]}, nil
}

func (s *ObjectStorage) TierState(_ context.Context, _ string, key, _ string) (interfaces.TierState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objects[key]; !ok {
		return interfaces.TierState{}, fmt.Errorf("read tier: %w", interfaces.ErrObjectNotFound)
	}
	t, ok := s.tiers[key]
	if !ok {
		return interfaces.TierState{Tier: model.TierStandard}, nil
	}
	return interfaces.TierState{Tier: t.tier, Restoring: t.restoring, ReadableUntil: t.until}, nil
}

// Restore starts a restore; FinishRestore completes it, as the provider
// would hours later.
func (s *ObjectStorage) Restore(_ context.Context, _ string, key, _ string, _ int, _ model.RestoreSpeed) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tiers[key]
	if t.until == nil {
		t.restoring = true
	}
	s.tiers[key] = t
	return nil
}

// FinishRestore makes an archived object readable until until.
func (s *ObjectStorage) FinishRestore(key string, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tiers[key]
	t.restoring, t.until = false, &until
	s.tiers[key] = t
}

// Tier is an object's tier, for tests.
func (s *ObjectStorage) Tier(key string) model.StorageTier {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tiers[key]; ok {
		return t.tier
	}
	return model.TierStandard
}
