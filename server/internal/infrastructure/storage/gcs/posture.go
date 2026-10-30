package gcs

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

func forbidden(err error) bool {
	var gErr *googleapi.Error
	return errors.As(err, &gErr) && (gErr.Code == 403 || gErr.Code == 401)
}

// Posture describes the bucket's security settings from its attributes
// and IAM policy. Nothing is changed.
func (s *Storage) Posture(ctx context.Context, bucket string, opts model.PostureOptions) (model.PostureFacts, error) {
	var facts model.PostureFacts
	add := func(name, status, detail string) {
		facts.Checks = append(facts.Checks, model.PostureCheck{Name: name, Status: status, Detail: detail})
	}
	a, err := s.client.Bucket(bucket).Attrs(ctx)
	if err != nil {
		return facts, classify("bucket attributes", err)
	}

	if a.PublicAccessPrevention == storage.PublicAccessPreventionEnforced {
		add(model.CheckPublicAccessBlock, model.PosturePass, "public access prevention is enforced")
	} else {
		add(model.CheckPublicAccessBlock, model.PostureWarn, "public access prevention is not enforced on the bucket; enforce it")
	}

	policy, err := s.client.Bucket(bucket).IAM().Policy(ctx)
	switch {
	case err != nil && forbidden(err):
		add(model.CheckPublicPolicy, model.PostureNotChecked, "the credentials may not read the bucket's IAM policy")
	case err != nil:
		return facts, classify("bucket iam policy", err)
	default:
		public := ""
		for _, role := range policy.Roles() {
			for _, m := range policy.Members(role) {
				if m == "allUsers" || m == "allAuthenticatedUsers" {
					public = fmt.Sprintf("the IAM policy grants %s to %s; remove the binding", role, m)
				}
			}
		}
		if public != "" {
			add(model.CheckPublicPolicy, model.PostureFail, public)
		} else {
			add(model.CheckPublicPolicy, model.PosturePass, "the IAM policy grants nothing to everyone")
		}
	}

	if a.UniformBucketLevelAccess.Enabled {
		add(model.CheckPublicACL, model.PosturePass, "uniform bucket-level access is on: object ACLs do not apply")
	} else {
		status, detail := model.PostureWarn, "fine-grained ACLs are on; turn on uniform bucket-level access"
		for _, rule := range a.ACL {
			if rule.Entity == storage.AllUsers || rule.Entity == storage.AllAuthenticatedUsers {
				status, detail = model.PostureFail, fmt.Sprintf("the bucket ACL grants %s to %s; remove it", rule.Role, rule.Entity)
			}
		}
		add(model.CheckPublicACL, status, detail)
	}

	facts.Location, facts.LocationVerified = strings.ToLower(a.Location), a.Location != ""
	facts.Encrypted = true
	if a.Encryption != nil && a.Encryption.DefaultKMSKeyName != "" {
		facts.EncryptionKey = a.Encryption.DefaultKMSKeyName
		add(model.CheckEncryption, model.PosturePass, "Cloud KMS by default with key "+a.Encryption.DefaultKMSKeyName)
	} else {
		add(model.CheckEncryption, model.PosturePass, "encrypted at rest with Google-managed keys")
	}
	add(model.CheckSSECDenied, model.PostureNotChecked,
		"GCS settings do not show whether customer-supplied keys are refused; run the connection test with the write probe to try one")

	if a.VersioningEnabled {
		add(model.CheckVersioning, model.PosturePass, "object versioning is on: snapshots pin exact generations")
	} else {
		detail := "object versioning is off: turn it on so an overwrite or deletion outside the platform can be undone"
		if a.SoftDeletePolicy != nil && a.SoftDeletePolicy.RetentionDuration > 0 {
			detail += fmt.Sprintf(" (soft delete keeps deleted objects %s)", a.SoftDeletePolicy.RetentionDuration)
		}
		add(model.CheckVersioning, model.PostureWarn, detail)
	}

	switch {
	case a.RetentionPolicy != nil && a.RetentionPolicy.IsLocked:
		add(model.CheckObjectLock, model.PosturePass, fmt.Sprintf("a locked retention policy keeps every object %s", a.RetentionPolicy.RetentionPeriod))
	case a.ObjectRetentionMode == "Enabled":
		add(model.CheckObjectLock, model.PosturePass, "object retention (per-object lock) is on")
	case a.RetentionPolicy != nil:
		add(model.CheckObjectLock, model.PostureWarn, "the retention policy is not locked: an owner can still shorten it")
	default:
		add(model.CheckObjectLock, model.PostureWarn, "no retention lock: retention and holds are kept by the platform only")
	}

	canonical, _ := json.Marshal(a.Lifecycle.Rules)
	sum := sha256.Sum256(canonical)
	facts.LifecycleHash = hex.EncodeToString(sum[:])
	var fails []string
	for i, rule := range a.Lifecycle.Rules {
		if rule.Action.Type != storage.DeleteAction {
			continue
		}
		c := rule.Condition
		reaches := len(c.MatchesPrefix) == 0
		for _, p := range c.MatchesPrefix {
			for _, ours := range opts.Prefixes {
				ours = strings.TrimSuffix(ours, "/") + "/"
				if strings.HasPrefix(ours, p) || strings.HasPrefix(p, ours) {
					reaches = true
				}
			}
		}
		if !reaches {
			continue
		}
		name := "rule " + strconv.Itoa(i+1)
		if c.Liveness == storage.Archived || c.NumNewerVersions > 0 || c.DaysSinceNoncurrentTime > 0 {
			fails = append(fails, name+" deletes non-current generations: snapshots pinned to them would break")
		} else {
			fails = append(fails, name+" deletes live objects the platform has published")
		}
	}
	if len(fails) > 0 {
		add(model.CheckLifecycle, model.PostureFail, strings.Join(fails, "; "))
	} else {
		add(model.CheckLifecycle, model.PosturePass, fmt.Sprintf("%d lifecycle rules; none deletes the platform's objects", len(a.Lifecycle.Rules)))
	}
	add(model.CheckIncompleteUploads, model.PosturePass, "the platform's composite uploads are cleaned up by the platform")
	add(model.CheckCredentialScope, model.PostureNotChecked, "GCS cannot tell what else a service account may reach")

	project := strconv.FormatUint(a.ProjectNumber, 10)
	switch {
	case opts.ExpectedOwner != "" && opts.ExpectedOwner != project:
		add(model.CheckBucketOwner, model.PostureFail, "the bucket belongs to project "+project+", not "+opts.ExpectedOwner)
	case opts.ExpectedOwner != "":
		add(model.CheckBucketOwner, model.PosturePass, "the bucket belongs to project "+project+", as pinned")
	default:
		add(model.CheckBucketOwner, model.PostureWarn, "the owner is not pinned; pin project number "+project)
	}
	facts.OwnerAccount = project
	return facts, nil
}

// ProbeSSEC tries to write one small object with a random customer-
// supplied key (CSEK) and reports whether the bucket accepted it. An
// accepted object is deleted at once.
func (s *Storage) ProbeSSEC(ctx context.Context, bucket, key string) (bool, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return false, err
	}
	obj := s.client.Bucket(bucket).Object(key)
	w := obj.Key(secret).NewWriter(ctx)
	_, err := w.Write(bytes.Repeat([]byte("x"), 16))
	if err == nil {
		err = w.Close()
	} else {
		_ = w.Close()
	}
	if err == nil {
		_ = obj.Delete(context.WithoutCancel(ctx))
		return true, nil
	}
	var gErr *googleapi.Error
	if errors.As(err, &gErr) && gErr.Code >= 400 && gErr.Code < 500 {
		return false, nil
	}
	return false, classify("csek probe", err)
}
