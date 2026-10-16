// Package cloudfront signs CloudFront URLs with a canned policy, so the
// distribution serves an object only to holders of an unexpired signature.
package cloudfront

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
)

// Signer holds a CloudFront key pair's private key. The key never leaves
// this type and is never logged.
type Signer struct {
	keyPairID string
	key       *rsa.PrivateKey
}

var _ interfaces.URLSigner = (*Signer)(nil)

// NewSigner parses a PEM private key (PKCS#1 or PKCS#8 RSA).
func NewSigner(keyPairID string, privateKeyPEM []byte) (*Signer, error) {
	if keyPairID == "" {
		return nil, errors.New("CloudFront key pair ID is required")
	}
	block, _ := pem.Decode(privateKeyPEM)
	if block == nil {
		return nil, errors.New("CloudFront private key is not PEM")
	}

	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return &Signer{keyPairID: keyPairID, key: key}, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("CloudFront private key could not be parsed")
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("CloudFront private key must be RSA")
	}
	return &Signer{keyPairID: keyPairID, key: key}, nil
}

// SignURL adds Expires, Signature and Key-Pair-Id. The canned policy binds
// the signature to this exact URL (path and query) and expiry.
func (s *Signer) SignURL(rawURL string, expires time.Time) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return "", errors.New("CDN URL must be an absolute https URL")
	}

	epoch := expires.Unix()
	policy := fmt.Sprintf(`{"Statement":[{"Resource":"%s","Condition":{"DateLessThan":{"AWS:EpochTime":%d}}}]}`, rawURL, epoch)

	digest := sha1.Sum([]byte(policy))
	signature, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA1, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign CDN URL: %w", err)
	}

	separator := "?"
	if parsed.RawQuery != "" {
		separator = "&"
	}
	return fmt.Sprintf("%s%sExpires=%d&Signature=%s&Key-Pair-Id=%s",
		rawURL, separator, epoch, urlSafe(signature), url.QueryEscape(s.keyPairID)), nil
}

// urlSafe is CloudFront's base64 variant: + -> -, = -> _, / -> ~.
func urlSafe(signature []byte) string {
	encoded := base64.StdEncoding.EncodeToString(signature)
	return strings.NewReplacer("+", "-", "=", "_", "/", "~").Replace(encoded)
}
