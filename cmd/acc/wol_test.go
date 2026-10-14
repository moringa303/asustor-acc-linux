package main

import (
	"net"
	"reflect"
	"testing"
)

func cidr(t *testing.T, s string) *net.IPNet {
	t.Helper()
	ip, ipnet, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatal(err)
	}
	ipnet.IP = ip // the interface address, not the network number
	return ipnet
}

func TestBroadcastsOf(t *testing.T) {
	tests := []struct {
		name  string
		addrs []net.Addr
		want  []string
	}{
		{"typical LAN", []net.Addr{cidr(t, "192.168.1.10/24")}, []string{"192.168.1.255"}},
		{"class A", []net.Addr{cidr(t, "10.1.2.3/8")}, []string{"10.255.255.255"}},
		{"odd prefix", []net.Addr{cidr(t, "172.16.5.77/27")}, []string{"172.16.5.95"}},
		{"host route", []net.Addr{cidr(t, "100.64.0.1/32")}, []string{"100.64.0.1"}},
		{"IPv6 skipped", []net.Addr{cidr(t, "fe80::1/64"), cidr(t, "2001:db8::5/48")}, nil},
		{"mixed", []net.Addr{cidr(t, "fe80::1/64"), cidr(t, "192.168.1.10/24"), cidr(t, "10.0.0.2/16")}, []string{"192.168.1.255", "10.0.255.255"}},
		{"not a network", []net.Addr{&net.IPAddr{IP: net.IPv4(192, 168, 1, 10)}, &net.UDPAddr{IP: net.IPv4(192, 168, 1, 11)}}, nil},
		// A 16-byte IPv4 address with a 16-byte mask, as some callers
		// build them.
		{"IPv4 in 16 bytes", []net.Addr{&net.IPNet{IP: net.IPv4(192, 168, 2, 3), Mask: net.CIDRMask(120, 128)}}, []string{"192.168.2.255"}},
		{"IPv4 with a mangled mask", []net.Addr{&net.IPNet{IP: net.IPv4(192, 168, 2, 3), Mask: net.CIDRMask(3, 8)}}, nil},
		{"empty", nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := broadcastsOf(tc.addrs); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
