package password

import (
	"strings"
	"testing"
)

var testParams = Params{MemoryKiB: 19 * 1024, Time: 2, Threads: 1}

func TestHashAndVerify(t *testing.T) {
	hash, err := Hash("correct horse battery staple", testParams)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$") || strings.Contains(hash, "correct") {
		t.Fatalf("hash %q", hash)
	}
	if ok, err := Verify("correct horse battery staple", hash); err != nil || !ok {
		t.Fatalf("the right password: %v %v", ok, err)
	}
	if ok, _ := Verify("Correct horse battery staple", hash); ok {
		t.Fatal("a wrong password matched")
	}
	again, _ := Hash("correct horse battery staple", testParams)
	if again == hash {
		t.Fatal("two hashes of one password must differ (salt)")
	}
	for _, bad := range []string{"", "$2a$10$abc", "$argon2id$v=19$m=x$salt$key", "$argon2i$v=19$m=19456,t=2,p=1$c2FsdA$a2V5"} {
		if ok, err := Verify("x", bad); ok || err == nil {
			t.Fatalf("malformed hash %q: %v %v", bad, ok, err)
		}
	}
	if NeedsRehash(hash, testParams) || !NeedsRehash(hash, Params{MemoryKiB: 64 * 1024, Time: 3, Threads: 1}) {
		t.Fatal("rehash decision")
	}
}

func TestGenerate(t *testing.T) {
	a, err := Generate(24)
	if err != nil || len(a) != 24 {
		t.Fatalf("%q %v", a, err)
	}
	b, _ := Generate(24)
	if a == b {
		t.Fatal("generated passwords must differ")
	}
}
