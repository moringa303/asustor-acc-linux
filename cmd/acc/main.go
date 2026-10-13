// acc is the CLI of Asustor ACC for Linux, an unofficial Linux version of
// the ASUSTOR Control Center.
//
// ASUSTOR NAS devices announce themselves via mDNS/DNS-SD:
//
//	_ASUSTOR_ADM._tcp.local   (initialized NAS running ADM)
//	_ASUSTOR_INIT._tcp.local  (factory-fresh NAS awaiting initialization)
//
// The SRV record carries the ADM web port; TXT records carry model, ADM
// version, serial number, hostid (MAC, dash-separated), https port, etc.
// (Protocol determined from ACC 2.1.8's NasScanManagerA.dll.)
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// version is set at build time: -ldflags "-X main.version=1.3.0".
var version = "dev"

var (
	verbose   bool
	ifaceName string
)

func vlog(format string, args ...any) {
	if verbose {
		fmt.Fprintf(os.Stderr, "acc: "+format+"\n", args...)
	}
}

func printTable(list []Nas, now time.Time) {
	if len(list) == 0 {
		fmt.Println("No ASUSTOR NAS devices found.")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tIP\tMODEL\tSERIAL\tMAC\tADM\tPORT\tSTATE\tONLINE")
	for _, n := range list {
		state, port := n.State, ""
		if !n.Online {
			state = "Offline"
		}
		if n.Port != 0 {
			port = strconv.Itoa(n.Port)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			n.Name, n.IP, n.Model, n.SerialNumber, n.MAC, n.ADMVersion, port, state, formatOnline(n, now))
	}
	tw.Flush()
}

// formatOnline renders the ONLINE column: for how long the NAS has
// answered every scan, or when it was last seen.
func formatOnline(n Nas, now time.Time) string {
	if n.Online {
		return formatDuration(now.Sub(n.OnlineSince))
	}
	if n.LastSeen.IsZero() {
		return "never seen"
	}
	return "last seen " + formatSeen(n.LastSeen, now)
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
}

func formatSeen(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04")
	}
	return t.Format("2 Jan 15:04")
}

func onlineOnly(list []Nas) []Nas {
	out := make([]Nas, 0, len(list))
	for _, n := range list {
		if n.Online {
			out = append(out, n)
		}
	}
	return out
}

