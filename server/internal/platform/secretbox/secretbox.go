// Package secretbox seals small secrets (customer storage credentials)
// with AES-256-GCM under a versioned keyring. A sealed value is bound to
// its context through the associated data: opened under another tenant,
// connection, provider or credential version, it fails to open.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// KeySize is the key length AES-256 needs.
const KeySize = 32

var (
	ErrUnknownKey = errors.New("the sealing key version is not configured")
	ErrOpen       = errors.New("the sealed value could not be opened")
)

// Sealed is what is stored: never the plaintext.
type Sealed struct {
	KeyVersion int
	Nonce      []byte
	Ciphertext []byte
}

// Sealer seals and opens secrets. The keyring below implements it with
// local keys; a KMS envelope implementation can take its place.
type Sealer interface {
	Seal(plaintext, associatedData []byte) (Sealed, error)
	Open(sealed Sealed, associatedData []byte) ([]byte, error)
}

// Keyring seals with its current key and opens with any of its keys, so
// keys can be rotated while older values stay readable.
type Keyring struct {
	current int
	aeads   map[int]cipher.AEAD
}

var _ Sealer = (*Keyring)(nil)

// NewKeyring builds a keyring from keys by version. Every key must be
// exactly KeySize bytes, not all zero, and different from the others.
func NewKeyring(keys map[int][]byte, current int) (*Keyring, error) {
	if _, ok := keys[current]; !ok {
		return nil, fmt.Errorf("current key version %d is not among the keys", current)
	}
	k := &Keyring{current: current, aeads: map[int]cipher.AEAD{}}
	seen := map[string]int{}
	for version, key := range keys {
		if version <= 0 {
			return nil, errors.New("key versions must be positive")
		}
		if len(key) != KeySize {
			return nil, fmt.Errorf("key version %d is %d bytes, not %d", version, len(key), KeySize)
		}
		if allZero(key) {
			return nil, fmt.Errorf("key version %d is all zeros", version)
		}
		if other, dup := seen[string(key)]; dup {
			return nil, fmt.Errorf("key versions %d and %d are the same key", other, version)
		}
		seen[string(key)] = version
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		k.aeads[version] = aead
	}
	return k, nil
}

// ParseKeys reads "1:<base64>,2:<base64>" (standard or URL base64).
func ParseKeys(spec string) (map[int][]byte, error) {
	keys := map[int][]byte{}
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		versionText, encoded, ok := strings.Cut(item, ":")
		version, err := strconv.Atoi(versionText)
		if !ok || err != nil {
			return nil, errors.New("each key is <version>:<base64 of 32 bytes>")
		}
		if _, dup := keys[version]; dup {
			return nil, fmt.Errorf("key version %d appears twice", version)
		}
		key, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			key, err = base64.URLEncoding.DecodeString(encoded)
		}
		if err != nil {
			return nil, fmt.Errorf("key version %d is not valid base64", version)
		}
		keys[version] = key
	}
	if len(keys) == 0 {
		return nil, errors.New("no keys")
	}
	return keys, nil
}

// CurrentVersion is the key new values are sealed with.
func (k *Keyring) CurrentVersion() int { return k.current }

func (k *Keyring) Seal(plaintext, associatedData []byte) (Sealed, error) {
	aead := k.aeads[k.current]
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return Sealed{}, err
	}
	return Sealed{KeyVersion: k.current, Nonce: nonce, Ciphertext: aead.Seal(nil, nonce, plaintext, associatedData)}, nil
}

func (k *Keyring) Open(sealed Sealed, associatedData []byte) ([]byte, error) {
	aead, ok := k.aeads[sealed.KeyVersion]
	if !ok {
		return nil, ErrUnknownKey
	}
	if len(sealed.Nonce) != aead.NonceSize() {
		return nil, ErrOpen
	}
	plaintext, err := aead.Open(nil, sealed.Nonce, sealed.Ciphertext, associatedData)
	if err != nil {
		// No detail: a wrong key, a wrong context and tampering look alike.
		return nil, ErrOpen
	}
	return plaintext, nil
}

// Wipe overwrites a buffer that held a secret.
func Wipe(b []byte) { clear(b) }

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
