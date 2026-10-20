package memory

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// StorageConnectionRepository keeps connections in memory, with the
// PostgreSQL repository's tenant and state rules. For tests.
type StorageConnectionRepository struct {
	mu          sync.Mutex
	connections map[string]model.StorageConnection
	credentials map[string]interfaces.SealedCredential
	defaults    map[string]string
	datasets    map[string]string // tenant/dataset -> connection
	// Dependents is what Dependents and Delete report, per connection.
	Blocking map[string]interfaces.ConnectionDependents
	// Reads counts GetConnection calls.
	Reads int
}

var (
	_ interfaces.StorageConnectionRepository = (*StorageConnectionRepository)(nil)
	_ interfaces.ConnectionLister            = (*StorageConnectionRepository)(nil)
)

func NewStorageConnectionRepository() *StorageConnectionRepository {
	return &StorageConnectionRepository{
		connections: map[string]model.StorageConnection{
			model.PlatformConnectionID: {ID: model.PlatformConnectionID, Name: "Platform-managed storage",
				Provider: model.ProviderPlatform, Status: model.ConnectionActive, AuthMethod: model.AuthPlatform, Revision: 1},
		},
		credentials: map[string]interfaces.SealedCredential{},
		defaults:    map[string]string{},
		datasets:    map[string]string{},
		Blocking:    map[string]interfaces.ConnectionDependents{},
	}
}

// Put stores a connection as it is, for test setup.
func (r *StorageConnectionRepository) Put(c model.StorageConnection, sealed interfaces.SealedCredential) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c.Revision == 0 {
		c.Revision = 1
	}
	if sealed.Version > 0 {
		c.CredentialVersion, c.CredentialKeyVersion = sealed.Version, sealed.KeyVersion
		r.credentials[c.ID] = sealed
	}
	r.connections[c.ID] = c
}

// SetDatasetConnection makes a dataset choose a connection.
func (r *StorageConnectionRepository) SetDatasetConnection(tenantID, dataset, connectionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.datasets[tenantID+"/"+dataset] = connectionID
}

func (r *StorageConnectionRepository) visible(tenantID, connectionID string) (model.StorageConnection, bool) {
	c, ok := r.connections[connectionID]
	if !ok || c.Status == model.ConnectionDeleted || (c.ID != model.PlatformConnectionID && c.TenantID != tenantID) {
		return model.StorageConnection{}, false
	}
	return c, true
}

func (r *StorageConnectionRepository) CreateConnection(_ context.Context, c model.StorageConnection, sealed interfaces.SealedCredential) (model.StorageConnection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.connections {
		if existing.TenantID == c.TenantID && existing.Status != model.ConnectionDeleted && strings.EqualFold(existing.Name, c.Name) {
			return model.StorageConnection{}, model.ErrConnectionExists
		}
	}
	c.Status, c.Revision = model.ConnectionPending, 1
	if sealed.Version > 0 {
		c.CredentialVersion, c.CredentialKeyVersion = sealed.Version, sealed.KeyVersion
		at := c.CreatedAt
		c.CredentialRotatedAt = &at
		r.credentials[c.ID] = sealed
	}
	r.connections[c.ID] = c
	return c, nil
}

func (r *StorageConnectionRepository) GetConnection(_ context.Context, tenantID, connectionID string) (model.StorageConnection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Reads++
	c, ok := r.visible(tenantID, connectionID)
	if !ok {
		return model.StorageConnection{}, model.ErrConnectionNotFound
	}
	return c, nil
}

