package services

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// GuardOptions configure NewGuardedDialer.
type GuardOptions struct {
	// AllowedPrefixes are always permitted even if they fall in a blocked range (e.g. an
	// in-cluster Alertmanager service CIDR).
	AllowedPrefixes []netip.Prefix
	// BlockedPrefixes are blocked in addition to the built-in non-public ranges.
	BlockedPrefixes []netip.Prefix
	// Timeout is the connect timeout (default 10s).
	Timeout time.Duration
}

// NewGuardedDialer returns a DialContext that refuses connections to loopback, private,
// link-local, CGNAT, unique-local, multicast and unspecified addresses (plus any extra
// blocked prefixes) unless explicitly allowed. The check runs on the resolved IP right
// before connect, so DNS rebinding cannot bypass it. Plug it into Network.DialContext to
// protect against SSRF through user-influenced notification endpoints.
func NewGuardedDialer(opts GuardOptions) func(ctx context.Context, network, address string) (net.Conn, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	d := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("%w: cannot parse dial address %q", ErrURLNotAllowed, address)
			}
			if !addrAllowed(ap.Addr().Unmap(), opts) {
				return fmt.Errorf("%w: address %s is not publicly routable", ErrURLNotAllowed, ap.Addr())
			}
			return nil
		},
	}
	return d.DialContext
}

var extraNonPublic = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),  // CGNAT
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("0.0.0.0/8"),      // "this" network
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
	netip.MustParsePrefix("2001:db8::/32"),  // documentation
	netip.MustParsePrefix("169.254.0.0/16"), // link-local (covers cloud metadata)
	netip.MustParsePrefix("fd00:ec2::/32"),  // AWS IMDS IPv6
	netip.MustParsePrefix("255.255.255.255/32"),
}

func addrAllowed(a netip.Addr, opts GuardOptions) bool {
	for _, p := range opts.AllowedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	for _, p := range opts.BlockedPrefixes {
		if p.Contains(a) {
			return false
		}
	}
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return false
	}
	for _, p := range extraNonPublic {
		if p.Contains(a) {
			return false
		}
	}
	// IPv6 forms that embed an IPv4 destination (NAT64 well-known prefix, 6to4) are judged
	// by the embedded address so that e.g. 64:ff9b::7f00:1 cannot reach 127.0.0.1 through a
	// NAT64 gateway.
	if v4, ok := embeddedIPv4(a); ok {
		return addrAllowed(v4, opts)
	}
	return true
}

var (
	nat64WellKnown = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour      = netip.MustParsePrefix("2002::/16")
)

func embeddedIPv4(a netip.Addr) (netip.Addr, bool) {
	if !a.Is6() {
		return netip.Addr{}, false
	}
	b := a.As16()
	switch {
	case nat64WellKnown.Contains(a):
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	case sixToFour.Contains(a):
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), true
	}
	return netip.Addr{}, false
}
