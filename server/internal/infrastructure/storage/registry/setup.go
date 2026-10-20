package registry

import (
	"fmt"

	s3storage "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/s3"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/netguard"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/secretbox"
)

// Policy is the network policy for tenants' endpoints under cfg.
func Policy(cfg config.Storage) netguard.Policy {
	policy := netguard.DefaultPolicy()
	policy.AllowPrivate = cfg.AllowPrivateEndpoints
	return policy
}

// Sealer opens and seals stored credentials; nil while BYOS is off.
func Sealer(cfg config.Storage) (secretbox.Sealer, error) {
	if !cfg.BYOSEnabled {
		return nil, nil
	}
	keyring, err := cfg.Keyring()
	if err != nil {
		return nil, fmt.Errorf("storage credential keys: %w", err)
	}
	return keyring, nil
}

// ForProcess builds a process's registry around the platform's own
// storage: its quarantine bucket and its delivery bucket and prefix.
// Tenants' connections resolve only when BYOS is enabled, which needs the
// credential keys; without it every record bound to a tenant connection
// fails to resolve (model.ErrStorageNotConfigured) instead of falling back
// to the platform.
func ForProcess(cfg config.Storage, platform *s3storage.ObjectStorage, quarantineBucket, deliveryBucket, deliveryPrefix string,
	connections interfaces.StorageConnectionRepository, registry *metrics.Registry) (*Registry, error) {
	sealer, err := Sealer(cfg)
	if err != nil {
		return nil, err
	}
	platform.SetTransferPartBytes(cfg.TransferPartBytes)
	return New(Dependencies{
		Config: cfg,
		Platform: interfaces.StorageLocation{
			StoragePlace: interfaces.StoragePlace{
				ConnectionID:  model.PlatformConnectionID,
				StagingBucket: quarantineBucket, StagingPrefix: "quarantine",
				ReadyBucket: deliveryBucket, ReadyPrefix: deliveryPrefix,
			},
			Provider: model.ProviderPlatform,
			Storage:  platform,
		},
		Connections: connections,
		Sealer:      sealer,
		Metrics:     registry,
		Build:       NewBuilder(Policy(cfg), cfg.TransferPartBytes).Build,
	})
}
