package identity

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
)

func TestIsAPITokenAcceptsCurrentAndLegacyPrefix(t *testing.T) {
	cases := map[string]bool{
		"orb_pat_abc":        true,
		"osp_pat_abc":        true,
		"eyJhbGciOiJIUzI1Ni": false,
		"orb_abc":            false,
	}
	for raw, want := range cases {
		if got := isAPIToken(raw); got != want {
			t.Errorf("isAPIToken(%q) = %v, want %v", raw, got, want)
		}
	}
}

type tokenTable struct {
	byHash map[string]model.APIToken
}

func (t *tokenTable) TokenByHash(_ context.Context, hash string) (model.APIToken, error) {
	token, ok := t.byHash[hash]
	if !ok {
		return model.APIToken{}, model.ErrAPITokenNotFound
	}
	return token, nil
}

func (t *tokenTable) TouchToken(context.Context, string, string, time.Time) error { return nil }

func (t *tokenTable) RehashToken(_ context.Context, id, oldHash, newHash string) error {
	if token, ok := t.byHash[oldHash]; ok && token.ID == id {
		delete(t.byHash, oldHash)
		token.TokenHash = newHash
		t.byHash[newHash] = token
	}
	return nil
}

func TestPepperedAndOlderTokensBothAuthenticate(t *testing.T) {
	pepper := bytes.Repeat([]byte{7}, 32)
	newer := APITokenPrefix + strings.Repeat("n", 43)
	older := APITokenPrefix + strings.Repeat("o", 43)
	table := &tokenTable{byHash: map[string]model.APIToken{
		HashAPIToken(newer, pepper): {ID: "tok_new"},
		HashAPIToken(older, nil):    {ID: "tok_old"},
	}}
	s := &Service{cfg: config.Auth{APITokenPepper: pepper}, tokens: table,
		deps: Dependencies{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	ctx := context.Background()

	if token, err := s.lookupToken(ctx, newer); err != nil || token.ID != "tok_new" {
		t.Fatalf("peppered token: %+v %v", token, err)
	}
	if token, err := s.lookupToken(ctx, older); err != nil || token.ID != "tok_old" {
		t.Fatalf("token issued before the pepper: %+v %v", token, err)
	}
	// Its plain hash was replaced on that first use.
	if _, ok := table.byHash[HashAPIToken(older, nil)]; ok {
		t.Fatal("the plain SHA-256 is still stored")
	}
	if token, err := s.lookupToken(ctx, older); err != nil || token.ID != "tok_old" {
		t.Fatalf("after the upgrade: %+v %v", token, err)
	}
	// A hash someone wrote into the table without the pepper is the plain
	// kind; a peppered hash of a guess made with another key never matches.
	if _, err := s.lookupToken(ctx, APITokenPrefix+strings.Repeat("x", 43)); err == nil {
		t.Fatal("an unknown token authenticated")
	}
	if HashAPIToken(newer, pepper) == HashAPIToken(newer, nil) || HashAPIToken(newer, pepper) == HashAPIToken(newer, bytes.Repeat([]byte{8}, 32)) {
		t.Fatal("the pepper does not change the stored hash")
	}

	// Without a pepper only plain hashes are looked up, as before.
	plain := &Service{tokens: &tokenTable{byHash: map[string]model.APIToken{HashAPIToken(older, nil): {ID: "tok_old"}}}}
	if token, err := plain.lookupToken(ctx, older); err != nil || token.ID != "tok_old" {
		t.Fatalf("no pepper: %+v %v", token, err)
	}
}
