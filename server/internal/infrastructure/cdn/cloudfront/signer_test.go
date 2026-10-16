package cloudfront

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSignedURLVerifiesWithThePublicKey(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	signer, err := NewSigner("K2JCJMDEHXQW5F", keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	raw := "https://cdn.example.test/assets/tenant-1/a1"
	expires := time.Unix(1_800_000_000, 0)
	signed, err := signer.SignURL(raw, expires)
	if err != nil {
		t.Fatal(err)
	}

	parsed, _ := url.Parse(signed)
	q := parsed.Query()
	if q.Get("Expires") != "1800000000" || q.Get("Key-Pair-Id") != "K2JCJMDEHXQW5F" {
		t.Fatalf("unexpected parameters in %s", signed)
	}

	// Undo CloudFront's URL-safe base64 and verify the canned policy.
	sig := strings.NewReplacer("-", "+", "_", "=", "~", "/").Replace(q.Get("Signature"))
	decoded, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		t.Fatal(err)
	}
	policy := fmt.Sprintf(`{"Statement":[{"Resource":"%s","Condition":{"DateLessThan":{"AWS:EpochTime":%d}}}]}`, raw, expires.Unix())
	digest := sha1.Sum([]byte(policy))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA1, digest[:], decoded); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}

	// The signature covers the expiry: changing it breaks verification.
	tampered := fmt.Sprintf(`{"Statement":[{"Resource":"%s","Condition":{"DateLessThan":{"AWS:EpochTime":%d}}}]}`, raw, expires.Unix()+3600)
	tamperedDigest := sha1.Sum([]byte(tampered))
	if rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA1, tamperedDigest[:], decoded) == nil {
		t.Fatal("a longer expiry must not verify")
	}
}

func TestSignerRefusesBadInput(t *testing.T) {
	if _, err := NewSigner("", []byte("x")); err == nil {
		t.Fatal("a key pair ID is required")
	}
	if _, err := NewSigner("K", []byte("not pem")); err == nil {
		t.Fatal("non-PEM keys must be refused")
	}

	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(key)
	signer, err := NewSigner("K", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}))
	if err != nil {
		t.Fatalf("PKCS#8 keys must be accepted: %v", err)
	}
	if _, err := signer.SignURL("http://cdn.example.test/x", time.Now()); err == nil {
		t.Fatal("plain http CDN URLs must be refused")
	}
}
