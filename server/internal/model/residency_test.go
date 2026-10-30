package model

import "testing"

func TestLocations(t *testing.T) {
	allowed, ok := NormalizeLocations([]string{" AP-South-1 ", "eu-*", ""})
	if !ok || len(allowed) != 2 || allowed[0] != "ap-south-1" {
		t.Fatalf("normalized: %v %v", allowed, ok)
	}
	if _, ok := NormalizeLocations([]string{"eu west"}); ok {
		t.Fatal("a malformed location was accepted")
	}
	for location, want := range map[string]bool{"ap-south-1": true, "eu-west-3": true, "us-east-1": false, "": false, "EU-CENTRAL-1": true} {
		if got := LocationAllowed(location, allowed); got != want {
			t.Errorf("%q: %v", location, got)
		}
	}
	if !LocationAllowed("", nil) {
		t.Error("no rule allows anywhere")
	}
}

func TestEncryptionKeys(t *testing.T) {
	for _, tc := range []struct {
		p    StorageProviderType
		key  string
		want bool
	}{
		{ProviderAWSS3, "arn:aws:kms:ap-south-1:758274585063:key/1234abcd-12ab-34cd-56ef-1234567890ab", true},
		{ProviderAWSS3, "alias/aws/s3", true},
		{ProviderAWSS3, "1234abcd-12ab-34cd-56ef-1234567890ab", true},
		{ProviderAWSS3, "not a key", false},
		{ProviderGCS, "projects/p1/locations/asia-south1/keyRings/r/cryptoKeys/k", true},
		{ProviderGCS, "alias/aws/s3", false},
		{ProviderAzureBlob, "phi-scope", true},
		{ProviderR2, "alias/aws/s3", false},
	} {
		if got := ValidEncryptionKey(tc.p, tc.key); got != tc.want {
			t.Errorf("%s %q: %v", tc.p, tc.key, got)
		}
	}
}
