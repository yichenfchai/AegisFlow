package deployments

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// errBlockedTargetAddress marks a connection refused by the deployment-target
// network guard. Probe and deployment failures surface it only through the
// sanitized detail used for any network error.
var errBlockedTargetAddress = errors.New("blocked deployment target address")

// blockedTargetNets are the addresses a deployment target may never reach, even
// though admin configures endpoints: cloud instance-metadata services and IPv6
// link-local addresses. Loopback and private ranges are deliberately absent so
// self-hosted targets on an internal network keep working, and public addresses
// are allowed by default.
var blockedTargetNets = []*net.IPNet{
	mustCIDR("169.254.0.0/16"),     // IPv4 link-local, includes 169.254.169.254
	mustCIDR("fe80::/10"),          // IPv6 link-local
	mustCIDR("100.100.100.200/32"), // Alibaba Cloud metadata IPv4
	mustCIDR("fd00:ec2::254/128"),  // AWS instance metadata over IPv6
}

func mustCIDR(cidr string) *net.IPNet {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		panic("deployments: invalid blocked target network " + cidr)
	}
	return network
}

// blockedTargetIP reports whether ip is a cloud metadata or link-local address.
// A nil IP is not blocked: callers that need a resolved address check that
// separately. IPv4-mapped IPv6 forms are matched through net.IPNet.Contains,
// which compares them in their 4-byte form.
func blockedTargetIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, network := range blockedTargetNets {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// parseHostIP parses an IP literal, tolerating an IPv6 zone suffix such as
// "fe80::1%eth0": url.Hostname keeps the zone and the dialer passes it through
// in the resolved address, but the blocked ranges are matched on the address
// itself.
func parseHostIP(host string) net.IP {
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	return net.ParseIP(host)
}

// blockedTargetHost reports whether host is an IP literal inside a blocked
// range. A DNS name returns false here: it is re-checked per connection by
// targetDialControl after resolution, which also covers DNS rebinding between
// the create/update validation and the moment a probe or deployment runs.
func blockedTargetHost(host string) bool {
	if host == "" {
		return false
	}
	return blockedTargetIP(parseHostIP(host))
}

// targetDialControl is installed as net.Dialer.Control on every connection the
// deployment clients make. Control runs after name resolution and immediately
// before connect, so a hostname that resolved to a blocked address is refused
// without opening a connection; callers cannot bypass it by changing the
// endpoint's spelling after validation.
//
// The system resolver guarantees address is an IP literal with a port; anything
// else is treated as a guard failure. The unused RawConn is what the net package
// passes to every Control hook.
func targetDialControl(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: malformed dial address", errBlockedTargetAddress)
	}
	ip := parseHostIP(host)
	if ip == nil {
		return fmt.Errorf("%w: unresolved dial address", errBlockedTargetAddress)
	}
	if blockedTargetIP(ip) {
		return fmt.Errorf("%w: %s is a link-local or cloud metadata address", errBlockedTargetAddress, ip)
	}
	return nil
}

// newTargetDialer builds the dialer shared by the guarded transports. The
// timeout and keep-alive mirror http.DefaultTransport's dialer; the guard is
// what makes the deployment clients refuse metadata/link-local peers.
func newTargetDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   targetDialControl,
	}
}

// guardedTransport is the transport for the probe/deploy and approval-webhook
// clients. It starts from http.DefaultTransport so timeouts, HTTP/2 and idle
// connection handling stay as they were, then replaces the dialer and disables
// proxy lookup: with an HTTP proxy the TCP peer would be the proxy, so the
// guard would never see the target address and SSRF-style redirects through a
// proxy could reach metadata. A configured proxy is therefore not used for
// these clients.
func guardedTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = newTargetDialer().DialContext
	return transport
}

// withTargetDialGuard returns a copy of a caller-supplied client with the
// network guard installed on its transport. Client settings (timeout, redirect
// policy, TLS configuration) are preserved; only the dialing path changes. A
// custom RoundTripper is left untouched because its dialing is opaque, which is
// documented on Options.HTTPClient.
func withTargetDialGuard(client *http.Client) *http.Client {
	guarded := *client
	switch transport := client.Transport.(type) {
	case nil:
		guarded.Transport = guardedTransport()
	case *http.Transport:
		clone := transport.Clone()
		clone.Proxy = nil
		clone.DialContext = newTargetDialer().DialContext
		// A transport that dials TLS through its own hook would skip
		// DialContext; clearing it routes the TLS handshake over the guarded
		// dial path instead.
		clone.DialTLSContext = nil
		guarded.Transport = clone
	}
	return &guarded
}
