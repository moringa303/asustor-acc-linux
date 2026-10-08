// acc is the CLI of Asustor ACC for Linux, an unofficial Linux version of
// the ASUSTOR Control Center.
//
// ASUSTOR NAS devices announce themselves via mDNS/DNS-SD:
//   _ASUSTOR_ADM._tcp.local   (initialized NAS running ADM)
//   _ASUSTOR_INIT._tcp.local  (factory-fresh NAS awaiting initialization)
// The SRV record carries the ADM web port; TXT records carry model, ADM
// version, serial number, hostid (MAC, dash-separated), https port, etc.
// (Protocol determined from ACC 2.1.8's NasScanManagerA.dll.)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
)

type Nas struct {
	Name         string `json:"name"`
	IP           string `json:"ip"`
	IPv6         string `json:"ipv6,omitempty"`
	Port         int    `json:"port"`
	HTTPSPort    string `json:"https_port,omitempty"`
	HTTPEnabled  string `json:"http_enabled,omitempty"`
	Model        string `json:"model,omitempty"`
	ADMVersion   string `json:"adm_version,omitempty"`
	SerialNumber string `json:"serial_number,omitempty"`
	MAC          string `json:"mac,omitempty"`
	State        string `json:"state,omitempty"`
	WOL          string `json:"wol,omitempty"`
	ADMUpdate    string `json:"adm_update,omitempty"`
	Initialized  bool   `json:"initialized"`
}

func (n *Nas) applyTxt(txt []string) {
	for _, kv := range txt {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch strings.ToLower(k) {
		case "model":
			n.Model = v
		case "version":
			n.ADMVersion = v
		case "serialnumber":
			n.SerialNumber = v
		case "hostid":
			n.MAC = strings.ToUpper(strings.ReplaceAll(v, "-", ":"))
		case "state":
			n.State = displayState(v)
		case "wol":
			n.WOL = v
		case "httpsport":
			n.HTTPSPort = v
		case "httpenabled":
			n.HTTPEnabled = v
		case "admupdate":
			n.ADMUpdate = v
		}
	}
}

// displayState maps the wire value of the state TXT record to a display
// name: inited means the NAS runs ADM (Ready), uninited means factory-fresh
// (Uninitialized), anything else means the NAS is up but ADM is not usable
// yet (Not ready).
func displayState(v string) string {
	switch strings.ToLower(v) {
	case "inited":
		return "Ready"
	case "uninited":
		return "Uninitialized"
	case "":
		return ""
	}
	return "Not ready"
}

func (n *Nas) url() string {
	host := n.IP
	if host == "" && n.IPv6 != "" {
		host = "[" + n.IPv6 + "]"
	}
	// An uninitialized NAS serves its setup wizard over plain HTTP,
	// normally on port 8000.
	if !n.Initialized {
		port := n.Port
		if port == 0 {
			port = 8000
		}
		return fmt.Sprintf("http://%s:%d/", host, port)
	}
	// Prefer plain HTTP on the SRV port unless it is disabled.
	if strings.EqualFold(n.HTTPEnabled, "No") && n.HTTPSPort != "" {
		return fmt.Sprintf("https://%s:%s/", host, n.HTTPSPort)
	}
	return fmt.Sprintf("http://%s:%d/", host, n.Port)
}

var (
	verbose   bool
	ifaceName string
)

func vlog(format string, args ...any) {
	if verbose {
		fmt.Fprintf(os.Stderr, "acc: "+format+"\n", args...)
	}
}

