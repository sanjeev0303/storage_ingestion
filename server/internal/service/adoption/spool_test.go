package adoption

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"os"
	"strings"
	"testing"
)

func TestSpoolCheckedKeepsTheFileOnDiskAndRefusesBadOnes(t *testing.T) {
	data := []byte("inventory rows")
	sum := md5.Sum(data)
	r := files{"ok": data}

	spool, err := spoolChecked(context.Background(), r, "ok", hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(spool)
	if string(got) != string(data) {
		t.Fatalf("spooled %q, want %q", got, data)
	}
	name := spool.Name()
	discardSpool(spool)
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Fatalf("the spool file was left behind: %v", err)
	}

	if _, err := spoolChecked(context.Background(), r, "ok", strings.Repeat("0", 32)); err == nil {
		t.Fatal("a wrong MD5 was accepted")
	}

	// Over the cap is refused, not truncated and parsed as if whole.
	old := maxInventoryFile
	maxInventoryFile = int64(len(data) - 1)
	defer func() { maxInventoryFile = old }()
	if _, err := spoolChecked(context.Background(), r, "ok", ""); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("an oversized file was accepted: %v", err)
	}
}
