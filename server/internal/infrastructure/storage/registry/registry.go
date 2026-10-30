// Package registry resolves storage connections to provider clients.
//
// The platform connection is the deployment's own storage, built at
// startup. Every other connection is a tenant's: its row is read from the
// database (cached for a few seconds), its sealed credential is opened
// only to build the client, and the client is cached by connection,
// credential version and revision, so a rotation or a configuration change
// builds a new one and the old one is dropped. The cache is bounded and
// every entry expires, so a client built from revoked credentials is not
// kept. Nothing here is global: each process builds its own registry.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/secretbox"
)

var (
	_ interfaces.StorageRouter  = (*Registry)(nil)
	_ interfaces.StorageChooser = (*Registry)(nil)
)

// BuildFunc builds a connection's provider client from its decrypted
// secrets. The returned close func releases it.
type BuildFunc func(ctx context.Context, c model.StorageConnection, secrets model.StorageSecrets) (interfaces.StorageProvider, func(), error)

// Dependencies of a registry.
type Dependencies struct {
	Config      config.Storage
	Platform    interfaces.StorageLocation
	Connections interfaces.StorageConnectionRepository
	// Sealer opens stored credentials; nil when BYOS is off, and then only
	// the platform connection resolves.
	Sealer  secretbox.Sealer
	Metrics *metrics.Registry
	// Build builds clients; NewBuilder's is the real one, tests pass fakes.
	Build BuildFunc
	Now   func() time.Time
}

// Registry is the storage router and chooser of one process.
type Registry struct {
	deps  Dependencies
	now   func() time.Time
	guard interfaces.PlacementGuard

	mu      sync.Mutex
	clients map[string]*cachedClient // by connection ID
	rows    map[string]cachedRow     // by connection ID
	tick    int64
}

type cachedClient struct {
	key      string // connection ID, credential version and revision
	provider *limited
	close    func()
	built    time.Time
	used     int64
}

type cachedRow struct {
	conn model.StorageConnection
	at   time.Time
}

