// acc is the CLI of Asustor Control Center for Linux, an unofficial Linux version of
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
	"math"
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
	fmt.Fprintln(tw, "NAME\tIP\tMODEL\tSERIAL\tMAC\tADM\tPORT\tSTATE\tUPTIME")
	for _, n := range list {
		state, port := n.State, ""
		if !n.Online {
			state = "Offline"
		}
		if n.Port != 0 {
			port = strconv.Itoa(n.Port)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			n.Name, n.IP, n.Model, n.SerialNumber, n.MAC, n.ADMVersion, port, state, formatUptime(n, now))
	}
	tw.Flush()
}

// formatUptime renders the UPTIME column: the uptime the NAS reported
// over SNMP, "-" for an online NAS that reported none, or when an offline
// NAS was last seen.
func formatUptime(n Nas, now time.Time) string {
	switch {
	case n.Online && n.UptimeSeconds != nil:
		return formatDuration(time.Duration(*n.UptimeSeconds) * time.Second)
	case n.Online:
		return "-"
	case n.LastSeen.IsZero():
		return "never seen"
	}
	return "last seen " + formatSeen(n.LastSeen, now)
}

// formatDuration renders d down to the minute ("3d 4h 12m"), or to the
// second under an hour ("12m 05s").
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	secs := int64(d / time.Second)
	days, hours, mins := secs/86400, secs/3600%24, secs/60%60
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", secs)
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", mins, secs%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %02dm", hours, mins)
	}
	return fmt.Sprintf("%dd %dh %02dm", days, hours, mins)
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

