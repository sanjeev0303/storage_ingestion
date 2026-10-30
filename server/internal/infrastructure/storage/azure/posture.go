package azure

import (
	"context"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

const accountLevel = "set on the storage account, which container credentials cannot read; check it in the Azure portal"

// Posture describes what a container's credentials can see. Versioning,
// lifecycle management and key settings live on the storage account,
// beyond them, and are reported as not checked.
func (s *Storage) Posture(ctx context.Context, bucket string, opts model.PostureOptions) (model.PostureFacts, error) {
	var facts model.PostureFacts
	add := func(name, status, detail string) {
		facts.Checks = append(facts.Checks, model.PostureCheck{Name: name, Status: status, Detail: detail})
	}
	props, err := s.container(bucket).GetProperties(ctx, nil)
	if err != nil {
		return facts, classify("container properties", err)
	}
	if props.BlobPublicAccess != nil && *props.BlobPublicAccess != "" {
		add(model.CheckPublicAccessBlock, model.PostureFail,
			"the container allows anonymous "+string(*props.BlobPublicAccess)+" reads; set its access level to private")
	} else {
		add(model.CheckPublicAccessBlock, model.PosturePass, "the container's access level is private")
	}
	add(model.CheckPublicPolicy, model.PostureNotChecked, "role assignments are "+accountLevel)
	add(model.CheckPublicACL, model.PostureNotChecked, "stored access policies are "+accountLevel)

	facts.Encrypted = true
	if props.DefaultEncryptionScope != nil && *props.DefaultEncryptionScope != "" && *props.DefaultEncryptionScope != "$account-encryption-key" {
		facts.EncryptionKey = *props.DefaultEncryptionScope
		detail := "the container encrypts with scope " + facts.EncryptionKey + " by default"
		if props.DenyEncryptionScopeOverride != nil && *props.DenyEncryptionScopeOverride {
			detail += ", and no write may use another"
		}
		add(model.CheckEncryption, model.PosturePass, detail)
	} else {
		add(model.CheckEncryption, model.PosturePass, "Azure Storage encrypts all data at rest; whether the key is customer-managed is "+accountLevel)
	}
	add(model.CheckSSECDenied, model.PostureNotChecked, "whether customer-provided keys are refused is "+accountLevel)

	worm := props.IsImmutableStorageWithVersioningEnabled != nil && *props.IsImmutableStorageWithVersioningEnabled
	if worm {
		add(model.CheckVersioning, model.PosturePass, "version-level immutability is on, which needs blob versioning")
	} else {
		add(model.CheckVersioning, model.PostureNotChecked, "blob versioning is "+accountLevel)
	}
	switch {
	case worm:
		add(model.CheckObjectLock, model.PosturePass, "version-level immutability (WORM) is on")
	case props.HasImmutabilityPolicy != nil && *props.HasImmutabilityPolicy:
		add(model.CheckObjectLock, model.PosturePass, "the container has an immutability policy")
	default:
		add(model.CheckObjectLock, model.PostureWarn, "no immutability policy: retention and holds are kept by the platform only")
	}
	add(model.CheckLifecycle, model.PostureNotChecked, "lifecycle management is "+accountLevel)
	add(model.CheckIncompleteUploads, model.PosturePass, "uncommitted blocks expire on their own after seven days")
	add(model.CheckCredentialScope, model.PostureNotChecked, "a shared key reaches the whole account; prefer a SAS for this container")
	return facts, nil
}
