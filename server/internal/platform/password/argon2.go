// Package password hashes passwords with Argon2id and checks them in
// constant time. Hashes are PHC strings, so their parameters travel with
// them and can be raised later without breaking existing accounts.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Params are Argon2id's cost parameters.
type Params struct {
	MemoryKiB uint32
	Time      uint32
	Threads   uint8
}

const (
	saltBytes = 16
	keyBytes  = 32
)

var b64 = base64.RawStdEncoding

// slots bounds concurrent Argon2id computations. Each holds MemoryKiB of
// memory for its duration, so a burst of sign-ins (spread over addresses,
// past the per-address throttle) would otherwise grow the heap without
// limit; this way it queues instead.
var slots = make(chan struct{}, runtime.NumCPU())

func idKey(password, salt []byte, p Params, n uint32) []byte {
	slots <- struct{}{}
	defer func() { <-slots }()
	return argon2.IDKey(password, salt, p.Time, p.MemoryKiB, p.Threads, n)
}

// Hash returns the PHC string of password under p.
func Hash(password string, p Params) (string, error) {
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key := idKey([]byte(password), salt, p, keyBytes)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.MemoryKiB, p.Time, p.Threads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// Verify reports whether password matches encoded. A malformed hash is an
// error, never a match.
func Verify(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("unsupported password hash")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errors.New("unsupported argon2 version")
	}
	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.MemoryKiB, &p.Time, &p.Threads); err != nil {
		return false, errors.New("malformed argon2 parameters")
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false, errors.New("malformed salt")
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false, errors.New("malformed key")
	}
	got := idKey([]byte(password), salt, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NeedsRehash reports whether encoded was made with weaker parameters
// than p, so it should be replaced at the next successful sign-in.
func NeedsRehash(encoded string, p Params) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 {
		return true
	}
	var old Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &old.MemoryKiB, &old.Time, &old.Threads); err != nil {
		return true
	}
	return old.MemoryKiB < p.MemoryKiB || old.Time < p.Time
}

// Generate returns a random password of n characters for a bootstrap
// account; it is shown once and never stored in clear.
func Generate(n int) (string, error) {
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789-_"
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i, b := range buf {
		// A byte at or above the largest multiple of the alphabet's length
		// is drawn again, so every character is equally likely.
		for int(b) >= 256-256%len(alphabet) {
			var one [1]byte
			if _, err := rand.Read(one[:]); err != nil {
				return "", err
			}
			b = one[0]
		}
		buf[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(buf), nil
}
