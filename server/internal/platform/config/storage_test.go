package config

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func testKey(b byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)) }

func TestLoadStorageWithoutBYOSNeedsNoKey(t *testing.T) {
	t.Setenv("BYOS_ENABLED", "false")
	s, err := LoadStorage()
	if err != nil || s.BYOSEnabled || s.CredentialKeys != nil || s.ClientCacheSize != 256 {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestLoadStorageKeys(t *testing.T) {
	t.Setenv("BYOS_ENABLED", "true")
	t.Setenv("STORAGE_CREDENTIAL_KEYS", "1:"+testKey(1)+",2:"+testKey(2))
	s, err := LoadStorage()
	if err != nil || s.CredentialKeyVersion != 2 || len(s.CredentialKeys) != 2 {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err := s.Keyring(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STORAGE_CREDENTIAL_KEY_VERSION", "1")
	if s, err := LoadStorage(); err != nil || s.CredentialKeyVersion != 1 {
		t.Fatalf("pinned version: %+v %v", s, err)
	}
}

func TestLoadStorageRefusesBadKeys(t *testing.T) {
	cases := map[string]map[string]string{
		"missing":   {"STORAGE_CREDENTIAL_KEYS": ""},
		"short":     {"STORAGE_CREDENTIAL_KEYS": "1:" + base64.StdEncoding.EncodeToString([]byte("too short"))},
		"malformed": {"STORAGE_CREDENTIAL_KEYS": "one:abc"},
		"no such current version": {
			"STORAGE_CREDENTIAL_KEYS": "1:" + testKey(1), "STORAGE_CREDENTIAL_KEY_VERSION": "3",
		},
		"reuses the database password": {
			"STORAGE_CREDENTIAL_KEYS": "1:" + testKey(7), "POSTGRES_PASSWORD": string(bytes.Repeat([]byte{7}, 32)),
		},
		"private endpoints outside development": {
			"STORAGE_CREDENTIAL_KEYS": "1:" + testKey(1), "STORAGE_ALLOW_PRIVATE_ENDPOINTS": "true", "APP_ENV": "production",
		},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("BYOS_ENABLED", "true")
			for k, v := range env {
				t.Setenv(k, v)
			}
			if _, err := LoadStorage(); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
