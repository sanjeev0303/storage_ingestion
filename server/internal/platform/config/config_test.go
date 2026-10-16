package config

import "testing"

func TestDeliveryDefaultsArePrivateAndBounded(t *testing.T) {
	t.Setenv("DELIVERY_CDN_ENABLED", "")
	cfg, err := LoadDelivery()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CDNEnabled || cfg.DefaultTTL.Minutes() != 5 || cfg.MaxTTL.Minutes() != 15 {
		t.Fatalf("unexpected defaults %+v", cfg)
	}
}

func TestCDNCannotBeEnabledWithoutSigning(t *testing.T) {
	t.Setenv("DELIVERY_CDN_ENABLED", "true")
	t.Setenv("DELIVERY_CDN_BASE_URL", "https://cdn.example.test")
	if _, err := LoadDelivery(); err == nil {
		t.Fatal("a CDN without a signing key must be refused")
	}
}

func TestDeliveryTTLMustNotBeShorterThanItsDefault(t *testing.T) {
	t.Setenv("DELIVERY_URL_TTL", "10m")
	t.Setenv("DELIVERY_URL_MAX_TTL", "5m")
	if _, err := LoadDelivery(); err == nil {
		t.Fatal("max below default must be refused")
	}
}