func (r *StorageConnectionRepository) ListConnections(_ context.Context, tenantID string) ([]model.StorageConnection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []model.StorageConnection
	for id := range r.connections {
		if c, ok := r.visible(tenantID, id); ok {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b model.StorageConnection) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (r *StorageConnectionRepository) own(tenantID, connectionID string, revision int64) (model.StorageConnection, error) {
	c, ok := r.connections[connectionID]
	if !ok || c.TenantID != tenantID || c.Status == model.ConnectionDeleted {
		return c, model.ErrConnectionNotFound
	}
	if revision != 0 && c.Revision != revision {
		return c, model.ErrRevisionConflict
	}
	return c, nil
}

func (r *StorageConnectionRepository) UpdateConnection(_ context.Context, tenantID, connectionID string, revision int64, change interfaces.ConnectionChange) (model.StorageConnection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, err := r.own(tenantID, connectionID, revision)
	if err != nil {
		return c, err
	}
	moved := false
	if change.Name != nil {
		c.Name = *change.Name
	}
	if change.Region != nil && *change.Region != c.Region {
		c.Region, moved = *change.Region, true
	}
	if change.Endpoint != nil && *change.Endpoint != c.Endpoint {
		c.Endpoint, moved = *change.Endpoint, true
	}
	if change.ForcePathStyle != nil && *change.ForcePathStyle != c.ForcePathStyle {
		c.ForcePathStyle, moved = *change.ForcePathStyle, true
	}
	if change.StagingMode != nil {
		c.StagingMode = *change.StagingMode
	}
	if moved && c.Status == model.ConnectionActive {
		c.Status = model.ConnectionPending
	}
	c.Revision++
	c.UpdatedBy, c.UpdatedAt = change.Actor, change.At
	r.connections[c.ID] = c
	return c, nil
}

func (r *StorageConnectionRepository) RotateCredential(_ context.Context, tenantID, connectionID string, revision int64, sealed interfaces.SealedCredential, actor string, at time.Time) (model.StorageConnection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, err := r.own(tenantID, connectionID, revision)
	if err != nil {
		return c, err
	}
	if sealed.Version <= c.CredentialVersion {
		return c, model.ErrRevisionConflict
	}
	r.credentials[c.ID] = sealed
	c.CredentialVersion, c.CredentialKeyVersion, c.CredentialRotatedAt = sealed.Version, sealed.KeyVersion, &at
	c.Revision++
	c.UpdatedBy, c.UpdatedAt = actor, at
	r.connections[c.ID] = c
	return c, nil
}

func (r *StorageConnectionRepository) Credential(_ context.Context, tenantID, connectionID string, version int) (interfaces.SealedCredential, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, err := r.own(tenantID, connectionID, 0)
	sealed, ok := r.credentials[connectionID]
	if err != nil || !ok || c.CredentialVersion != version || sealed.Version != version {
		return interfaces.SealedCredential{}, model.ErrCredentialMissing
	}
	return sealed, nil
}

func (r *StorageConnectionRepository) RecordTest(_ context.Context, tenantID, connectionID string, result interfaces.ConnectionTestRecord) (model.StorageConnection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, err := r.own(tenantID, connectionID, 0)
	if err != nil {
		return c, err
	}
	at := result.At
	c.LastTestedAt = &at
	if result.OK {
		c.LastTestOKAt, c.LastErrorCategory = &at, ""
		if c.Status == model.ConnectionPending {
			c.Status = model.ConnectionActive
		}
	} else {
		c.LastErrorCategory = result.ErrorCategory
	}
	if result.Capabilities != nil {
		caps := *result.Capabilities
		c.Capabilities, c.CapabilitiesCheckedAt = &caps, &at
	}
	r.connections[c.ID] = c
	return c, nil
}

func (r *StorageConnectionRepository) SetStatus(_ context.Context, tenantID, connectionID string, from []model.ConnectionStatus, to model.ConnectionStatus, actor string, at time.Time) (model.StorageConnection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, err := r.own(tenantID, connectionID, 0)
	if err != nil {
		return c, err
	}
	if !slices.Contains(from, c.Status) {
		return c, model.ErrConnectionNotActive
	}
	c.Status = to
	c.DisabledAt = nil
	if to == model.ConnectionDisabled {
		c.DisabledAt = &at
	}
	c.Revision++
	c.UpdatedBy, c.UpdatedAt = actor, at
	r.connections[c.ID] = c
	return c, nil
}

func (r *StorageConnectionRepository) Dependents(_ context.Context, tenantID, connectionID string) (interfaces.ConnectionDependents, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dependents(tenantID, connectionID), nil
}

func (r *StorageConnectionRepository) dependents(tenantID, connectionID string) interfaces.ConnectionDependents {
	d := r.Blocking[connectionID]
	d.IsDefault = r.defaults[tenantID] == connectionID
	return d
}

func (r *StorageConnectionRepository) Delete(_ context.Context, tenantID, connectionID, actor string, at time.Time) (model.StorageConnection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, err := r.own(tenantID, connectionID, 0)
	if err != nil {
		return c, err
	}
	if c.Status == model.ConnectionActive || r.dependents(tenantID, connectionID).Any() {
		return model.StorageConnection{}, model.ErrConnectionInUse
	}
	delete(r.credentials, connectionID)
	c.Status, c.DeletedAt, c.CredentialVersion, c.CredentialKeyVersion = model.ConnectionDeleted, &at, 0, 0
	c.UpdatedBy, c.UpdatedAt = actor, at
	c.Revision++
	r.connections[c.ID] = c
	return c, nil
}

func (r *StorageConnectionRepository) DefaultConnection(_ context.Context, tenantID string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.defaults[tenantID]; ok {
		return id, nil
	}
	return model.PlatformConnectionID, nil
}

func (r *StorageConnectionRepository) SetDefault(_ context.Context, tenantID, connectionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.visible(tenantID, connectionID)
	if !ok {
		return model.ErrConnectionNotFound
	}
	if !c.AcceptsNewWork() {
		return model.ErrConnectionNotActive
	}
	r.defaults[tenantID] = connectionID
	return nil
}

func (r *StorageConnectionRepository) DatasetConnection(_ context.Context, tenantID, dataset string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.datasets[tenantID+"/"+dataset], nil
}

func (r *StorageConnectionRepository) ConnectionsHoldingWork(_ context.Context, tenantID string) ([]model.StorageConnection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []model.StorageConnection
	for _, c := range r.connections {
		if c.ID == model.PlatformConnectionID || (tenantID != "" && c.TenantID != tenantID) {
			continue
		}
		if c.Status == model.ConnectionActive || c.Status == model.ConnectionDisabled {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b model.StorageConnection) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}
