package secretbox

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, KeySize) }

func TestSealAndOpenAreBoundToTheirContext(t *testing.T) {
	ring, err := NewKeyring(map[int][]byte{1: key(1)}, 1)
	if err != nil {
		t.Fatal(err)
	}
	ad := []byte("tenant-a/conn-1/AWS_S3/1")
	sealed, err := ring.Seal([]byte("secret"), ad)
	if err != nil || sealed.KeyVersion != 1 || bytes.Contains(sealed.Ciphertext, []byte("secret")) {
		t.Fatalf("sealed: %+v %v", sealed, err)
	}
	if plain, err := ring.Open(sealed, ad); err != nil || string(plain) != "secret" {
		t.Fatalf("open: %q %v", plain, err)
	}
	// Another tenant, connection or version: the same bytes do not open.
	for _, other := range []string{"tenant-b/conn-1/AWS_S3/1", "tenant-a/conn-2/AWS_S3/1", "tenant-a/conn-1/AWS_S3/2"} {
		if _, err := ring.Open(sealed, []byte(other)); !errors.Is(err, ErrOpen) {
			t.Fatalf("opened under %s: %v", other, err)
		}
	}
	tampered := sealed
	tampered.Ciphertext = append([]byte{}, sealed.Ciphertext...)
	tampered.Ciphertext[0] ^= 1
	if _, err := ring.Open(tampered, ad); !errors.Is(err, ErrOpen) {
		t.Fatalf("tampered: %v", err)
	}
	// Two seals of the same value differ (fresh nonces).
	if again, _ := ring.Seal([]byte("secret"), ad); bytes.Equal(again.Ciphertext, sealed.Ciphertext) {
		t.Fatal("nonce reuse")
	}
}

func TestKeyRotationKeepsOldValuesReadable(t *testing.T) {
	old, _ := NewKeyring(map[int][]byte{1: key(1)}, 1)
	sealed, _ := old.Seal([]byte("v1 secret"), []byte("ad"))
	ring, err := NewKeyring(map[int][]byte{1: key(1), 2: key(2)}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if plain, err := ring.Open(sealed, []byte("ad")); err != nil || string(plain) != "v1 secret" {
		t.Fatalf("old value: %q %v", plain, err)
	}
	if fresh, _ := ring.Seal([]byte("x"), nil); fresh.KeyVersion != 2 {
		t.Fatalf("new values use the current key: %d", fresh.KeyVersion)
	}
	if _, err := old.Open(Sealed{KeyVersion: 2, Nonce: make([]byte, 12)}, nil); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown version: %v", err)
	}
}

