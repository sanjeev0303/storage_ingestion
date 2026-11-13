package request

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestStorageCredentialsNeverPrint(t *testing.T) {
	body := RotateStorageCredentialsRequest{Credentials: StorageCredentials{AccessKeyID: "AKIDPRINTED", SecretAccessKey: "printed-secret"}}
	var logs bytes.Buffer
	slog.New(slog.NewTextHandler(&logs, nil)).Info("request", "body", body, "credentials", body.Credentials)
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("request", "credentials", body.Credentials)
	for _, out := range []string{fmt.Sprintf("%v %+v %#v %s", body, body, body, body.Credentials), logs.String()} {
		if strings.Contains(out, "printed-secret") || strings.Contains(out, "AKIDPRINTED") {
			t.Fatalf("a credential printed: %s", out)
		}
	}
}