func printJSON(list []Nas) {
	if list == nil {
		list = []Nas{}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(list)
}

// watchScan rescans every interval and redraws the table in place.
func watchScan(timeout, interval time.Duration, all, asJSON bool) {
	for {
		list, err := scan(timeout)
		if !all {
			list = onlineOnly(list)
		}
		now := time.Now()
		if asJSON {
			if err != nil {
				fmt.Fprintln(os.Stderr, "acc:", err)
			} else {
				json.NewEncoder(os.Stdout).Encode(list)
			}
		} else {
			fmt.Print("\033[H\033[2J")
			fmt.Printf("acc scan --watch   every %s   last scan %s   (Ctrl-C to stop)\n\n",
				interval, now.Format("15:04:05"))
			if err != nil {
				fmt.Println("scan failed:", err)
			} else {
				printTable(list, now)
			}
		}
		time.Sleep(interval)
	}
}

func openBrowser(url string) error {
	if err := exec.Command("xdg-open", url).Start(); err != nil {
		return fmt.Errorf("cannot start a browser (%w); open %s yourself", err, url)
	}
	return nil
}

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: acc <command> [options] [argument]

Commands:
  scan                   discover ASUSTOR NAS devices via mDNS
  open <name|ip|mac>     open the NAS web UI (ADM) in the browser
  url <name|ip|mac>      print the NAS web UI URL
  wol <name|ip|mac>      send a Wake-on-LAN magic packet; works for a
                         sleeping NAS that acc has seen before
  forget <name|ip|mac>   drop a NAS from acc's device memory (--all: every one)
  doctor                 diagnose why discovery finds nothing
  version                print the acc version

Options:
  --json                 machine-readable output (scan)
  --all                  also list remembered devices that did not answer (scan)
  --watch                rescan continuously and redraw the table (scan)
  --interval <seconds>   rescan interval for --watch (default 30)
  --timeout <seconds>    how long to listen for answers (default 4)
  --iface <name>         restrict scanning and Wake-on-LAN to one interface
  --verbose, -v          explain what is going on, on stderr

The ONLINE column is for how long the NAS has answered every scan without
a miss, as observed by acc; ADM does not announce its uptime.
`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var (
		timeout    = 4 * time.Second
		interval   = 30 * time.Second
		timeoutSet bool
		asJSON     bool
		all        bool
		watch      bool
	)
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "acc:", err)
		os.Exit(1)
	}
	seconds := func(flag, s string) time.Duration {
		secs, err := strconv.ParseFloat(s, 64)
		if err != nil || secs <= 0 {
			fail(fmt.Errorf("%s wants a positive number of seconds, not %q", flag, s))
		}
		return time.Duration(secs * float64(time.Second))
	}
	for i := 0; i < len(args); i++ {
		consumed := 1
		switch args[i] {
		case "--json":
			asJSON = true
		case "--all":
			all = true
		case "--watch":
			watch = true
		case "--verbose", "-v":
			verbose = true
		case "--iface", "--timeout", "--interval":
			if i+1 >= len(args) {
				usage()
			}
			consumed = 2
			switch args[i] {
			case "--iface":
				ifaceName = args[i+1]
				if _, err := net.InterfaceByName(ifaceName); err != nil {
					fail(fmt.Errorf("interface %q: %w", ifaceName, err))
				}
			case "--timeout":
				timeout, timeoutSet = seconds("--timeout", args[i+1]), true
			case "--interval":
				interval = seconds("--interval", args[i+1])
			}
		case "--help", "-h":
			usage()
		default:
			if strings.HasPrefix(args[i], "-") {
				fail(fmt.Errorf("unknown option %s", args[i]))
			}
			consumed = 0
		}
		if consumed > 0 {
			args = append(args[:i], args[i+consumed:]...)
			i--
		}
	}

	switch cmd {
	case "version", "--version", "-V":
		fmt.Println("acc", version)
	case "doctor":
		if !timeoutSet {
			timeout = 5 * time.Second
		}
		doctor(timeout)
	case "scan":
		if watch {
			watchScan(timeout, interval, all, asJSON)
		}
		list, err := scan(timeout)
		if err != nil {
			fail(err)
		}
		if !all {
			list = onlineOnly(list)
		}
		if asJSON {
			printJSON(list)
		} else {
			printTable(list, time.Now())
		}
	case "open", "url":
		if len(args) != 1 {
			usage()
		}
		nas, err := findNas(args[0], timeout)
		if err != nil {
			fail(err)
		}
		if nas.URL == "" {
			fail(fmt.Errorf("%s did not report an address", nas.Name))
		}
		if cmd == "url" {
			fmt.Println(nas.URL)
			return
		}
		if !nas.Online {
			fail(fmt.Errorf("%s is not answering (last seen %s); wake it with 'acc wol %s'",
				nas.Name, formatSeen(nas.LastSeen, time.Now()), nas.Name))
		}
		if err := openBrowser(nas.URL); err != nil {
			fail(err)
		}
	case "wol":
		if len(args) != 1 {
			usage()
		}
		target := args[0]
		var mac, ip, name string
		// The device memory answers without a scan: a NAS that is asleep
		// cannot be discovered, which is the whole point of waking it.
		for _, n := range loadMemory() {
			if n.matches(target) {
				mac, ip, name = n.MAC, n.IP, n.Name
				break
			}
		}
		if hw, err := net.ParseMAC(target); err == nil {
			mac = hw.String()
		} else if mac == "" {
			nas, err := findNas(target, timeout)
			if err != nil {
				fail(err)
			}
			mac, ip, name = nas.MAC, nas.IP, nas.Name
		}
		if mac == "" {
			fail(fmt.Errorf("%s did not report a MAC address", name))
		}
		sent, err := sendWOL(mac, ip)
		if err != nil {
			fail(err)
		}
		if name != "" {
			fmt.Printf("Magic packet sent to %s (%s) via %s\n", name, strings.ToUpper(mac), strings.Join(sent, ", "))
		} else {
			fmt.Printf("Magic packet sent to %s via %s\n", strings.ToUpper(mac), strings.Join(sent, ", "))
		}
	case "forget":
		target := ""
		switch {
		case all && len(args) == 0:
		case !all && len(args) == 1:
			target = args[0]
		default:
			usage()
		}
		n := forget(target)
		fmt.Printf("Forgot %d device(s).\n", n)
	default:
		usage()
	}
}