func scan(timeout time.Duration) ([]Nas, error) {
	list, err := builtinScan(timeout)
	if err != nil {
		vlog("built-in mDNS scan failed: %v", err)
	}
	vlog("built-in mDNS scan found %d device(s)", len(list))
	if len(list) == 0 {
		if avahiList, ok := avahiScan(timeout); ok {
			vlog("avahi-browse scan found %d device(s)", len(avahiList))
			if len(avahiList) > 0 {
				sort.Slice(avahiList, func(i, j int) bool { return avahiList[i].Name < avahiList[j].Name })
				return avahiList, nil
			}
		} else {
			vlog("avahi-browse not available, no fallback")
		}
	}
	return list, err
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
				// Keyed by instance name; ADM entry wins over INIT entry.
				if prev, dup := found[e.Instance]; !dup || (initialized && !prev.Initialized) {
					found[e.Instance] = nas
				}
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
	<-ctx.Done()

	mu.Lock()
	defer mu.Unlock()
	list := make([]Nas, 0, len(found))
	for _, n := range found {
		list = append(list, n)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, nil
}

func findNas(target string, timeout time.Duration) (*Nas, error) {
	list, err := scan(timeout)
	if err != nil {
		return nil, err
	}
	for i := range list {
		n := &list[i]
		if strings.EqualFold(n.Name, target) || n.IP == target || strings.EqualFold(n.MAC, target) {
			return n, nil
		}
	}
	return nil, fmt.Errorf("no NAS matching %q found (try 'acc scan')", target)
}

func sendWOL(mac string) error {
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return fmt.Errorf("invalid MAC %q: %w", mac, err)
	}
	packet := make([]byte, 0, 102)
	packet = append(packet, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF)
	for i := 0; i < 16; i++ {
		packet = append(packet, hw...)
	}
	conn, err := net.Dial("udp", "255.255.255.255:9")
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write(packet)
	return err
}

func printTable(list []Nas) {
	if len(list) == 0 {
		fmt.Println("No ASUSTOR NAS devices found.")
		return
	}
	w := func(cols ...string) {
		fmt.Printf("%-20s %-16s %-12s %-12s %-18s %-10s %-6s %s\n",
			cols[0], cols[1], cols[2], cols[3], cols[4], cols[5], cols[6], cols[7])
	}
	w("NAME", "IP", "MODEL", "SERIAL", "MAC", "ADM", "PORT", "STATE")
	for _, n := range list {
		w(n.Name, n.IP, n.Model, n.SerialNumber, n.MAC, n.ADMVersion, fmt.Sprint(n.Port), n.State)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: acc <command> [args]

Commands:
  scan [--json] [--timeout <seconds>]   discover ASUSTOR NAS devices via mDNS
  open <name|ip>                        open the NAS web UI (ADM) in the browser
  url <name|ip>                         print the NAS web UI URL
  wol <name|ip|mac>                     send a Wake-on-LAN magic packet
  doctor                                diagnose why discovery finds nothing

Options:
  --iface <name>    restrict scanning to one network interface
  --verbose, -v     explain what the scan is doing on stderr
`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	timeout := 4 * time.Second
	args := os.Args[2:]
	asJSON := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			asJSON = true
			args = append(args[:i], args[i+1:]...)
			i--
		case "--verbose", "-v":
			verbose = true
			args = append(args[:i], args[i+1:]...)
			i--
		case "--iface":
			if i+1 >= len(args) {
				usage()
			}
			ifaceName = args[i+1]
			args = append(args[:i], args[i+2:]...)
			i--
		case "--timeout":
			if i+1 >= len(args) {
				usage()
			}
			var secs float64
			fmt.Sscanf(args[i+1], "%g", &secs)
			if secs > 0 {
				timeout = time.Duration(secs * float64(time.Second))
			}
			args = append(args[:i], args[i+2:]...)
			i--
		}
	}

	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "acc:", err)
		os.Exit(1)
	}

	switch os.Args[1] {
	case "doctor":
		if timeout == 4*time.Second {
			timeout = 5 * time.Second
		}
		doctor(timeout)
	case "scan":
		list, err := scan(timeout)
		if err != nil {
			fail(err)
		}
		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			enc.Encode(list)
		} else {
			printTable(list)
		}
	case "open", "url":
		if len(args) != 1 {
			usage()
		}
		nas, err := findNas(args[0], timeout)
		if err != nil {
			fail(err)
		}
		if os.Args[1] == "url" {
			fmt.Println(nas.url())
			return
		}
		if err := exec.Command("xdg-open", nas.url()).Start(); err != nil {
			fail(err)
		}
	case "wol":
		if len(args) != 1 {
			usage()
		}
		mac := args[0]
		if _, err := net.ParseMAC(mac); err != nil {
			nas, err := findNas(args[0], timeout)
			if err != nil {
				fail(err)
			}
			if nas.MAC == "" {
				fail(fmt.Errorf("NAS %q did not report a MAC address", nas.Name))
			}
			mac = nas.MAC
		}
		if err := sendWOL(mac); err != nil {
			fail(err)
		}
		fmt.Println("Magic packet sent to", mac)
	default:
		usage()
	}
}
