package audit

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestScrubDropsCredentialsAtAnyDepth(t *testing.T) {
	details := map[string]any{
		"role":          "developer",
		"password":      "hunter2hunter2",
		"new_password":  "hunter3hunter3",
		"refresh_token": "srt_secret",
		"Authorization": "Bearer sat_secret",
		"download_url":  "https://storage/x?X-Amz-Signature=abc",
		"client_secret": "s3cr3t",
		"nested": map[string]any{
			"api_token": "tok",
			"size":      42,
			"deeper":    map[string]any{"Cookie": "session=1", "kept": true},
		},
		"items": []any{map[string]any{"credential": "c", "name": "a"}, "plain"},
	}
	scrubbed := Scrub(details)
	raw, err := json.Marshal(scrubbed)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"hunter", "srt_secret", "sat_secret", "Signature", "s3cr3t", "tok", "session=1", `"c"`} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("%q survived scrubbing: %s", secret, raw)
		}
	}
	for _, kept := range []string{`"role":"developer"`, `"size":42`, `"kept":true`, `"name":"a"`, `"plain"`} {
		if !strings.Contains(string(raw), kept) {
			t.Fatalf("%s was dropped: %s", kept, raw)
		}
	}
	if details["password"] != "hunter2hunter2" {
		t.Fatal("Scrub must not change its argument")
	}
	if Scrub(nil) != nil {
		t.Fatal("nil stays nil")
	}
}
