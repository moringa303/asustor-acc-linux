package main

import (
	"fmt"
	"net"
	"strings"
	"time"
)

// sendWOL sends a Wake-on-LAN magic packet for mac. The limited broadcast
// 255.255.255.255 only leaves through the default route, so the packet
// also goes to the directed broadcast of every usable interface (or of
// --iface only) and, when the address of the NAS is known, to the NAS
// itself. It returns the addresses the packet went to.
func sendWOL(mac, ip string) ([]string, error) {
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return nil, fmt.Errorf("invalid MAC %q: %w", mac, err)
	}
	packet := make([]byte, 0, 102)
	packet = append(packet, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF)
	for i := 0; i < 16; i++ {
		packet = append(packet, hw...)
	}

	targets := append([]string{"255.255.255.255"}, broadcastAddrs()...)
	if ip4 := net.ParseIP(ip); ip4 != nil && ip4.To4() != nil {
		targets = append(targets, ip4.String())
	}

	var sent []string
	var lastErr error
	seen := map[string]bool{}
	for _, t := range targets {
		if seen[t] {
			continue
		}
		seen[t] = true
		conn, err := net.DialTimeout("udp4", net.JoinHostPort(t, "9"), time.Second)
		if err == nil {
			_, err = conn.Write(packet)
			conn.Close()
		}
		if err != nil {
			vlog("magic packet to %s: %v", t, err)
			lastErr = err
			continue
		}
		sent = append(sent, t)
	}
	if len(sent) == 0 {
		return nil, fmt.Errorf("cannot send magic packet: %w", lastErr)
	}
	vlog("magic packet for %s sent to %s", strings.ToUpper(hw.String()), strings.Join(sent, ", "))
	return sent, nil
}

// broadcastAddrs lists the IPv4 directed broadcast address of every
// interface that is up and not loopback (only --iface when set).
func broadcastAddrs() []string {
	ifaces, _ := net.Interfaces()
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 ||
			ifc.Flags&net.FlagBroadcast == 0 {
			continue
		}
		if ifaceName != "" && ifc.Name != ifaceName {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			vlog("addresses of %s: %v", ifc.Name, err)
			continue
		}
		out = append(out, broadcastsOf(addrs)...)
	}
	return out
}

// broadcastsOf computes the directed broadcast address of every IPv4
// network among addrs; other addresses are skipped.
func broadcastsOf(addrs []net.Addr) []string {
	var out []string
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip4 := ipnet.IP.To4()
		mask := ipnet.Mask
		// A 16-byte mask of an IPv4-mapped address: only the tail counts.
		if len(mask) == net.IPv6len {
			mask = mask[12:]
		}
		if ip4 == nil || len(mask) != net.IPv4len {
			continue
		}
		bcast := make(net.IP, net.IPv4len)
		for i := range bcast {
			bcast[i] = ip4[i] | ^mask[i]
		}
		out = append(out, bcast.String())
	}
	return out
}
