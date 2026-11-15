package catalogserver

import (
	"net/netip"
	"testing"
)

func TestRefusedAddr(t *testing.T) {
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
		"64:ff9b::a00:1": true,
		"8.8.8.8":        false, "1.1.1.1": false, "100.63.255.255": false, "100.128.0.1": false,
		"172.32.0.1": false, "2606:4700::1111": false, "::ffff:8.8.8.8": false,
	} {
		if got := refusedAddr(netip.MustParseAddr(addr)); got != refused {
			t.Errorf("%s: refused %v, want %v", addr, got, refused)
		}
	}
}
