package agent

import (
	"syscall"
	"testing"
)

// lockFile opens file with no sharing, as an instrument writing it might:
// nothing else can open it until the test ends.
func lockFile(t *testing.T, file string) {
	t.Helper()
	name, err := syscall.UTF16PtrFromString(file)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.CloseHandle(h) })
}
