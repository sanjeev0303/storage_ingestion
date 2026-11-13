package azure

import (
	"encoding/base64"
	"testing"
)

// A block ID whose number does not fit int32 is not one of ours: it is
// ignored, not wrapped round to a part number that is.
func TestPartOfRefusesANumberPastInt32(t *testing.T) {
	id := newUploadID()
	huge := base64.StdEncoding.EncodeToString([]byte(id + "-4294967297")) // 2^32 + 1 wraps to 1
	if n, ok := partOf(id, huge); ok {
		t.Fatalf("an out-of-range block number was read as part %d", n)
	}
	if n, ok := partOf(id, blockID(id, 10000)); !ok || n != 10000 {
		t.Fatalf("part 10000 decoded as %d %v", n, ok)
	}
}
