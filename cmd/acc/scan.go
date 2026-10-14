package main

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
)

// recheckTimeout is how long a second look lasts when a NAS that answered
// the previous scan is missing from this one. mDNS answers do get lost
// occasionally; one lost answer should not show a NAS as offline.
const recheckTimeout = 2 * time.Second

// scan discovers NAS devices on the LAN and records the result in the
// device memory. The returned list holds the devices that answered first,
// then remembered devices that did not (Online == false).
func scan(timeout time.Duration) ([]Nas, error) {
	known := loadMemory()
	// Remembered devices are asked for their uptime while discovery
	// listens, so SNMP adds no time to the scan unless a NAS is new.
	probe := newUptimeProbe()
	answeredSNMP := map[string]bool{}
	for _, k := range known {
		answeredSNMP[k.key()] = k.SNMP
		probe.ask(k.IP, k.SNMP)
	}
	found, err := discover(timeout)
	if err != nil {
		return nil, err
	}
	if up := stillUp(known, found, probe.answered); len(up) > 0 {
		for _, n := range up {
			vlog("%s answered SNMP but not mDNS, keeping it online", n.Name)
		}
		byKey := map[string]Nas{}
		for _, n := range append(found, up...) {
			mergeNas(byKey, n)
		}
		found = sortedByName(byKey)
	}
	if missing := missingSince(known, found); len(missing) > 0 {
		vlog("no answer from %s, looking again for %s", strings.Join(missing, ", "), recheckTimeout)
		again, _ := discover(recheckTimeout)
		byKey := map[string]Nas{}
		for _, n := range found {
			mergeNas(byKey, n)
		}
		for _, n := range again {
			mergeNas(byKey, n)
		}
		found = sortedByName(byKey)
	}
	for _, n := range found {
		probe.ask(n.IP, answeredSNMP[n.key()])
	}
	probe.apply(found)
	return remember(known, found, time.Now()), nil
}

// stillUp returns the remembered devices that answered the previous scan,
// are absent from found and have just answered an SNMP query: such a NAS
// is up and only its mDNS answer was lost, so it keeps its place in the
// list. answered(ip) is expected to wait for the query to finish.
func stillUp(known, found []Nas, answered func(ip string) bool) []Nas {
	present := map[string]bool{}
	for _, n := range found {
		present[n.key()] = true
	}
	var up []Nas
	for _, k := range known {
		if k.Online && !present[k.key()] && k.IP != "" && answered(k.IP) {
			up = append(up, k)
		}
	}
	return up
}

// missingSince names the remembered devices that answered the previous
// scan but are absent from found.
func missingSince(known, found []Nas) []string {
	present := map[string]bool{}
	for _, n := range found {
		present[n.key()] = true
	}
	var missing []string
	for _, k := range known {
		if k.Online && !present[k.key()] {
			missing = append(missing, k.Name)
		}
	}
	return missing
}

// discover runs the built-in mDNS resolver and, when installed,
// avahi-browse at the same time and merges their answers. On desktops
// where avahi-daemon owns the mDNS socket, unicast replies reach only
// avahi, so one path alone misses devices.
func discover(timeout time.Duration) ([]Nas, error) {
	type result struct {
		list []Nas
		err  error
	}
	builtin := make(chan result, 1)
	go func() {
		list, err := builtinScan(timeout)
		builtin <- result{list, err}
	}()
	avahiList, avahiRan := avahiScan(timeout)
	b := <-builtin

	if b.err != nil {
		vlog("built-in mDNS scan failed: %v", b.err)
	} else {
		vlog("built-in mDNS scan found %d device(s)", len(b.list))
	}
	if avahiRan {
		vlog("avahi-browse found %d device(s)", len(avahiList))
	}
	if b.err != nil && !avahiRan {
		return nil, b.err
	}

	byKey := map[string]Nas{}
	for _, n := range b.list {
		mergeNas(byKey, n)
	}
	for _, n := range avahiList {
		mergeNas(byKey, n)
	}
	return sortedByName(byKey), nil
}

func sortedByName(byKey map[string]Nas) []Nas {
	list := make([]Nas, 0, len(byKey))
	for _, n := range byKey {
		list = append(list, n)
	}
	sort.Slice(list, func(i, j int) bool {
		return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
	})
	return list
}

func builtinScan(timeout time.Duration) ([]Nas, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var opts []zeroconf.ClientOption
	if ifaceName != "" {
		ifc, err := net.InterfaceByName(ifaceName)
		if err != nil {
			return nil, fmt.Errorf("interface %q: %w", ifaceName, err)
		}
		opts = append(opts, zeroconf.SelectIfaces([]net.Interface{*ifc}))
	}

	var mu sync.Mutex
	found := map[string]Nas{}

	browse := func(service string, initialized bool) error {
		resolver, err := zeroconf.NewResolver(opts...)
		if err != nil {
			// The resolver refuses to start when it cannot join the IPv6
			// group, which is the case on hosts with IPv6 disabled. The
			// NAS answers over IPv4 anyway.
			vlog("mDNS over IPv4+IPv6 unavailable (%v), using IPv4 only", err)
			opts = append(opts, zeroconf.SelectIPTraffic(zeroconf.IPv4))
			resolver, err = zeroconf.NewResolver(opts...)
		}
		if err != nil {
			return err
		}
		entries := make(chan *zeroconf.ServiceEntry)
		go func() {
			for e := range entries {
				nas := Nas{
					Name:        e.Instance,
					Port:        e.Port,
					Initialized: initialized,
				}
				if len(e.AddrIPv4) > 0 {
					nas.IP = e.AddrIPv4[0].String()
				}
				if len(e.AddrIPv6) > 0 {
					nas.IPv6 = e.AddrIPv6[0].String()
				}
				nas.applyTxt(e.Text)
				if nas.State == "" {
					if initialized {
						nas.State = "Ready"
					} else {
						nas.State = "Uninitialized"
					}
				}
				vlog("mDNS answer: %s (%s) via %s", e.Instance, nas.IP, service)
				mu.Lock()
				mergeNas(found, nas)
				mu.Unlock()
			}
		}()
		return resolver.Browse(ctx, service, "local.", entries)
	}

	if err := browse("_ASUSTOR_ADM._tcp", true); err != nil {
		return nil, err
	}
	if err := browse("_ASUSTOR_INIT._tcp", false); err != nil {
		return nil, err
	}
	// A second query halfway through: on a segment with hundreds of
	// NAS, answers to a single query do get lost, and the resolver's
	// own retry would only come after the listen is over.
	time.AfterFunc(timeout/2, func() {
		if ctx.Err() != nil {
			return
		}
		for _, svc := range []struct {
			name string
			init bool
		}{{"_ASUSTOR_ADM._tcp", true}, {"_ASUSTOR_INIT._tcp", false}} {
			if err := browse(svc.name, svc.init); err != nil {
				vlog("second query for %s: %v", svc.name, err)
			}
		}
	})
	<-ctx.Done()

	mu.Lock()
	defer mu.Unlock()
	return sortedByName(found), nil
}

// findNas scans for target (a name, IP or MAC). A NAS that is not
// answering but is remembered from an earlier scan is returned with
// Online == false, so callers can tell the user when it was last seen.
func findNas(target string, timeout time.Duration) (*Nas, error) {
	list, err := scan(timeout)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].matches(target) {
			return &list[i], nil
		}
	}
	return nil, fmt.Errorf("no NAS matching %q found (try 'acc scan --all')", target)
}
