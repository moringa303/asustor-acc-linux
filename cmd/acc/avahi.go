package main

import (
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// avahiScan discovers NAS devices through the system's avahi-daemon via
// avahi-browse. On desktops where avahi owns the mDNS socket, this is more
// reliable than a standalone mDNS listener.
func avahiScan(timeout time.Duration) ([]Nas, bool) {
	if _, err := exec.LookPath("avahi-browse"); err != nil {
		return nil, false
	}
	found := map[string]Nas{}
	ok := false
	for _, svc := range []struct {
		name        string
		initialized bool
	}{{"_ASUSTOR_ADM._tcp", true}, {"_ASUSTOR_INIT._tcp", false}} {
		cmd := exec.Command("avahi-browse", "--parsable", "--resolve", "--terminate", svc.name)
		done := make(chan struct{})
		var out []byte
		go func() { out, _ = cmd.Output(); close(done) }()
		select {
		case <-done:
			ok = true
		case <-time.After(timeout + 2*time.Second):
			cmd.Process.Kill()
			<-done
		}
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Split(line, ";")
			// =;iface;proto;instance;type;domain;hostname;address;port;"k=v" "k=v"
			if len(f) < 10 || f[0] != "=" {
				continue
			}
			nas := Nas{
				Name:        avahiUnescape(f[3]),
				Initialized: svc.initialized,
			}
			if strings.Contains(f[7], ":") {
				nas.IPv6 = f[7]
			} else {
				nas.IP = f[7]
			}
			nas.Port, _ = strconv.Atoi(f[8])
			nas.applyTxt(parseAvahiTxt(strings.Join(f[9:], ";")))
			if !svc.initialized && nas.State == "" {
				nas.State = "uninited"
			}
			if prev, dup := found[nas.Name]; !dup ||
				(nas.IP != "" && prev.IP == "") ||
				(svc.initialized && !prev.Initialized) {
				if dup && nas.IP == "" {
					nas.IP = prev.IP
				}
				if dup && nas.IPv6 == "" {
					nas.IPv6 = prev.IPv6
				}
				found[nas.Name] = nas
			}
		}
	}
	list := make([]Nas, 0, len(found))
	for _, n := range found {
		list = append(list, n)
	}
	return list, ok
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
