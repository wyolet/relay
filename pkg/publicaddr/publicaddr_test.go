package publicaddr

import (
	"errors"
	"net/netip"
	"testing"
)

func TestRefused(t *testing.T) {
	for addr, refused := range map[string]bool{
		"127.0.0.1": true, "127.8.9.1": true, "::1": true,
		"10.1.2.3": true, "172.16.0.1": true, "172.31.255.254": true, "192.168.1.1": true,
		"169.254.169.254": true, "fe80::1": true,
		"100.64.0.1": true, "100.127.255.254": true,
		"fc00::1": true, "fd12:3456::1": true,
		"0.0.0.0": true, "::": true, "0.1.2.3": true,
		"224.0.0.1": true, "239.1.1.1": true, "ff02::1": true, "ff05::2": true,
		"255.255.255.255": true, "240.0.0.1": true,
		"::ffff:127.0.0.1": true, "::ffff:10.0.0.1": true, "::ffff:169.254.169.254": true,
		"64:ff9b::a00:1": true, "64:ff9b::a00:1%eth0": true, "fe80::1%eth0": true,
		"192.0.0.8": true, "198.18.0.1": true, "198.19.255.254": true,
		"::a00:1": true, "::ffff:0:a00:1": true, "2002:a00:1::1": true, "2001:0:4136:e378::1": true, "fec0::1": true,
		"192.0.1.1": false, "198.20.0.1": false, "2001:4860::8888": false, "2003::1": false,
		"8.8.8.8": false, "1.1.1.1": false, "100.63.255.255": false, "100.128.0.1": false,
		"172.32.0.1": false, "2606:4700::1111": false, "::ffff:8.8.8.8": false,
	} {
		if got := Refused(netip.MustParseAddr(addr)); got != refused {
			t.Errorf("%s: refused %v, want %v", addr, got, refused)
		}
	}
}

func TestDialControl(t *testing.T) {
	for address, refused := range map[string]bool{
		"10.0.0.5:443": true, "[fd00::1]:443": true, "169.254.169.254:80": true, "127.0.0.1:8443": true,
		"not-an-address": true, "8.8.8.8:443": false, "[2606:4700::1111]:443": false,
	} {
		err := DialControl("tcp", address, nil)
		if got := errors.Is(err, ErrRefused); got != refused {
			t.Errorf("%s: refused %v, want %v", address, got, refused)
		}
	}
}
