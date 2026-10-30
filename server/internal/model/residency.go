package model

import (
	"regexp"
	"strings"
)

// Posture checks for residency and keys (docs/residency.md).
const (
	CheckRegion      = "region"
	CheckCustomerKey = "customer_key"
)

var locationPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}\*?$`)

// NormalizeLocations lowercases and checks a dataset's allowed locations:
// provider region or location codes ("ap-south-1", "asia-south1",
// "eu"), each optionally ending in * to allow every location it starts
// ("eu-*"). It reports false on a malformed entry.
func NormalizeLocations(in []string) ([]string, bool) {
	out := make([]string, 0, len(in))
	for _, l := range in {
		l = strings.ToLower(strings.TrimSpace(l))
		if l == "" {
			continue
		}
		if !locationPattern.MatchString(l) {
			return nil, false
		}
		out = append(out, l)
	}
	return out, len(out) <= 20
}

// LocationAllowed reports whether location is one the list allows. An
// empty list allows anywhere; an unknown location is never allowed by a
// non-empty list.
func LocationAllowed(location string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	location = strings.ToLower(location)
	if location == "" {
		return false
	}
	for _, a := range allowed {
		if prefix, wildcard := strings.CutSuffix(a, "*"); wildcard {
			if strings.HasPrefix(location, prefix) {
				return true
			}
		} else if location == a {
			return true
		}
	}
	return false
}

var (
	awsKey   = regexp.MustCompile(`^(arn:aws[a-z-]*:kms:[a-z0-9-]+:[0-9]{12}:(key|alias)/[A-Za-z0-9/_+=,.@-]+|alias/[A-Za-z0-9/_-]+|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)
	gcsKey   = regexp.MustCompile(`^projects/[a-z0-9-]+/locations/[a-z0-9-]+/keyRings/[A-Za-z0-9_-]+/cryptoKeys/[A-Za-z0-9_-]+$`)
	azureKey = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
)

// ValidEncryptionKey checks a connection's customer-managed key for its
// provider: a KMS key ID, ARN or alias on AWS and S3-compatible stores, a
// Cloud KMS key name on GCS, an encryption scope on Azure.
func ValidEncryptionKey(provider StorageProviderType, key string) bool {
	switch provider {
	case ProviderAWSS3, ProviderS3Compatible:
		return awsKey.MatchString(key)
	case ProviderGCS:
		return gcsKey.MatchString(key)
	case ProviderAzureBlob:
		return azureKey.MatchString(key)
	}
	return false
}
