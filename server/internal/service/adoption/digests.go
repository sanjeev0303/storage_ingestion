// Package adoption registers objects already in an organization's own
// bucket without uploading them again, and imports objects from URL lists
// and STAC catalogs (docs/adoption.md).
package adoption

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"hash/crc32"
	"math/big"
	"sort"
	"strings"

	"github.com/cespare/xxhash/v2"
	"github.com/zeebo/xxh3"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// alwaysComputed are worked out for every object read whole: SHA-256 is
// the platform's own, MD5 and xxh64 are what MHL and BagIt exports use.
var alwaysComputed = []string{"sha256", "md5", "xxh64"}

// digester computes several digests in one pass over the bytes.
type digester struct {
	sums   map[string]hash.Hash
	xxh3   *xxh3.Hasher // xxh3 and xxh128 share one state
	wants3 bool
	want   map[string]bool
}

// newDigester computes alwaysComputed and every algorithm in expected.
func newDigester(expected model.Digests) *digester {
	d := &digester{sums: map[string]hash.Hash{}, want: map[string]bool{}}
	for _, alg := range alwaysComputed {
		d.want[alg] = true
	}
	for alg := range expected {
		d.want[alg] = true
	}
	for alg := range d.want {
		switch alg {
		case "md5":
			d.sums[alg] = md5.New()
		case "sha1":
			d.sums[alg] = sha1.New()
		case "sha256":
			d.sums[alg] = sha256.New()
		case "sha512", "c4":
			if d.sums["sha512"] == nil {
				d.sums["sha512"] = sha512.New()
			}
		case "crc32c":
			d.sums[alg] = crc32.New(crc32.MakeTable(crc32.Castagnoli))
		case "xxh64":
			d.sums[alg] = xxhash.New()
		case "xxh3", "xxh128":
			d.wants3 = true
		}
	}
	if d.wants3 {
		d.xxh3 = xxh3.New()
	}
	return d
}

func (d *digester) Write(p []byte) (int, error) {
	for _, h := range d.sums {
		h.Write(p)
	}
	if d.xxh3 != nil {
		d.xxh3.Write(p)
	}
	return len(p), nil
}

// Digests are the computed digests, in the forms expected digests use.
func (d *digester) Digests() model.Digests {
	out := model.Digests{}
	for alg, h := range d.sums {
		out[alg] = hex.EncodeToString(h.Sum(nil))
	}
	if d.want["c4"] {
		out["c4"] = c4ID(d.sums["sha512"].Sum(nil))
		if !d.want["sha512"] {
			delete(out, "sha512")
		}
	}
	if d.xxh3 != nil {
		if d.want["xxh3"] {
			var b [8]byte
			binary.BigEndian.PutUint64(b[:], d.xxh3.Sum64())
			out["xxh3"] = hex.EncodeToString(b[:])
		}
		if d.want["xxh128"] {
			sum := d.xxh3.Sum128()
			var b [16]byte
			binary.BigEndian.PutUint64(b[:8], sum.Hi)
			binary.BigEndian.PutUint64(b[8:], sum.Lo)
			out["xxh128"] = hex.EncodeToString(b[:])
		}
	}
	return out
}

// mismatch names the expected digests the computed ones contradict.
func mismatch(expected, computed model.Digests) []string {
	var bad []string
	for alg, want := range expected {
		if computed[alg] != want {
			bad = append(bad, alg)
		}
	}
	sort.Strings(bad)
	return bad
}

const base58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// c4ID is the SMPTE ST 2114 C4 id of a SHA-512 digest: "c4" and the digest
// in base 58, padded with '1' to 90 characters.
func c4ID(sha512Sum []byte) string {
	n := new(big.Int).SetBytes(sha512Sum)
	radix, mod := big.NewInt(58), new(big.Int)
	var digits []byte
	for n.Sign() > 0 {
		n.DivMod(n, radix, mod)
		digits = append(digits, base58[mod.Int64()])
	}
	for i, j := 0, len(digits)-1; i < j; i, j = i+1, j-1 {
		digits[i], digits[j] = digits[j], digits[i]
	}
	return "c4" + strings.Repeat("1", 88-len(digits)) + string(digits)
}
