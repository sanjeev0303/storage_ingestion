package adoption

import (
	"context"
	"crypto/sha512"
	"fmt"
	"strings"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

func mhlGeneration(rows ...string) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?><hashlist version="2.0" xmlns="urn:ASC:MHL:v2.0"><hashes>` +
		strings.Join(rows, "") + `</hashes></hashlist>`)
}

func mhlRow(path string, size int, md5 string) string {
	return fmt.Sprintf(`<hash><path size="%d">%s</path><md5 action="original">%s</md5></hash>`, size, path, md5)
}

func chainOf(gens map[int]string, src files) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ascmhldirectory xmlns="urn:ASC:MHL:DIRECTORY:v2.0">`)
	for seq := 1; seq <= len(gens); seq++ {
		sum := sha512.Sum512(src["shoot/ascmhl/"+gens[seq]])
		fmt.Fprintf(&b, `<hashlist sequencenr="%d"><path>%s</path><c4>%s</c4></hashlist>`, seq, gens[seq], c4ID(sum[:]))
	}
	b.WriteString(`</ascmhldirectory>`)
	return []byte(b.String())
}

func TestMHLChainTakesEveryGenerationNewestFirst(t *testing.T) {
	gens := map[int]string{1: "0001_shoot.mhl", 2: "0002_shoot.mhl"}
	src := files{
		"shoot/ascmhl/0001_shoot.mhl": mhlGeneration(
			mhlRow("Clips/A001.mov", 10, "900150983cd24fb0d6963f7d28e17f72"),
			mhlRow("Clips/A002.mov", 20, "e2fc714c4727ee9395f324cd2e7f331f")),
		"shoot/ascmhl/0002_shoot.mhl": mhlGeneration(
			mhlRow("Clips/A002.mov", 21, "d41d8cd98f00b204e9800998ecf8427e"),
			mhlRow("Sound/A001.wav", 5, "0cc175b9c0f1b6a831c399e269772661")),
	}
	src["shoot/ascmhl/ascmhl_chain.xml"] = chainOf(gens, src)

	for _, key := range []string{"shoot/ascmhl", "shoot/ascmhl/", "shoot/ascmhl/ascmhl_chain.xml"} {
		got := collectEntries(t, func(e *emitter) error { return readMHL(context.Background(), src, key, e) })
		if len(got) != 3 {
			t.Fatalf("%s: %d entries %+v", key, len(got), got)
		}
		byKey := map[string]int{}
		for i, entry := range got {
			byKey[entry.ObjectKey] = i
		}
		a2 := got[byKey["shoot/Clips/A002.mov"]]
		if a2.SizeBytes != 21 || a2.Expected["md5"] != "d41d8cd98f00b204e9800998ecf8427e" {
			t.Fatalf("%s: the newest generation's hash should win: %+v", key, a2)
		}
		if _, ok := byKey["shoot/Clips/A001.mov"]; !ok {
			t.Fatalf("%s: a file only in the first generation is missing", key)
		}
	}

	// A generation changed after the chain recorded it vouches for nothing.
	src["shoot/ascmhl/0001_shoot.mhl"] = mhlGeneration(mhlRow("Clips/A001.mov", 10, "ffffffffffffffffffffffffffffffff"))
	err := readMHL(context.Background(), src, "shoot/ascmhl", &emitter{emit: func(model.AdoptionEntry) error { return nil }})
	if err == nil || !strings.Contains(err.Error(), "does not match the chain") {
		t.Fatalf("a tampered generation was read: %v", err)
	}
}
