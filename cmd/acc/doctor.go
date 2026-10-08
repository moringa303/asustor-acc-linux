package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

// doctor runs a low-level mDNS health check: joins the multicast group on
// every candidate interface, sends PTR queries (generic service enumeration
// plus the ASUSTOR types), and reports what actually comes back. This
// separates "no mDNS traffic at all" (firewall / VLAN isolation) from
// "mDNS works but no ASUSTOR NAS is announcing".
func doctor(timeout time.Duration) {
	fmt.Println("acc doctor: mDNS discovery health check")
	fmt.Println()

	ifaces, _ := net.Interfaces()
	var candidates []net.Interface
	fmt.Println("Network interfaces:")
	for _, ifc := range ifaces {
		var note string
		switch {
		case ifc.Flags&net.FlagUp == 0:
			note = "down, skipped"
		case ifc.Flags&net.FlagLoopback != 0:
			note = "loopback, skipped"
		case ifc.Flags&net.FlagMulticast == 0:
			note = "no multicast, skipped"
		case !hasIPv4(ifc):
			note = "no IPv4 address, skipped"
		default:
			note = "OK, will use"
			candidates = append(candidates, ifc)
		}
		fmt.Printf("  %-14s %s (%s)\n", ifc.Name, note, addrSummary(ifc))
	}
	if len(candidates) == 0 {
		fmt.Println("\nRESULT: no usable multicast interface. Connect to the LAN first.")
		return
	}

	// Bind 0.0.0.0:5353 with SO_REUSEADDR/SO_REUSEPORT so we can coexist
	// with avahi-daemon, then join the mDNS group everywhere.
	lc := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) {
			unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
			unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		})
	}}
	pconn, err := lc.ListenPacket(context.Background(), "udp4", "0.0.0.0:5353")
	if err != nil {
		fmt.Printf("\nRESULT: cannot open UDP port 5353: %v\n", err)
		return
	}
	defer pconn.Close()
	p := ipv4.NewPacketConn(pconn)
	group := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251)}
	joined := 0
	for _, ifc := range candidates {
		ifc := ifc
		if err := p.JoinGroup(&ifc, group); err == nil {
			joined++
		} else {
			fmt.Printf("  warning: cannot join multicast group on %s: %v\n", ifc.Name, err)
		}
	}
	fmt.Printf("\nJoined mDNS multicast group 224.0.0.251 on %d interface(s).\n", joined)

	dst := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
	queries := [][]byte{
		mdnsQuery("_services._dns-sd._udp.local"),
		mdnsQuery("_ASUSTOR_ADM._tcp.local"),
		mdnsQuery("_ASUSTOR_INIT._tcp.local"),
	}
	for _, q := range queries {
		pconn.WriteTo(q, dst)
	}
	// Re-send once mid-listen; mDNS responders may rate-limit.
	go func() {
		time.Sleep(timeout / 2)
		for _, q := range queries {
			pconn.WriteTo(q, dst)
		}
	}()

	hosts := map[string]bool{}
	asustorHosts := map[string]bool{}
	packets, asustorSeen := 0, false
	buf := make([]byte, 9000)
	deadline := time.Now().Add(timeout)
	pconn.SetReadDeadline(deadline)
	for {
		n, src, err := pconn.ReadFrom(buf)
		if err != nil {
			break
		}
		packets++
		host, _, _ := net.SplitHostPort(src.String())
		hosts[host] = true
		if bytes.Contains(bytes.ToUpper(buf[:n]), []byte("ASUSTOR")) {
			asustorSeen = true
			if !asustorHosts[host] {
				asustorHosts[host] = true
				fmt.Printf("  ASUSTOR mDNS answer received from %s\n", host)
			}
		}
	}
	fmt.Printf("Listened %s: %d mDNS packet(s) from %d host(s).\n",
		timeout, packets, len(hosts))

	fmt.Println()
	switch {
	case asustorSeen:
		fmt.Println("RESULT: an ASUSTOR NAS IS answering on this network, so 'acc scan'")
		fmt.Println("should work. If it does not, avahi-daemon may be swallowing unicast")
		fmt.Println("replies; install avahi ('sudo pacman -S avahi' / enable avahi-daemon)")
		fmt.Println("and acc uses it automatically. Or rerun scan with --verbose.")
	case packets > 0:
		fmt.Println("RESULT: mDNS reception works (other devices answered), but no ASUSTOR")
		fmt.Println("NAS responded. Check that:")
		fmt.Println("  - the NAS is powered on and on the SAME subnet/VLAN as this machine")
		fmt.Println("    (mDNS does not cross routers; guest Wi-Fi and IoT VLANs are isolated)")
		fmt.Println("  - ping the NAS IP directly; open http://<nas-ip>:8000 to confirm ADM runs")
		fmt.Println("  - in ADM check Settings > General that the web ports are standard, and")
		fmt.Println("    that no ADM Defender firewall rule blocks the LAN")
	default:
		fmt.Println("RESULT: NO mDNS traffic received at all; replies are being blocked")
		fmt.Println("before they reach this program. On EndeavourOS the usual cause is")
		fmt.Println("firewalld. Allow mDNS with:")
		fmt.Println("  sudo firewall-cmd --add-service=mdns --permanent")
		fmt.Println("  sudo firewall-cmd --reload")
		fmt.Println("(or for ufw: sudo ufw allow 5353/udp)")
		fmt.Println("Then run 'acc doctor' again.")
	}

	if out, err := exec.Command("firewall-cmd", "--state").CombinedOutput(); err == nil &&
		strings.TrimSpace(string(out)) == "running" {
		zones, _ := exec.Command("firewall-cmd", "--get-active-zones").Output()
		services, _ := exec.Command("firewall-cmd", "--list-services").Output()
		fmt.Println()
		fmt.Printf("firewalld is running. Active zones: %s", firstLine(zones))
		fmt.Printf("Allowed services in default zone: %s\n", strings.TrimSpace(string(services)))
		if !strings.Contains(string(services), "mdns") {
			fmt.Println("NOTE: 'mdns' is NOT in the allowed services, which blocks discovery.")
		}
	}
}

func hasIPv4(ifc net.Interface) bool {
	addrs, _ := ifc.Addrs()
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() != nil {
			return true
		}
	}
	return false
}

func addrSummary(ifc net.Interface) string {
	addrs, _ := ifc.Addrs()
	var parts []string
	for _, a := range addrs {
		parts = append(parts, a.String())
	}
	if len(parts) == 0 {
		return "no address"
	}
	return strings.Join(parts, ", ")
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s + "\n"
}

// mdnsQuery builds a bare DNS query packet: one PTR question, QM (multicast
// response requested), as per RFC 6762.
func mdnsQuery(name string) []byte {
	var b bytes.Buffer
	b.Write([]byte{0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0}) // header: 1 question
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		b.WriteByte(byte(len(label)))
		b.WriteString(label)
	}
	b.WriteByte(0)                  // root
	b.Write([]byte{0, 12, 0, 1})    // QTYPE=PTR, QCLASS=IN
	return b.Bytes()
}
