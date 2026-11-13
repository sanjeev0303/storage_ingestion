// Package secretbox seals small secrets (customer storage credentials)
// with AES-256-GCM under a versioned keyring. A sealed value is bound to
// its context through the associated data: opened under another tenant,
// connection, provider or credential version, it fails to open.
//
// Envelope adds a layer: each value gets its own random data key, and only
// that data key is sealed by the keyring (or a KMS behind Sealer).
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
	// WrappedKey is the value's own data key, sealed by the master key
	// KeyVersion names (Envelope). Empty means the value was sealed by the
	// master key directly, as every value was before envelopes.
	WrappedKey []byte
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
		aead, err := newGCM(key)
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

// Envelope seals each value under a fresh random data key and seals only
// that key with the master sealer. A master-key rotation then re-wraps the
// small data keys (Rewrap) without opening the secrets themselves, and a
// KMS can stand in for the keyring as the master. Values sealed directly
// by the master key still open, so older rows keep working.
type Envelope struct {
	master Sealer
}

var _ Sealer = (*Envelope)(nil)

// NewEnvelope seals under master: the keyring, or anything else that
// implements Sealer.
func NewEnvelope(master Sealer) *Envelope { return &Envelope{master: master} }

func (e *Envelope) Seal(plaintext, associatedData []byte) (Sealed, error) {
	dataKey := make([]byte, KeySize)
	if _, err := rand.Read(dataKey); err != nil {
		return Sealed{}, err
	}
	defer Wipe(dataKey)
	nonce, ciphertext, err := sealWith(dataKey, plaintext, associatedData)
	if err != nil {
		return Sealed{}, err
	}
	wrapped, err := e.master.Seal(dataKey, wrapContext(associatedData))
	if err != nil {
		return Sealed{}, err
	}
	return Sealed{KeyVersion: wrapped.KeyVersion, Nonce: nonce, Ciphertext: ciphertext, WrappedKey: joinWrapped(wrapped)}, nil
}

func (e *Envelope) Open(sealed Sealed, associatedData []byte) ([]byte, error) {
	if len(sealed.WrappedKey) == 0 {
		return e.master.Open(sealed, associatedData)
	}
	dataKey, err := e.unwrap(sealed, associatedData)
	if err != nil {
		return nil, err
	}
	defer Wipe(dataKey)
	return openWith(dataKey, sealed.Nonce, sealed.Ciphertext, associatedData)
}

// Rewrap seals the value's data key again with the master's current key
// and leaves the sealed secret as it is. A value sealed directly by the
// master key is moved into an envelope.
func (e *Envelope) Rewrap(sealed Sealed, associatedData []byte) (Sealed, error) {
	if len(sealed.WrappedKey) == 0 {
		plaintext, err := e.master.Open(sealed, associatedData)
		if err != nil {
			return Sealed{}, err
		}
		defer Wipe(plaintext)
		return e.Seal(plaintext, associatedData)
	}
	dataKey, err := e.unwrap(sealed, associatedData)
	if err != nil {
		return Sealed{}, err
	}
	defer Wipe(dataKey)
	wrapped, err := e.master.Seal(dataKey, wrapContext(associatedData))
	if err != nil {
		return Sealed{}, err
	}
	sealed.KeyVersion, sealed.WrappedKey = wrapped.KeyVersion, joinWrapped(wrapped)
	return sealed, nil
}

func (e *Envelope) unwrap(sealed Sealed, associatedData []byte) ([]byte, error) {
	w := sealed.WrappedKey
	if len(w) < 1 || len(w) < 1+int(w[0]) {
		return nil, ErrOpen
	}
	nonceSize := int(w[0])
	dataKey, err := e.master.Open(Sealed{KeyVersion: sealed.KeyVersion, Nonce: w[1 : 1+nonceSize], Ciphertext: w[1+nonceSize:]},
		wrapContext(associatedData))
	if err != nil {
		return nil, err
	}
	if len(dataKey) != KeySize {
		Wipe(dataKey)
		return nil, ErrOpen
	}
	return dataKey, nil
}

// wrapContext is the associated data a data key is sealed under: the
// value's own, set apart so a wrapped key can never pass for a value
// sealed directly by the master key, or the other way round.
func wrapContext(associatedData []byte) []byte {
	return append([]byte("data-key|"), associatedData...)
}

// joinWrapped stores a wrapped key as one byte of nonce length, the nonce
// and the ciphertext. A KMS without a separate nonce stores length 0.
func joinWrapped(w Sealed) []byte {
	out := make([]byte, 0, 1+len(w.Nonce)+len(w.Ciphertext))
	out = append(out, byte(len(w.Nonce)))
	out = append(out, w.Nonce...)
	return append(out, w.Ciphertext...)
}

func sealWith(key, plaintext, associatedData []byte) ([]byte, []byte, error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return nonce, aead.Seal(nil, nonce, plaintext, associatedData), nil
}

func openWith(key, nonce, ciphertext, associatedData []byte) ([]byte, error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != aead.NonceSize() {
		return nil, ErrOpen
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, associatedData)
	if err != nil {
		return nil, ErrOpen
	}
	return plaintext, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
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