// New builds a registry.
func New(deps Dependencies) (*Registry, error) {
	if deps.Platform.Storage == nil || deps.Platform.ConnectionID != model.PlatformConnectionID {
		return nil, errors.New("the registry needs the platform storage location")
	}
	if deps.Connections == nil || deps.Build == nil {
		return nil, errors.New("the registry needs the connection repository and a client builder")
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return &Registry{deps: deps, now: now, clients: map[string]*cachedClient{}, rows: map[string]cachedRow{}}, nil
}

// Platform is the platform connection's location.
func (r *Registry) Platform() interfaces.StorageLocation { return r.deps.Platform }

// Locate resolves a record's bound connection. A deleted, pending or
// unknown connection, or one of another tenant, does not resolve.
func (r *Registry) Locate(ctx context.Context, tenantID, connectionID string) (interfaces.StorageLocation, error) {
	if connectionID == model.PlatformConnectionID {
		return r.deps.Platform, nil
	}
	if connectionID == "" {
		return interfaces.StorageLocation{}, fmt.Errorf("%w: the record has no storage binding", model.ErrConnectionNotFound)
	}
	missing := 0 // the newest credential version found missing
	for {
		c, err := r.row(ctx, tenantID, connectionID)
		if err != nil {
			return interfaces.StorageLocation{}, err
		}
		if !c.HoldsWork() {
			return interfaces.StorageLocation{}, fmt.Errorf("%w: it is %s", model.ErrConnectionNotActive, c.Status)
		}
		loc, err := r.location(ctx, c)
		// Only the newest credential is stored: one missing at the row's
		// version was rotated after the row was read, here or in another
		// process. Read the row again, as long as that finds a newer version.
		if errors.Is(err, model.ErrCredentialMissing) && c.CredentialVersion > missing {
			missing = c.CredentialVersion
			r.mu.Lock()
			delete(r.rows, connectionID)
			r.mu.Unlock()
			continue
		}
		return loc, err
	}
}

// ChooseForUpload picks the connection for a new upload: the one asked
// for, else the dataset's, else the organization's default. Asking for a
// connection other than the default needs storage.use. The connection is
// read fresh, so one disabled a moment ago takes no new upload.
func (r *Registry) ChooseForUpload(ctx context.Context, p model.Principal, requested, dataset string) (interfaces.StorageChoice, error) {
	defaultID, err := r.deps.Connections.DefaultConnection(ctx, p.TenantID)
	if err != nil {
		return interfaces.StorageChoice{}, err
	}
	id := requested
	if id != "" && id != defaultID {
		if err := p.Require(model.PermStorageUse); err != nil {
			return interfaces.StorageChoice{}, err
		}
	}
	if id == "" && dataset != "" {
		if id, err = r.deps.Connections.DatasetConnection(ctx, p.TenantID, dataset); err != nil {
			return interfaces.StorageChoice{}, err
		}
	}
	if id == "" {
		id = defaultID
	}
	if id == model.PlatformConnectionID {
		return interfaces.StorageChoice{Final: r.deps.Platform, Staging: r.deps.Platform}, nil
	}
	c, err := r.deps.Connections.GetConnection(ctx, p.TenantID, id)
	if err != nil {
		return interfaces.StorageChoice{}, err
	}
	r.remember(c)
	if !c.AcceptsNewWork() {
		return interfaces.StorageChoice{}, fmt.Errorf("%w: it is %s", model.ErrConnectionNotActive, c.Status)
	}
	if r.guard != nil && dataset != "" {
		if err := r.guard.CheckDatasetPlacement(ctx, p, dataset, c.ID); err != nil {
			return interfaces.StorageChoice{}, err
		}
	}
	final, err := r.location(ctx, c)
	if err != nil {
		return interfaces.StorageChoice{}, err
	}
	staging := final
	if c.StagingMode == model.StagingPlatform {
		staging = r.deps.Platform
	}
	return interfaces.StorageChoice{Final: final, Staging: staging}, nil
}

// GuardPlacement checks every upload's connection against its dataset's
// placement rules.
func (r *Registry) GuardPlacement(g interfaces.PlacementGuard) { r.guard = g }

// Invalidate drops what the registry holds for a connection: called when
// it is rotated, changed, disabled or deleted in this process. Other
// processes notice by the row cache's expiry.
func (r *Registry) Invalidate(connectionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.rows, connectionID)
	if e := r.clients[connectionID]; e != nil {
		e.close()
		delete(r.clients, connectionID)
	}
}

// Build builds a client for a connection that is not saved yet or not
// active, for a connection test. It is not cached.
func (r *Registry) Build(ctx context.Context, c model.StorageConnection, secrets model.StorageSecrets) (interfaces.StorageProvider, func(), error) {
	return r.deps.Build(ctx, c, secrets)
}

// BuildStored builds an uncached client from a connection's stored
// credentials, for a connection test.
func (r *Registry) BuildStored(ctx context.Context, c model.StorageConnection) (interfaces.StorageProvider, func(), error) {
	return r.open(ctx, c)
}

func (r *Registry) row(ctx context.Context, tenantID, connectionID string) (model.StorageConnection, error) {
	r.mu.Lock()
	cached, ok := r.rows[connectionID]
	r.mu.Unlock()
	if ok && cached.conn.TenantID == tenantID && r.now().Sub(cached.at) < r.deps.Config.ConnectionCacheTTL {
		return cached.conn, nil
	}
	c, err := r.deps.Connections.GetConnection(ctx, tenantID, connectionID)
	if err != nil {
		return model.StorageConnection{}, err
	}
	r.remember(c)
	return c, nil
}

func (r *Registry) remember(c model.StorageConnection) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// ponytail: dropping every row when the map is full is crude, but rows
	// are cheap to read again.
	if len(r.rows) >= 4*r.cacheSize() {
		clear(r.rows)
	}
	r.rows[c.ID] = cachedRow{conn: c, at: r.now()}
}

