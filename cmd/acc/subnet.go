package main

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// Scanning other subnets. Multicast DNS stops at the router, but an
// ASUSTOR NAS answers the same question when it is asked directly
// (a unicast query to its port 5353, RFC 6762 section 6.7), so a subnet
// can be swept host by host: `acc scan --subnet 10.1.2.0/24`.

// maxSubnetBits caps a sweep at a /22 (1022 hosts).
const maxSubnetBits = 22

// subnetHosts lists the host addresses of an IPv4 CIDR.
func subnetHosts(cidr string) ([]net.IP, error) {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}
	ip4 := ipnet.IP.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("%s: only IPv4 subnets can be swept", cidr)
	}
	ones, _ := ipnet.Mask.Size()
	if ones < maxSubnetBits {
		return nil, fmt.Errorf("%s: at most a /%d can be swept", cidr, maxSubnetBits)
	}
	if ones >= 31 {
		return []net.IP{ip.To4()}, nil
	}
	start := uint32(ip4[0])<<24 | uint32(ip4[1])<<16 | uint32(ip4[2])<<8 | uint32(ip4[3])
	n := uint32(1) << (32 - ones)
	hosts := make([]net.IP, 0, n-2)
	for i := uint32(1); i < n-1; i++ { // skip the network and broadcast addresses
		a := start + i
		hosts = append(hosts, net.IPv4(byte(a>>24), byte(a>>16), byte(a>>8), byte(a)).To4())
	}
	return hosts, nil
}

// subnetScan asks every host of cidr for the ASUSTOR services.
func subnetScan(cidr string, timeout time.Duration) ([]Nas, error) {
	hosts, err := subnetHosts(cidr)
	if err != nil {
		return nil, err
	}
	var (
		mu    sync.Mutex
		found = map[string]Nas{}
		wg    sync.WaitGroup
		sem   = make(chan struct{}, 128)
	)
	for _, h := range hosts {
		wg.Add(1)
		go func(h net.IP) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			for _, n := range unicastQuery(h.String(), timeout) {
				vlog("unicast mDNS answer: %s (%s)", n.Name, n.IP)
				mu.Lock()
				mergeNas(found, n)
				mu.Unlock()
			}
		}(h)
	}
	wg.Wait()
	return sortedByName(found), nil
}

var serviceNames = []struct {
	name string
	init bool
}{{"_ASUSTOR_ADM._tcp.local.", true}, {"_ASUSTOR_INIT._tcp.local.", false}}

// unicastQuery asks one host for both services and reads the records
// of every instance it announces.
func unicastQuery(ip string, timeout time.Duration) []Nas {
	conn, err := net.DialTimeout("udp", net.JoinHostPort(ip, "5353"), timeout)
	if err != nil {
		return nil
	}
	defer conn.Close()
	q := new(dns.Msg)
	q.Id = dns.Id()
	for _, s := range serviceNames {
		q.Question = append(q.Question, dns.Question{Name: s.name, Qtype: dns.TypePTR, Qclass: dns.ClassINET | 1<<15})
	}
	records := exchange(conn, q, timeout)
	if len(records) == 0 {
		return nil
	}
	// Instances announced, and the records they need. A responder that
	// leaves out the SRV, TXT or A records is asked once more for them.
	instances := map[string]bool{} // instance -> initialized
	for _, rr := range records {
		if ptr, ok := rr.(*dns.PTR); ok {
			for _, s := range serviceNames {
				if strings.EqualFold(ptr.Hdr.Name, s.name) {
					instances[ptr.Ptr] = s.init
				}
			}
		}
	}
	if len(instances) == 0 {
		return nil
	}
	missing := false
	for inst := range instances {
		if srvOf(records, inst) == nil {
			missing = true
		}
	}
	if missing {
		q := new(dns.Msg)
		q.Id = dns.Id()
		for inst := range instances {
			q.Question = append(q.Question, dns.Question{Name: inst, Qtype: dns.TypeANY, Qclass: dns.ClassINET | 1<<15})
		}
		records = append(records, exchange(conn, q, timeout)...)
	}
	var list []Nas
	for inst, initialized := range instances {
		nas := Nas{Name: instanceLabel(inst), IP: ip, Initialized: initialized}
		if srv := srvOf(records, inst); srv != nil {
			nas.Port = int(srv.Port)
			for _, rr := range records {
				if a, ok := rr.(*dns.A); ok && strings.EqualFold(a.Hdr.Name, srv.Target) {
					nas.IP = a.A.String()
				}
			}
		}
		for _, rr := range records {
			if txt, ok := rr.(*dns.TXT); ok && strings.EqualFold(txt.Hdr.Name, inst) {
				nas.applyTxt(txt.Txt)
			}
		}
		if nas.State == "" {
			if initialized {
				nas.State = "Ready"
			} else {
				nas.State = "Uninitialized"
			}
		}
		list = append(list, nas)
	}
	return list
}

// exchange sends q and collects the records of every answer that comes
// back before the deadline (a responder may send several packets).
func exchange(conn net.Conn, q *dns.Msg, timeout time.Duration) []dns.RR {
	packet, err := q.Pack()
	if err != nil {
		return nil
	}
	if _, err := conn.Write(packet); err != nil {
		return nil
	}
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil
	}
	var records []dns.RR
	buf := make([]byte, 9000)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return records
		}
		resp := new(dns.Msg)
		if err := resp.Unpack(buf[:n]); err != nil || resp.Id != q.Id && resp.Id != 0 {
			continue
		}
		records = append(records, resp.Answer...)
		records = append(records, resp.Extra...)
		// Everything there is usually comes in one packet; a short wait
		// catches a second one without costing the full timeout.
		conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	}
}

func srvOf(records []dns.RR, instance string) *dns.SRV {
	for _, rr := range records {
		if srv, ok := rr.(*dns.SRV); ok && strings.EqualFold(srv.Hdr.Name, instance) {
			return srv
		}
	}
	return nil
}

// instanceLabel turns "My NAS._ASUSTOR_ADM._tcp.local." (with DNS
// escapes such as \032 for a space) back into "My NAS".
func instanceLabel(name string) string {
	labels := dns.SplitDomainName(name)
	if len(labels) == 0 {
		return name
	}
	l := labels[0]
	var b strings.Builder
	for i := 0; i < len(l); i++ {
		if l[i] == '\\' && i+1 < len(l) {
			if i+3 < len(l) && isDigit(l[i+1]) && isDigit(l[i+2]) && isDigit(l[i+3]) {
				if v, err := strconv.Atoi(l[i+1 : i+4]); err == nil && v < 256 {
					b.WriteByte(byte(v))
					i += 3
					continue
				}
			}
			b.WriteByte(l[i+1])
			i++
			continue
		}
		b.WriteByte(l[i])
	}
	return b.String()
}