func TestBadKeysAreRefused(t *testing.T) {
	bad := []struct {
		keys    map[int][]byte
		current int
	}{
		{map[int][]byte{1: make([]byte, 16)}, 1},      // too short
		{map[int][]byte{1: make([]byte, KeySize)}, 1}, // all zeros
		{map[int][]byte{1: key(3), 2: key(3)}, 1},     // the same key twice
		{map[int][]byte{0: key(4)}, 0},                // version 0
		{map[int][]byte{1: key(1)}, 2},                // no current key
	}
	for i, c := range bad {
		if _, err := NewKeyring(c.keys, c.current); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	one, two := base64.StdEncoding.EncodeToString(key(1)), base64.URLEncoding.EncodeToString(key(2))
	for _, spec := range []string{"", "1", "x:abc", "1:not base64!", "1:" + one + ",1:" + one} {
		if _, err := ParseKeys(spec); err == nil {
			t.Errorf("%q parsed", spec)
		}
	}
	keys, err := ParseKeys(" 1:" + one + ", 2:" + two)
	if err != nil || len(keys) != 2 || !bytes.Equal(keys[2], key(2)) {
		t.Fatalf("parse: %v %v", keys, err)
	}
}

func TestEnvelopeSealsEachValueUnderItsOwnDataKey(t *testing.T) {
	ring, _ := NewKeyring(map[int][]byte{1: key(1)}, 1)
	env := NewEnvelope(ring)
	ad := []byte("storage-credential/v1|tenant-a|conn-1|AWS_S3|1")
	sealed, err := env.Seal([]byte("secret"), ad)
	if err != nil || sealed.KeyVersion != 1 || len(sealed.WrappedKey) == 0 || bytes.Contains(sealed.Ciphertext, []byte("secret")) {
		t.Fatalf("sealed: %+v %v", sealed, err)
	}
	if plain, err := env.Open(sealed, ad); err != nil || string(plain) != "secret" {
		t.Fatalf("open: %q %v", plain, err)
	}
	// The secret is not sealed by the master key itself.
	if _, err := ring.Open(sealed, ad); !errors.Is(err, ErrOpen) {
		t.Fatalf("the master key opened the value directly: %v", err)
	}
	// Bound to its connection and version, through both layers.
	for _, other := range []string{"storage-credential/v1|tenant-a|conn-2|AWS_S3|1", "storage-credential/v1|tenant-a|conn-1|AWS_S3|2"} {
		if _, err := env.Open(sealed, []byte(other)); !errors.Is(err, ErrOpen) {
			t.Fatalf("opened under %s: %v", other, err)
		}
	}
	// Another value's wrapped key does not open this one.
	again, _ := env.Seal([]byte("secret"), ad)
	if bytes.Equal(again.WrappedKey, sealed.WrappedKey) {
		t.Fatal("two values share a data key")
	}
	swapped := sealed
	swapped.WrappedKey = again.WrappedKey
	if _, err := env.Open(swapped, ad); !errors.Is(err, ErrOpen) {
		t.Fatalf("opened with another value's data key: %v", err)
	}
	// A wrapped data key cannot pass for a value sealed directly.
	asDirect := Sealed{KeyVersion: 1, Nonce: sealed.WrappedKey[1:13], Ciphertext: sealed.WrappedKey[13:]}
	if _, err := env.Open(asDirect, ad); !errors.Is(err, ErrOpen) {
		t.Fatalf("a wrapped key opened as a value: %v", err)
	}
	for _, bad := range [][]byte{{200}, {12, 1, 2}} {
		broken := sealed
		broken.WrappedKey = bad
		if _, err := env.Open(broken, ad); !errors.Is(err, ErrOpen) {
			t.Fatalf("malformed wrapped key %v: %v", bad, err)
		}
	}
}

func TestEnvelopeOpensValuesSealedBeforeIt(t *testing.T) {
	ring, _ := NewKeyring(map[int][]byte{1: key(1)}, 1)
	direct, _ := ring.Seal([]byte("old secret"), []byte("ad"))
	if plain, err := NewEnvelope(ring).Open(direct, []byte("ad")); err != nil || string(plain) != "old secret" {
		t.Fatalf("direct value: %q %v", plain, err)
	}
}

func TestRewrapMovesValuesToTheNewestKeyWithoutChangingThem(t *testing.T) {
	old, _ := NewKeyring(map[int][]byte{1: key(1)}, 1)
	ad := []byte("ad")
	enveloped, _ := NewEnvelope(old).Seal([]byte("enveloped"), ad)
	direct, _ := old.Seal([]byte("direct"), ad)

	ring, _ := NewKeyring(map[int][]byte{1: key(1), 2: key(2)}, 2)
	env := NewEnvelope(ring)
	moved, err := env.Rewrap(enveloped, ad)
	if err != nil || moved.KeyVersion != 2 {
		t.Fatalf("rewrap: %+v %v", moved, err)
	}
	// Only the data key's wrapping changed; the sealed secret is the same.
	if !bytes.Equal(moved.Ciphertext, enveloped.Ciphertext) || !bytes.Equal(moved.Nonce, enveloped.Nonce) {
		t.Fatal("rewrap re-encrypted the secret")
	}
	upgraded, err := env.Rewrap(direct, ad)
	if err != nil || upgraded.KeyVersion != 2 || len(upgraded.WrappedKey) == 0 {
		t.Fatalf("upgrade: %+v %v", upgraded, err)
	}
	newOnly, _ := NewKeyring(map[int][]byte{2: key(2)}, 2)
	for want, s := range map[string]Sealed{"enveloped": moved, "direct": upgraded} {
		if plain, err := NewEnvelope(newOnly).Open(s, ad); err != nil || string(plain) != want {
			t.Fatalf("%s after rewrap, without the old key: %q %v", want, plain, err)
		}
	}
	// Until the rewrap is done, old values still open with both keys loaded.
	if plain, err := env.Open(enveloped, ad); err != nil || string(plain) != "enveloped" {
		t.Fatalf("old value during the move: %q %v", plain, err)
	}
	if _, err := env.Rewrap(enveloped, []byte("other")); !errors.Is(err, ErrOpen) {
		t.Fatalf("rewrapped under the wrong context: %v", err)
	}
}
