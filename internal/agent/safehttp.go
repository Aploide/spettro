package agent

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"
)

// newSafeHTTPClient builds an HTTP client for fetching untrusted, model- or
// response-chosen URLs. A dial-time control hook rejects connections to
// non-public IP ranges (loopback, link-local, private, unspecified, multicast),
// blocking SSRF to localhost services, cloud metadata (169.254.169.254) and
// internal hosts. The check runs on every dial — including those that result
// from an HTTP redirect or a DNS rebind — because it inspects the actual
// resolved address being dialed rather than the original URL.
func newSafeHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("blocked dial to non-IP address %q", address)
			}
			if !isPublicIP(ip) {
				return fmt.Errorf("blocked request to non-public address %s", ip)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}
}

// isPublicIP reports whether ip is a globally routable unicast address — i.e.
// not loopback, unspecified, link-local, multicast, or RFC1918/ULA private.
func isPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsPrivate() {
		return false
	}
	return true
}

// validatePublicURL ensures a model-provided URL targets http(s) with a host.
// IP-range enforcement happens at dial time via newSafeHTTPClient.
func validatePublicURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported url scheme %q (only http/https allowed)", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("url has no host")
	}
	return nil
}
