// Package publicaddr decides which addresses relay may send a request to when
// the URL comes from a caller (a webhook receiver, a token endpoint): public
// unicast only, so a caller cannot reach loopback, private networks, link-local
// services such as a cloud metadata endpoint, or the relay host itself.
//
// It decides on resolved IPs, never on hostnames: Refused checks one address,
// and DialControl checks the address a net.Dialer is about to connect to, after
// DNS. Redirects, proxies and URL syntax are the caller's concern.
package publicaddr

import (
	"errors"
	"net/netip"
	"syscall"
)

// ErrRefused is the error for an address that is not public unicast.
var ErrRefused = errors.New("address is not a public unicast address")

// refusedPrefixes are the non-public ranges netip has no predicate for.
var refusedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("240.0.0.0/4"),
	// NAT64 embeds an IPv4 address the gateway would route to.
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
}

// Refused reports whether a is refused: loopback, RFC 1918, link-local, CGNAT,
// IPv6 ULA, unspecified, multicast and reserved ranges.
func Refused(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsUnspecified() || a.IsMulticast() ||
		a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() {
		return true
	}
	for _, p := range refusedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// DialControl is a net.Dialer Control function that refuses to connect to a
// refused address. address is the resolved "ip:port" the dialer is connecting
// to.
func DialControl(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil || Refused(ap.Addr()) {
		return ErrRefused
	}
	return nil
}
