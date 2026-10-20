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
