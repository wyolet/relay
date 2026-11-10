package catalogserver

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"
)

const (
	hookTimeout = 5 * time.Second
	maxHookURL  = 2048
)

var errRefusedAddr = errors.New("address is not a public unicast address")

// refusedPrefixes are the non-public ranges netip has no predicate for.
var refusedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("240.0.0.0/4"),
	// NAT64 embeds an IPv4 address the gateway would route to.
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
}

// refusedAddr reports whether a receiver at a is refused: loopback, RFC 1918,
// link-local, CGNAT, IPv6 ULA, unspecified, multicast and reserved ranges.
func refusedAddr(a netip.Addr) bool {
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

type resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// receiverPolicy decides which receiver URLs and addresses are accepted.
// allowLoopback is a development switch: it admits http:// and loopback
// addresses, never any other refused range.
type receiverPolicy struct {
	resolver      resolver
	allowLoopback bool
}

func (p receiverPolicy) refused(a netip.Addr) bool {
	a = a.Unmap()
	return refusedAddr(a) && !(p.allowLoopback && a.IsLoopback())
}

// parseURL accepts an absolute https URL without credentials.
func (p receiverPolicy) parseURL(raw string) (*url.URL, error) {
	if len(raw) > maxHookURL {
		return nil, fmt.Errorf("url longer than %d bytes", maxHookURL)
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return nil, err
	case u.Scheme != "https" && !(p.allowLoopback && u.Scheme == "http"):
		return nil, errors.New("url must be https")
	case u.User != nil:
		return nil, errors.New("url must not carry credentials")
	case u.Hostname() == "":
		return nil, errors.New("url has no host")
	}
	u.Fragment, u.RawFragment = "", ""
	return u, nil
}

// resolve looks host up once and returns the address every request to it
// dials. Any refused address in the answer refuses the host, so a mixed
// public/private answer can't be used to reach the private one later.
func (p receiverPolicy) resolve(ctx context.Context, host string) (netip.Addr, error) {
	addrs := []netip.Addr{}
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = append(addrs, a)
	} else {
		ctx, cancel := context.WithTimeout(ctx, hookTimeout)
		defer cancel()
		if addrs, err = p.resolver.LookupNetIP(ctx, "ip", host); err != nil {
			return netip.Addr{}, err
		}
	}
	if len(addrs) == 0 {
		return netip.Addr{}, fmt.Errorf("%s has no address", host)
	}
	for _, a := range addrs {
		if p.refused(a) {
			return netip.Addr{}, errRefusedAddr
		}
	}
	return addrs[0].Unmap(), nil
}

type pinnedAddrKey struct{}

// withPinnedAddr makes the receiver client dial addr whatever the URL's host
// resolves to now, so a DNS answer that changes after registration can't
// redirect a request.
func withPinnedAddr(ctx context.Context, addr netip.Addr) context.Context {
	return context.WithValue(ctx, pinnedAddrKey{}, addr)
}

// newReceiverClient keeps the URL's host for SNI, certificate checks and the
// Host header while dialing the pinned address. No proxy, no redirects and
// no pooled connections, so every request dials the address it was given.
func newReceiverClient(p receiverPolicy, tlsConfig *tls.Config) *http.Client {
	dialer := &net.Dialer{Timeout: hookTimeout}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			pinned, ok := ctx.Value(pinnedAddrKey{}).(netip.Addr)
			if !ok || p.refused(pinned) {
				return nil, errRefusedAddr
			}
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(pinned.String(), port))
		},
		TLSClientConfig:        tlsConfig,
		TLSHandshakeTimeout:    hookTimeout,
		ResponseHeaderTimeout:  hookTimeout,
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: 16 << 10,
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