func (r *Registry) cacheSize() int {
	if r.deps.Config.ClientCacheSize > 0 {
		return r.deps.Config.ClientCacheSize
	}
	return 256
}

func (r *Registry) location(ctx context.Context, c model.StorageConnection) (interfaces.StorageLocation, error) {
	provider, err := r.client(ctx, c)
	if err != nil {
		return interfaces.StorageLocation{}, err
	}
	loc := interfaces.StorageLocation{
		StoragePlace: interfaces.StoragePlace{
			ConnectionID:  c.ID,
			StagingBucket: c.Bucket, StagingPrefix: c.StagingPrefix,
			ReadyBucket: c.Bucket, ReadyPrefix: c.ReadyPrefix,
		},
		Provider: c.Provider,
		Storage:  provider,
	}
	if c.Capabilities != nil {
		loc.Capabilities = *c.Capabilities
	}
	return loc, nil
}

// client returns the cached client for the connection's current
// credential version and revision, building one when there is none.
func (r *Registry) client(ctx context.Context, c model.StorageConnection) (*limited, error) {
	key := c.ID + "/" + strconv.Itoa(c.CredentialVersion) + "/" + strconv.FormatInt(c.Revision, 10)
	r.mu.Lock()
	if e := r.clients[c.ID]; e != nil && e.key == key && r.now().Sub(e.built) < r.deps.Config.ClientCacheTTL {
		r.tick++
		e.used = r.tick
		r.mu.Unlock()
		return e.provider, nil
	}
	r.mu.Unlock()

	built, closeFn, err := r.open(ctx, c)
	if err != nil {
		r.deps.Metrics.Add("storage.client_build_errors", 1)
		return nil, err
	}
	r.deps.Metrics.Add("storage.client_builds", 1)
	provider := newLimited(built, c.Provider, r.deps.Config.MaxConcurrentPerConnection, r.deps.Config.OperationTimeout, r.deps.Metrics)

	r.mu.Lock()
	defer r.mu.Unlock()
	if old := r.clients[c.ID]; old != nil {
		old.close()
	}
	r.tick++
	r.clients[c.ID] = &cachedClient{key: key, provider: provider, close: closeFn, built: r.now(), used: r.tick}
	if len(r.clients) > r.cacheSize() {
		r.evictOldest()
	}
	r.deps.Metrics.Set("storage.cached_clients", int64(len(r.clients)))
	return provider, nil
}

// evictOldest drops the least recently used client.
func (r *Registry) evictOldest() {
	oldestID := ""
	var oldest int64
	for id, e := range r.clients {
		if oldestID == "" || e.used < oldest {
			oldestID, oldest = id, e.used
		}
	}
	r.clients[oldestID].close()
	delete(r.clients, oldestID)
}

// open decrypts the connection's credential and builds its client. The
// plaintext lives only for the build.
func (r *Registry) open(ctx context.Context, c model.StorageConnection) (interfaces.StorageProvider, func(), error) {
	if r.deps.Sealer == nil {
		return nil, nil, model.ErrStorageNotConfigured
	}
	var secrets model.StorageSecrets
	if c.HasCredential() {
		sealed, err := r.deps.Connections.Credential(ctx, c.TenantID, c.ID, c.CredentialVersion)
		if err != nil {
			return nil, nil, err
		}
		plain, err := r.deps.Sealer.Open(secretbox.Sealed{KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext},
			c.CredentialAAD(c.CredentialVersion))
		if err != nil {
			return nil, nil, fmt.Errorf("%w: the stored credentials could not be decrypted", model.ErrCredentialMissing)
		}
		err = json.Unmarshal(plain, &secrets)
		clear(plain)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: the stored credentials are unreadable", model.ErrCredentialMissing)
		}
	} else if c.AuthMethod != model.AuthAssumeRole {
		return nil, nil, model.ErrCredentialMissing
	}
	return r.deps.Build(ctx, c, secrets)
}
