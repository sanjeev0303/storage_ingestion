//go:build !windows

package agent

import (
	"os"
	"testing"
)

// lockFile makes file unreadable until the test ends.
func lockFile(t *testing.T, file string) {
	t.Helper()
	if err := os.Chmod(file, 0); err != nil {
		t.Fatal(err)
	}
	if f, err := os.Open(file); err == nil {
		f.Close()
		t.Skip("running as a user who can read any file")
	}
	t.Cleanup(func() { _ = os.Chmod(file, 0o644) })
}
