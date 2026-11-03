package domain

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

func TestStacValueKeepsWhatWouldBeLost(t *testing.T) {
	if v := stacValue("04.00"); v != "04.00" {
		t.Fatalf("a baseline became %v", v)
	}
	if v := stacValue("12.5"); v != 12.5 {
		t.Fatalf("a number stayed %v", v)
	}
	if v := stacValue("true"); v != true {
		t.Fatalf("a boolean stayed %v", v)
	}
}

func TestScore(t *testing.T) {
	q := []float32{1, 0}
	a := encodeVector([]float32{2, 0})
	b := encodeVector([]float32{0, 3})
	if s := score(model.MetricCosine, q, 1, a); math.Abs(s-1) > 1e-9 {
		t.Fatalf("cosine: %v", s)
	}
	if s := score(model.MetricCosine, q, 1, b); math.Abs(s) > 1e-9 {
		t.Fatalf("cosine: %v", s)
	}
	if s := score(model.MetricDot, q, 1, a); s != 2 {
		t.Fatalf("dot: %v", s)
	}
	if s := score(model.MetricEuclidean, q, 1, a); s != -1 {
		t.Fatalf("euclidean: %v", s)
	}
}

func TestCitationsCannotBeBrokenByTheirValues(t *testing.T) {
	pub := model.Publication{PublicationMeta: model.PublicationMeta{Title: "Bad } title\nER  - ", Publisher: "Lab & co",
		Creators: []model.Creator{{Name: "A\nTI  - forged"}, {Name: "B"}}}, Slug: "s", PublicationYear: 2026, VersionNumber: 3,
		CreatedAt: time.Now(), DOI: "10.5072/osp.s"}
	if got := bibtex(pub, "https://x/published/s"); !strings.Contains(got, `Bad \} title`) || !strings.Contains(got, `Lab \& co`) {
		t.Fatalf("bibtex: %s", got)
	}
	got := ris(pub, "https://x/published/s")
	if strings.Count(got, "ER  - ") != 2 && !strings.Contains(got, "TI  - Bad } title ER  - ") {
		t.Fatalf("ris: %s", got)
	}
	for _, line := range strings.Split(got, "\r\n") {
		if strings.HasPrefix(line, "TI  - forged") {
			t.Fatalf("a value forged a tag: %s", got)
		}
	}
	if c := citation(pub, "https://x"); !strings.Contains(c, "https://doi.org/10.5072/osp.s") {
		t.Fatalf("citation: %s", c)
	}
}

// The bug this guards: a server that did not know its address put a relative
// link in every citation.
func TestCitationsNeverHoldHalfALink(t *testing.T) {
	for _, bad := range []string{"", "console.example.org", "/published", "ftp://x.org", "https://", "https://x.org?a=1"} {
		if CheckPublicURL(bad) == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
	for _, good := range []string{"https://console.example.org", "http://localhost:3100", "https://x.org/console/"} {
		if err := CheckPublicURL(good); err != nil {
			t.Fatal(err)
		}
	}
	s := New(Deps{})
	if got := s.LandingURL("abc"); got != "" {
		t.Fatalf("landing: %q", got)
	}
	pub := model.Publication{PublicationMeta: model.PublicationMeta{Title: "T", Publisher: "P", Creators: []model.Creator{{Name: "A"}}}, Slug: "abc", PublicationYear: 2026, CreatedAt: time.Now()}
	for name, text := range map[string]string{"citation": citation(pub, ""), "bibtex": bibtex(pub, ""), "ris": ris(pub, "")} {
		if strings.Contains(text, "/published") || strings.Contains(text, "url = {}") || strings.Contains(text, "UR  - ") {
			t.Fatalf("%s: %s", name, text)
		}
	}
}

func TestRetentionEstimate(t *testing.T) {
	e := estimate(100<<30, 10, []TierCost{{Tier: "x", PricePerGBMonth: 0.01}})
	if math.Abs(e.Tiers[0].PerYear-12) > 1e-9 || math.Abs(e.Tiers[0].Total-120) > 1e-9 {
		t.Fatalf("%+v", e)
	}
}
