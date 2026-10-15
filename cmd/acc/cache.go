package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Device memory: acc keeps every NAS it has seen in
// $XDG_CACHE_HOME/asustor-acc/devices.json. That is what lets
// `acc wol <name>` wake a NAS that is asleep (its MAC is remembered), lets
// `acc scan --all` and the GUI list devices that are currently offline
// (with when they were last seen), and is what lets a scan ask remembered
// devices for their uptime while discovery is still listening.

// memoryMaxAge is how long a device that no longer answers stays in the
// memory.
const memoryMaxAge = 180 * 24 * time.Hour

func memoryPath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "asustor-acc", "devices.json")
}

func loadMemory() []Nas {
	path := memoryPath()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var list []Nas
	if err := json.Unmarshal(data, &list); err != nil {
		vlog("ignoring unreadable device memory %s: %v", path, err)
		return nil
	}
	return list
}

func saveMemory(list []Nas) {
	path := memoryPath()
	if path == "" {
		return
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return
	}
	// Write-then-rename so a crash never leaves a truncated file behind.
	tmp := path + ".tmp"
	err = os.MkdirAll(filepath.Dir(path), 0o755)
	if err == nil {
		err = os.WriteFile(tmp, data, 0o644)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		vlog("cannot save device memory: %v", err)
		os.Remove(tmp)
	}
}

// remember merges the devices found by a scan into the device memory and
// returns the complete list: found devices (online) followed by remembered
// ones that did not answer (offline). An uptime is only ever reported for
// a NAS that answered; the SNMP marker survives a scan without one.
func remember(known, found []Nas, now time.Time) []Nas {
	now = now.Truncate(time.Second)
	prev := map[string]Nas{}
	for _, k := range known {
		prev[k.key()] = k
	}

	all := make([]Nas, 0, len(found)+len(prev))
	for _, n := range found {
		n.Online, n.LastSeen = true, now
		if p, ok := prev[n.key()]; ok && p.SNMP {
			n.SNMP = true
		}
		n.URL, n.SharesURL = n.webURL(), n.sharesURL()
		delete(prev, n.key())
		all = append(all, n)
	}

	var offline []Nas
	for _, p := range prev {
		if p.LastSeen.IsZero() || now.Sub(p.LastSeen) > memoryMaxAge {
			continue
		}
		p.Online = false
		p.UptimeSeconds, p.UptimeSource, p.RTTms = nil, "", nil
		p.URL, p.SharesURL = p.webURL(), p.sharesURL()
		offline = append(offline, p)
	}
	sort.Slice(offline, func(i, j int) bool {
		return strings.ToLower(offline[i].Name) < strings.ToLower(offline[j].Name)
	})
	all = append(all, offline...)

	saveMemory(all)
	return all
}

// forget drops the devices matching target ("" drops all) from the device
// memory and reports how many were removed.
func forget(target string) int {
	known := loadMemory()
	kept := known[:0]
	for _, n := range known {
		if target == "" || n.matches(target) {
			continue
		}
		kept = append(kept, n)
	}
	saveMemory(kept)
	return len(known) - len(kept)
}
