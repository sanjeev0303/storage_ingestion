package main

import (
	"bytes"
	"context"
	"io"
	"slices"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/secretbox"
)

// memoryStore keeps rows by ID and answers ResealBatch like the
// repository: rows after the cursor that still need work, in ID order.
type memoryStore struct {
	rows    map[string]*postgres.SealedCredentials
	batches int
}

func (m *memoryStore) needsWork(s *postgres.SealedCredentials, keyVersion int) bool {
	for _, c := range []interfaces.SealedCredential{s.Current, s.Staged, s.Previous} {
		if c.Version > 0 && (len(c.WrappedKey) == 0 || c.KeyVersion != keyVersion) {
			return true
		}
	}
	return false
}

func (m *memoryStore) ResealBatch(_ context.Context, keyVersion int, after string, limit int, write bool,
	reseal func(*postgres.SealedCredentials) error) (string, int, error) {
	m.batches++
	ids := make([]string, 0, len(m.rows))
	for id := range m.rows {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	last, resealed := "", 0
	for _, id := range ids {
		if id <= after || !m.needsWork(m.rows[id], keyVersion) {
			continue
		}
		if limit == 0 {
			break
		}
		limit--
		last = id
		copied := *m.rows[id]
		if err := reseal(&copied); err != nil {
			continue
		}
		if write {
			m.rows[id] = &copied
		}
		resealed++
	}
	return last, resealed, nil
}

func (m *memoryStore) EraseDeletedCredentials(context.Context) (int64, error) { return 0, nil }

func sealedFor(t *testing.T, sealer secretbox.Sealer, c model.StorageConnection, version int, secret string) interfaces.SealedCredential {
	t.Helper()
	s, err := sealer.Seal([]byte(secret), c.CredentialAAD(version))
	if err != nil {
		t.Fatal(err)
	}
	return interfaces.SealedCredential{Version: version, KeyVersion: s.KeyVersion, Nonce: s.Nonce, Ciphertext: s.Ciphertext, WrappedKey: s.WrappedKey}
}

func TestResealMovesEveryCredentialToTheNewestKey(t *testing.T) {
	key := func(b byte) []byte { return bytes.Repeat([]byte{b}, secretbox.KeySize) }
	old, _ := secretbox.NewKeyring(map[int][]byte{1: key(1)}, 1)
	store := &memoryStore{rows: map[string]*postgres.SealedCredentials{}}
	for i, id := range []string{"sc_a", "sc_b", "sc_c", "sc_d", "sc_e"} {
		c := model.StorageConnection{ID: id, TenantID: "t1", Provider: model.ProviderAWSS3}
		row := &postgres.SealedCredentials{Connection: c}
		switch i % 3 {
		case 0: // sealed before envelopes
			row.Current = sealedFor(t, old, c, 1, "direct-"+id)
		case 1: // an envelope under the old key, with a staged credential
			row.Current = sealedFor(t, secretbox.NewEnvelope(old), c, 2, "enveloped-"+id)
			row.Staged = sealedFor(t, secretbox.NewEnvelope(old), c, 3, "staged-"+id)
		case 2: // a previous credential kept for a rollback
			row.Current = sealedFor(t, secretbox.NewEnvelope(old), c, 4, "current-"+id)
			row.Previous = sealedFor(t, old, c, 3, "previous-"+id)
		}
		store.rows[id] = row
	}
	// One credential under a key that is no longer configured.
	gone, _ := secretbox.NewKeyring(map[int][]byte{9: key(9)}, 9)
	lost := model.StorageConnection{ID: "sc_lost", TenantID: "t1", Provider: model.ProviderAWSS3}
	store.rows["sc_lost"] = &postgres.SealedCredentials{Connection: lost, Current: sealedFor(t, gone, lost, 1, "lost")}

	ring, _ := secretbox.NewKeyring(map[int][]byte{1: key(1), 2: key(2)}, 2)
	envelope := secretbox.NewEnvelope(ring)
	ctx := context.Background()

	// A dry run checks and counts, and changes nothing.
	report, err := reseal(ctx, store, envelope, 2, "", 2, false, io.Discard)
	if err != nil || report.Resealed != 5 || !slices.Equal(report.Failed, []string{"sc_lost"}) || !report.DryRun {
		t.Fatalf("dry run: %+v %v", report, err)
	}
	if store.rows["sc_a"].Current.KeyVersion != 1 {
		t.Fatal("the dry run wrote")
	}

	// Resumed after the first connection, then run again from the start:
	// together they cover everything, and the second run finds only the
	// lost one left.
	if _, err := reseal(ctx, store, envelope, 2, "sc_a", 2, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	if store.rows["sc_a"].Current.KeyVersion != 1 || store.rows["sc_b"].Current.KeyVersion != 2 {
		t.Fatal("a resumed run starts after its cursor")
	}
	report, err = reseal(ctx, store, envelope, 2, "", 2, true, io.Discard)
	if err != nil || report.Resealed != 1 || !slices.Equal(report.Failed, []string{"sc_lost"}) {
		t.Fatalf("the run from the start reseals only what the resumed one skipped: %+v %v", report, err)
	}

	// Every credential is now an envelope under key 2 and opens without key 1.
	newOnly, _ := secretbox.NewKeyring(map[int][]byte{2: key(2)}, 2)
	for id, row := range store.rows {
		if id == "sc_lost" {
			continue
		}
		for _, c := range []interfaces.SealedCredential{row.Current, row.Staged, row.Previous} {
			if c.Version == 0 {
				continue
			}
			if c.KeyVersion != 2 || len(c.WrappedKey) == 0 {
				t.Fatalf("%s v%d is not under key 2: %d", id, c.Version, c.KeyVersion)
			}
			sealed := secretbox.Sealed{KeyVersion: c.KeyVersion, Nonce: c.Nonce, Ciphertext: c.Ciphertext, WrappedKey: c.WrappedKey}
			if _, err := secretbox.NewEnvelope(newOnly).Open(sealed, row.Connection.CredentialAAD(c.Version)); err != nil {
				t.Fatalf("%s v%d does not open with key 2 alone: %v", id, c.Version, err)
			}
		}
	}
}
