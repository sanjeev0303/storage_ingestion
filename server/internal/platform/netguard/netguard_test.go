package netguard

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestValidateURL(t *testing.T) {
	strict := DefaultPolicy()
	local := DefaultPolicy()
	local.AllowPrivate = true

	ok := []string{"https://s3.eu-west-1.amazonaws.com", "https://abc123.r2.cloudflarestorage.com/", "https://minio.example.com:9443"}
	for _, raw := range ok {
		if _, err := strict.ValidateURL(raw); err != nil {
			t.Errorf("%s refused: %v", raw, err)
		}
	}
	refused := []string{
		"http://s3.example.com",             // plain http to a public host
		"https://user:pass@s3.example.com",  // credentials
		"https://s3.example.com/bucket",     // a path
		"https://s3.example.com?x=1",        // a query
		"https://s3.example.com#frag",       // a fragment
		"ftp://s3.example.com",              // a scheme
		"https://169.254.169.254",           // metadata
		"https://127.0.0.1:9000",            // loopback
		"https://10.0.0.5",                  // private
		"https://[::1]",                     // loopback v6
		"https://[fd00:ec2::254]",           // AWS IPv6 metadata (private)
		"https://100.100.100.200",           // carrier-grade NAT (Alibaba metadata)
		"https://0.0.0.0",                   // unspecified
		"https://s3.example.com:0",          // port
		"https://s3.example.com:99999",      // port
		"https://bad_host.example.com",      // host name
		"//s3.example.com",                  // not absolute
		"https://s3.example.com\\@evil.com", // backslash tricks
		"https://[::ffff:169.254.169.254]",  // mapped metadata
		"https://-bad.example.com",          // label
	}
	for _, raw := range refused {
		if _, err := strict.ValidateURL(raw); !errors.Is(err, ErrRejected) {
			t.Errorf("%s accepted (%v)", raw, err)
		}
	}
	// Local development may use plain http to loopback and private hosts,
	// never to metadata.
	for _, raw := range []string{"http://127.0.0.1:9000", "http://localhost:9000", "http://172.18.0.4:9000"} {
		if _, err := local.ValidateURL(raw); err != nil {
			t.Errorf("local %s refused: %v", raw, err)
		}
	}
	for _, raw := range []string{"http://169.254.169.254", "http://s3.example.com"} {
		if _, err := local.ValidateURL(raw); err == nil {
			t.Errorf("local %s accepted", raw)
		}
	}
}

func TestBlockedAddresses(t *testing.T) {
	p := DefaultPolicy()
	for _, a := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "::1", "fe80::1",
		"fd00:ec2::254", "100.100.100.200", "0.0.0.0", "224.0.0.1", "::ffff:127.0.0.1", "240.0.0.1"} {
		if !p.Blocked(netip.MustParseAddr(a)) {
			t.Errorf("%s allowed", a)
		}
	}
	for _, a := range []string{"52.95.110.1", "104.18.0.1", "2600:1f18::1"} {
		if p.Blocked(netip.MustParseAddr(a)) {
			t.Errorf("%s blocked", a)
		}
	}
}

func TestClientChecksEveryDialAndFollowsNoRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	// The production policy refuses the loopback test server at dial time,
	// whatever the URL said.
	if _, err := DefaultPolicy().Client().Get(target.URL); !errors.Is(err, ErrRejected) {
		t.Fatalf("loopback dial: %v", err)
	}
	local := DefaultPolicy()
	local.AllowPrivate = true
	client := local.Client()
	resp, err := client.Get(target.URL + "/redirect")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("a redirect was followed: %d", resp.StatusCode)
	}
}
