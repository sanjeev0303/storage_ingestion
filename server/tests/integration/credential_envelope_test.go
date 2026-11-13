package integration

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/secretbox"
)

func openCredential(t *testing.T, envelope *secretbox.Envelope, c model.StorageConnection, s interfaces.SealedCredential) string {
	t.Helper()
	plain, err := envelope.Open(secretbox.Sealed{KeyVersion: s.KeyVersion, Nonce: s.Nonce, Ciphertext: s.Ciphertext, WrappedKey: s.WrappedKey},
		c.CredentialAAD(s.Version))
	if err != nil {
		t.Fatalf("%s v%d does not open: %v", c.ID, s.Version, err)
	}
	return string(plain)
}

// A master-key rotation: credentials sealed before envelopes and envelopes
// under the old key keep opening while both keys are loaded, the re-seal
// moves every one of them (current and staged) to the new key, and after
// it the old key is not needed. Deleted connections keep nothing.
func TestResealMovesStoredCredentialsToTheNewestKey(t *testing.T) {
	e := openSecurityEnv(t, nil)
	ctx := context.Background()
	owner, _ := e.account(t, "reseal")
	org := e.organization(t, "reseal", owner)
	repo := postgres.NewStorageConnectionRepository(e.db)
	oldKey := testKeyring(t)
	both, err := secretbox.NewKeyring(map[int][]byte{1: bytes.Repeat([]byte{0x5a}, 32), 2: bytes.Repeat([]byte{0x6b}, 32)}, 2)
	if err != nil {
		t.Fatal(err)
	}
	newOnly, _ := secretbox.NewKeyring(map[int][]byte{2: bytes.Repeat([]byte{0x6b}, 32)}, 2)

	// Sealed before envelopes, with a staged credential in an envelope
	// under the old key.
	c := activeConnection(t, repo, org.ID, "legacy", oldKey)
	staged, err := secretbox.NewEnvelope(oldKey).Seal([]byte("staged-secret"), c.CredentialAAD(2))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.StageCredential(ctx, org.ID, c.ID, interfaces.SealedCredential{Version: 2, KeyVersion: staged.KeyVersion,
		Nonce: staged.Nonce, Ciphertext: staged.Ciphertext, WrappedKey: staged.WrappedKey}, "it", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	before, err := repo.Credential(ctx, org.ID, c.ID, 1)
	if err != nil || len(before.WrappedKey) != 0 {
		t.Fatalf("the current credential predates envelopes: %v", err)
	}
	// During the move both open with both keys loaded.
	openCredential(t, secretbox.NewEnvelope(both), c, before)

	// A dry run changes nothing.
	ours := func(rewrap func(*postgres.SealedCredentials) error) func(*postgres.SealedCredentials) error {
		return func(s *postgres.SealedCredentials) error {
			if s.Connection.TenantID != org.ID {
				return errors.New("another test's connection: leave it")
			}
			return rewrap(s)
		}
	}
	envelope := secretbox.NewEnvelope(both)
	rewrap := func(s *postgres.SealedCredentials) error {
		for _, cred := range []*interfaces.SealedCredential{&s.Current, &s.Staged, &s.Previous} {
			if cred.Version == 0 {
				continue
			}
			moved, err := envelope.Rewrap(secretbox.Sealed{KeyVersion: cred.KeyVersion, Nonce: cred.Nonce, Ciphertext: cred.Ciphertext,
				WrappedKey: cred.WrappedKey}, s.Connection.CredentialAAD(cred.Version))
			if err != nil {
				return err
			}
			cred.KeyVersion, cred.Nonce, cred.Ciphertext, cred.WrappedKey = moved.KeyVersion, moved.Nonce, moved.Ciphertext, moved.WrappedKey
		}
		return nil
	}
	walk := func(write bool) int {
		total, after := 0, ""
		for {
			last, n, err := repo.ResealBatch(ctx, 2, after, 1, write, ours(rewrap))
			if err != nil {
				t.Fatal(err)
			}
			if last == "" {
				return total
			}
			total, after = total+n, last
		}
	}
	if n := walk(false); n != 1 {
		t.Fatalf("the dry run counts the one connection: %d", n)
	}
	if again, _ := repo.Credential(ctx, org.ID, c.ID, 1); again.KeyVersion != 1 || len(again.WrappedKey) != 0 {
		t.Fatal("the dry run wrote")
	}

	if n := walk(true); n != 1 {
		t.Fatalf("resealed %d connections, want 1", n)
	}
	if n := walk(true); n != 0 {
		t.Fatalf("a second run found %d more: it must be idempotent", n)
	}
	current, err := repo.Credential(ctx, org.ID, c.ID, 1)
	if err != nil || current.KeyVersion != 2 || len(current.WrappedKey) == 0 {
		t.Fatalf("current credential after the re-seal: key %d, enveloped %v, %v", current.KeyVersion, len(current.WrappedKey) > 0, err)
	}
	if got := openCredential(t, secretbox.NewEnvelope(newOnly), c, current); got != `{"AccessKeyID":"AKIDIT","SecretAccessKey":"it-secret-value"}` {
		t.Fatal("the re-sealed credential changed")
	}
	stagedNow, err := repo.StagedCredential(ctx, org.ID, c.ID)
	if err != nil || stagedNow.KeyVersion != 2 {
		t.Fatalf("staged credential after the re-seal: %+v %v", stagedNow.KeyVersion, err)
	}
	if openCredential(t, secretbox.NewEnvelope(newOnly), c, stagedNow) != "staged-secret" {
		t.Fatal("the staged credential changed")
	}
	stored, _ := repo.GetConnection(ctx, org.ID, c.ID)
	if stored.Revision != c.Revision+1 { // the staging bumped it once; the re-seal did not
		t.Fatalf("the re-seal changed the revision: %d -> %d", c.Revision, stored.Revision)
	}

	// Promotion and rollback carry the envelope with the credential.
	if _, err := repo.PromoteCredential(ctx, org.ID, c.ID, 2, "it", time.Now().Add(time.Hour), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	promoted, _ := repo.Credential(ctx, org.ID, c.ID, 2)
	if openCredential(t, secretbox.NewEnvelope(newOnly), c, promoted) != "staged-secret" {
		t.Fatal("the promoted credential lost its envelope")
	}
	if _, err := repo.RollbackCredential(ctx, org.ID, c.ID, 2, "it", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	restored, _ := repo.Credential(ctx, org.ID, c.ID, 1)
	openCredential(t, secretbox.NewEnvelope(newOnly), c, restored)

	// Deleting a connection erases its staged and previous credentials too.
	gone := activeConnection(t, repo, org.ID, "to delete", oldKey)
	if _, err := repo.StageCredential(ctx, org.ID, gone.ID, interfaces.SealedCredential{Version: 2, KeyVersion: staged.KeyVersion,
		Nonce: staged.Nonce, Ciphertext: staged.Ciphertext, WrappedKey: staged.WrappedKey}, "it", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Disable(ctx, org.ID, gone.ID, "it", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Delete(ctx, org.ID, gone.ID, "it", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM storage_connections WHERE id = $1 AND
		(credential_ciphertext IS NOT NULL OR staged_ciphertext IS NOT NULL OR previous_ciphertext IS NOT NULL)`, gone.ID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("a deleted connection kept %d credentials: %v", left, err)
	}
}
