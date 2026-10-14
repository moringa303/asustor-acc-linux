package main

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// avahiScan discovers NAS devices through the system's avahi-daemon via
// avahi-browse. On desktops where avahi owns the mDNS socket, this sees
// unicast replies that a standalone mDNS listener never receives. The
// second result is false when avahi-browse is not installed or could not
// talk to the daemon.
func avahiScan(timeout time.Duration) ([]Nas, bool) {
	if _, err := exec.LookPath("avahi-browse"); err != nil {
		vlog("avahi-browse not installed")
		return nil, false
	}
	var (
		mu    sync.Mutex
		found = map[string]Nas{}
		ran   bool
		wg    sync.WaitGroup
	)
	for _, svc := range []struct {
		name        string
		initialized bool
	}{{"_ASUSTOR_ADM._tcp", true}, {"_ASUSTOR_INIT._tcp", false}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// --terminate ends after avahi's "all for now"; the deadline
			// only guards against a wedged daemon.
			ctx, cancel := context.WithTimeout(context.Background(), timeout+2*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, "avahi-browse",
				"--parsable", "--resolve", "--terminate", svc.name).Output()
			mu.Lock()
			defer mu.Unlock()
			if err != nil && len(out) == 0 {
				vlog("avahi-browse %s: %v", svc.name, err)
				return
			}
			ran = true
			for _, line := range strings.Split(string(out), "\n") {
				nas, iface, ok := parseAvahiLine(line, svc.initialized)
				if !ok || (ifaceName != "" && iface != ifaceName) {
					continue
				}
				vlog("avahi answer: %s (%s) via %s", nas.Name, nas.IP, svc.name)
				mergeNas(found, nas)
			}
		}()
	}
	wg.Wait()
	return sortedByName(found), ran
}

// parseAvahiLine turns one resolved line of `avahi-browse --parsable`
// output into a NAS record and names the interface it was seen on. Other
// lines (browse events, "all for now") yield ok == false.
func parseAvahiLine(line string, initialized bool) (nas Nas, iface string, ok bool) {
	f := strings.Split(line, ";")
	// =;iface;proto;instance;type;domain;hostname;address;port;"k=v" "k=v"
	if len(f) < 10 || f[0] != "=" {
		return Nas{}, "", false
	}
	nas = Nas{
		Name:        avahiUnescape(f[3]),
		Initialized: initialized,
	}
	if strings.Contains(f[7], ":") {
		nas.IPv6 = f[7]
	} else {
		nas.IP = f[7]
	}
	if port, err := strconv.Atoi(f[8]); err == nil && port > 0 && port <= 65535 {
		nas.Port = port
	}
	// TXT values may themselves contain the field separator.
	nas.applyTxt(parseAvahiTxt(strings.Join(f[9:], ";")))
	if nas.State == "" {
		if initialized {
			nas.State = "Ready"
		} else {
			nas.State = "Uninitialized"
		}
	}
	return nas, f[1], true
}

// avahiUnescape decodes avahi's \ddd decimal escapes (e.g. \032 = space).
func avahiUnescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) &&
			s[i+1] >= '0' && s[i+1] <= '9' &&
			s[i+2] >= '0' && s[i+2] <= '9' &&
			s[i+3] >= '0' && s[i+3] <= '9' {
			n, _ := strconv.Atoi(s[i+1 : i+4])
			b.WriteByte(byte(n))
			i += 3
		} else if s[i] == '\\' && i+1 < len(s) {
			b.WriteByte(s[i+1])
			i++
		} else {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// parseAvahiTxt splits `"k=v" "k2=v2"` into []string{"k=v", "k2=v2"}.
func parseAvahiTxt(s string) []string {
	var txt []string
	for {
		start := strings.IndexByte(s, '"')
		if start < 0 {
			break
		}
		s = s[start+1:]
		end := strings.IndexByte(s, '"')
		if end < 0 {
			break
		}
		txt = append(txt, s[:end])
		s = s[end+1:]
	}
	return txt
}
