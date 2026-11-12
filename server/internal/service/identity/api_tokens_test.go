package identity

import "testing"

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