func printJSON(list []Nas, indent bool) error {
	if list == nil {
		list = []Nas{}
	}
	enc := json.NewEncoder(os.Stdout)
	if indent {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(list)
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
			if err == nil {
				err = printJSON(list, false)
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, "acc:", err)
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

// launchWait is how long a launcher gets to exit with an error before it
// counts as started (a file manager that is up keeps running).
const launchWait = 3 * time.Second

// fileManagers lists file managers that browse smb:// on their own, by
// desktop (an entry of XDG_CURRENT_DESKTOP). Most register no handler for
// the smb scheme, so xdg-open finds nothing although the file manager
// opens the address fine when started with it.
var fileManagers = []struct{ desktop, command string }{
	{"GNOME", "nautilus"}, {"ubuntu", "nautilus"}, {"Budgie", "nautilus"},
	{"Unity", "nautilus"}, {"KDE", "dolphin"}, {"XFCE", "thunar"},
	{"X-Cinnamon", "nemo"}, {"MATE", "caja"}, {"LXQt", "pcmanfm-qt"},
	{"LXDE", "pcmanfm"}, {"Pantheon", "io.elementary.files"},
	{"Deepin", "dde-file-manager"},
}

// installedFileManagers returns the file managers found on PATH, the
// running desktop's own first. installed is exec.LookPath in production.
func installedFileManagers(desktop string, installed func(string) (string, error)) []string {
	current := map[string]bool{}
	for _, d := range strings.Split(strings.ToLower(desktop), ":") {
		current[d] = true
	}
	var own, others []string
	seen := map[string]bool{}
	for _, fm := range fileManagers {
		if seen[fm.command] {
			continue
		}
		if _, err := installed(fm.command); err != nil {
			continue
		}
		seen[fm.command] = true
		if current[strings.ToLower(fm.desktop)] {
			own = append(own, fm.command)
		} else {
			others = append(others, fm.command)
		}
	}
	return append(own, others...)
}

// openURL hands url to the desktop: xdg-open first and, for smb://, the
// installed file managers after it. A launcher that exits with an error
// within launchWait has failed and the next one is tried. what names the
// kind of program expected, for the error message.
func openURL(url, what string) error {
	commands := [][]string{{"xdg-open", url}}
	if strings.HasPrefix(url, "smb://") {
		for _, fm := range installedFileManagers(os.Getenv("XDG_CURRENT_DESKTOP"), exec.LookPath) {
			commands = append(commands, []string{fm, url})
		}
	}
	var lastErr error
	for _, c := range commands {
		cmd := exec.Command(c[0], c[1:]...)
		if err := cmd.Start(); err != nil {
			lastErr = err
			continue
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err == nil {
				vlog("opened %s with %s", url, c[0])
				return nil
			}
			lastErr = fmt.Errorf("%s: %w", c[0], err)
			vlog("%v", lastErr)
		case <-time.After(launchWait):
			vlog("opened %s with %s", url, c[0])
			return nil // still running: it has a window up
		}
	}
	hint := ""
	if strings.HasPrefix(url, "smb://") {
		hint = "; install gvfs-smb (GNOME, Cinnamon, XFCE) or kio-extras (KDE)"
	}
	return fmt.Errorf("cannot start a %s (%v)%s; open %s yourself", what, lastErr, hint, url)
}

// maxSeconds bounds --timeout and --interval; beyond it the value would
// not fit a time.Duration anyway.
const maxSeconds = 1e6

// parseSeconds parses the value of a duration option given in seconds.
func parseSeconds(flag, s string) (time.Duration, error) {
	secs, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(secs) || secs <= 0 || secs > maxSeconds {
		return 0, fmt.Errorf("%s wants a positive number of seconds, not %q", flag, s)
	}
	return time.Duration(secs * float64(time.Second)), nil
}

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: acc <command> [options] [argument]

Commands:
  scan                   discover ASUSTOR NAS devices via mDNS
  open <name|ip|mac>     open the NAS web UI (ADM) in the browser
  url <name|ip|mac>      print the NAS web UI URL
  shares <name|ip|mac>   open the shared folders (smb://<ip>/) in the file manager
  ports <name|ip|mac>    which services of the NAS answer (ADM, SMB, SSH, NFS, ...):
                         a TCP connect to each well-known port, nothing more
  wol <name|ip|mac>      send a Wake-on-LAN magic packet; works for a
                         sleeping NAS that acc has seen before
  forget <name|ip|mac>   drop a NAS from acc's device memory (--all: every one)
  doctor                 diagnose why discovery finds nothing
  version                print the acc version

Options:
  --json                 machine-readable output (scan, ports)
  --all                  also list remembered devices that did not answer (scan)
  --watch                rescan continuously and redraw the table (scan)
  --interval <seconds>   rescan interval for --watch (default 30)
  --timeout <seconds>    how long to listen for answers (default 4)
  --iface <name>         restrict scanning and Wake-on-LAN to one interface
  --community <string>   SNMP read community (default "public", or $ACC_SNMP_COMMUNITY)
  --no-snmp              do not ask the devices for their uptime
  --verbose, -v          explain what is going on, on stderr

The UPTIME column is what the NAS reports over SNMP, which has to be
enabled in ADM > Services > SNMP (v1/v2c with the read community given
above); without it the column shows "-". Offline devices show when they
were last seen.
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
	snmpCommunity = defaultCommunity()
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "acc:", err)
		os.Exit(1)
	}
	seconds := func(flag, s string) time.Duration {
		d, err := parseSeconds(flag, s)
		if err != nil {
			fail(err)
		}
		return d
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
		case "--no-snmp":
			snmpDisabled = true
		case "--verbose", "-v":
			verbose = true
		case "--iface", "--timeout", "--interval", "--community":
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
			case "--community":
				snmpCommunity = args[i+1]
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
			if err := printJSON(list, true); err != nil {
				fail(err)
			}
		} else {
			printTable(list, time.Now())
		}
	case "open", "url", "shares":
		if len(args) != 1 {
			usage()
		}
		nas, err := findNas(args[0], timeout)
		if err != nil {
			fail(err)
		}
		url, what := nas.URL, "browser"
		if cmd == "shares" {
			url, what = nas.SharesURL, "file manager"
		}
		if url == "" && cmd == "shares" {
			fail(fmt.Errorf("%s has no IPv4 address; shared folders need one", nas.Name))
		}
		if url == "" {
			fail(fmt.Errorf("%s did not report an address", nas.Name))
		}
		if cmd == "url" {
			fmt.Println(url)
			return
		}
		if !nas.Online {
			fail(fmt.Errorf("%s is not answering (last seen %s); wake it with 'acc wol %s'",
				nas.Name, formatSeen(nas.LastSeen, time.Now()), nas.Name))
		}
		if err := openURL(url, what); err != nil {
			fail(err)
		}
	case "ports":
		if len(args) != 1 {
			usage()
		}
		host, name := args[0], args[0]
		if net.ParseIP(host) == nil {
			// A name or MAC: the device memory knows the address without
			// a scan; a NAS never seen before needs one.
			var found bool
			for _, n := range loadMemory() {
				if n.matches(host) && n.IP != "" {
					host, name, found = n.IP, n.Name, true
					break
				}
			}
			if !found {
				nas, err := findNas(host, timeout)
				if err != nil {
					fail(err)
				}
				if nas.IP == "" {
					fail(fmt.Errorf("%s has no IPv4 address", nas.Name))
				}
				host, name = nas.IP, nas.Name
			}
		}
		results := checkPorts(host, knownPorts, portTimeout)
		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(results); err != nil {
				fail(err)
			}
			return
		}
		fmt.Printf("%s (%s):\n", name, host)
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "PORT\tSERVICE\tSTATE")
		open := 0
		for _, r := range results {
			if r.State == "open" {
				open++
			}
			fmt.Fprintf(w, "%d\t%s\t%s\n", r.Port, r.Service, r.State)
		}
		w.Flush()
		fmt.Printf("%d of %d services answer. closed: nothing listens; filtered: no answer (firewall).\n",
			open, len(results))
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
