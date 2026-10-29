// Package netguard keeps provider clients from reaching places they must
// not: a customer-supplied storage endpoint is a request to make the
// server connect somewhere. Endpoints are checked in form when they are
// saved, and every connection is checked again at dial time against the
// address actually resolved, so a DNS answer that changes later (DNS
// rebinding) cannot steer a client to loopback, private, link-local or
// cloud metadata addresses. Redirects are never followed.
package netguard

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrRejected: the endpoint or address is not allowed.
var ErrRejected = errors.New("network destination not allowed")

// Policy decides where provider clients may connect.
type Policy struct {
	// AllowPrivate permits loopback and private addresses, and plain HTTP
	// to them: local development only (MinIO, emulators in Docker).
	// Link-local and metadata addresses stay refused regardless.
	AllowPrivate          bool
	DialTimeout           time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
}

// DefaultPolicy is the production policy: public HTTPS endpoints only.
func DefaultPolicy() Policy {
	return Policy{DialTimeout: 10 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 60 * time.Second}
}

// alwaysBlocked are addresses no policy allows: "this network",
// carrier-grade NAT (which hosts some clouds' metadata services), IETF
// protocol assignments, benchmarking and reserved ranges, IPv6 ranges that
// translate or tunnel to an IPv4 address (NAT64, 6to4, Teredo) and so could
// reach a private one, and AWS's IPv6 metadata service, which sits inside
// the private fd00::/8 range.
var alwaysBlocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("fd00:ec2::254/128"),
}

// Blocked reports whether a client may not connect to ip.
func (p Policy) Blocked(ip netip.Addr) bool {
	ip = ip.Unmap()
	switch {
	case !ip.IsValid(), ip.IsUnspecified(), ip.IsMulticast(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(),
		ip.IsInterfaceLocalMulticast():
		// Link-local includes 169.254.169.254, the metadata service of
		// most clouds.
		return true
	}
	// Checked before the private ranges, which AllowPrivate opens.
	for _, prefix := range alwaysBlocked {
		if prefix.Contains(ip) {
			return true
		}
	}
	if ip.IsLoopback() || ip.IsPrivate() {
		return !p.AllowPrivate
	}
	return false
}

// ValidateURL checks an endpoint's form: an absolute https URL (http only
// to a literal private address or localhost under AllowPrivate) with a
// host, an optional valid port, and nothing else: no user info, path,
// query or fragment.
func (p Policy) ValidateURL(raw string) (*url.URL, error) {
	reject := func(reason string) (*url.URL, error) { return nil, fmt.Errorf("%w: %s", ErrRejected, reason) }
	if len(raw) > 512 || strings.ContainsAny(raw, " \t\r\n\\") {
		return reject("the endpoint is malformed")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Opaque != "" {
		return reject("the endpoint must be an absolute URL")
	}
	if u.User != nil {
		return reject("the endpoint must not carry credentials")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return reject("the endpoint must not have a path, query or fragment")
	}
	host := u.Hostname()
	if host == "" || strings.HasSuffix(host, ".") {
		return reject("the endpoint has no valid host")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return reject("the endpoint port is invalid")
		}
	}
	ip, parseErr := netip.ParseAddr(host)
	literal := parseErr == nil
	if literal && p.Blocked(ip) {
		return reject("the endpoint address is not allowed")
	}
	if !literal && !validHostname(host) {
		return reject("the endpoint host name is invalid")
	}
	switch u.Scheme {
	case "https":
	case "http":
		local := host == "localhost" || (literal && (ip.IsLoopback() || ip.IsPrivate()))
		if !p.AllowPrivate || !local {
			return reject("the endpoint must use https")
		}
	default:
		return reject("the endpoint must use https")
	}
	u.Path = ""
	return u, nil
}

// ValidateURLWithPath checks a webhook target or an OIDC issuer:
// ValidateURL's rules for its scheme, host and port, but a path and query
// are allowed. No fragment.
func (p Policy) ValidateURLWithPath(raw string) (*url.URL, error) {
	if len(raw) > 2048 || strings.ContainsAny(raw, " \t\r\n\\") {
		return nil, fmt.Errorf("%w: the URL is malformed", ErrRejected)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" {
		return nil, fmt.Errorf("%w: the URL is malformed or has a fragment", ErrRejected)
	}
	base := *u
	base.Path, base.RawPath, base.RawQuery, base.ForceQuery = "", "", "", false
	if _, err := p.ValidateURL(base.String()); err != nil {
		return nil, err
	}
	return u, nil
}

// validHostname accepts DNS names: labels of letters, digits and hyphens.
func validHostname(host string) bool {
	if len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

// HostIn reports whether host is one of domains or a subdomain of one.
func HostIn(host string, domains ...string) bool {
	host = strings.ToLower(host)
	for _, d := range domains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// Resolve checks every address host resolves to now, so a test reports a
// refused destination clearly. The dial-time check is the one that holds.
func (p Policy) Resolve(ctx context.Context, host string) error {
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("resolve endpoint: %w", err)
	}
	for _, a := range addrs {
		if p.Blocked(a) {
			return fmt.Errorf("%w: the endpoint resolves to a refused address", ErrRejected)
		}
	}
	return nil
}

// control runs after DNS resolution, on the exact address about to be
// dialed.
func (p Policy) control(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrRejected, "unparseable address")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || p.Blocked(ip) {
		return fmt.Errorf("%w: refused address", ErrRejected)
	}
	return nil
}

// Client returns an HTTP client for provider SDKs: every dial checked
// against the policy, no proxy from the environment, no redirects, TLS
// 1.2 or later, and bounded connect, handshake and response-header times.
// The caller still bounds each whole operation with its context.
func (p Policy) Client() *http.Client {
	if p.DialTimeout <= 0 {
		p.DialTimeout = 10 * time.Second
	}
	if p.TLSHandshakeTimeout <= 0 {
		p.TLSHandshakeTimeout = 10 * time.Second
	}
	if p.ResponseHeaderTimeout <= 0 {
		p.ResponseHeaderTimeout = 60 * time.Second
	}
	dialer := &net.Dialer{Timeout: p.DialTimeout, KeepAlive: 30 * time.Second, Control: p.control}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   p.TLSHandshakeTimeout,
		ResponseHeaderTimeout: p.ResponseHeaderTimeout,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			// The response is returned as is; the SDK treats it as an
			// answer, never as a new destination.
			return http.ErrUseLastResponse
		},
	}
}
