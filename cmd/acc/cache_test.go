package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRememberAndForget(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	now := time.Date(2026, 10, 14, 12, 0, 0, 500e6, time.UTC)
	uptime := int64(1234)
	nimbus := Nas{Name: "Nimbustor", IP: "192.168.1.20", Port: 8000, MAC: "00:11:32:12:34:56", Initialized: true,
		UptimeSeconds: &uptime, UptimeSource: "snmp", SNMP: true}
	locker := Nas{Name: "Lockerstor", IP: "192.168.1.21", IPv6: "fe80::21", Port: 8000, MAC: "00:11:32:00:00:01", Initialized: true,
		UptimeSeconds: &uptime, UptimeSource: "snmp", SNMP: true}

	// First scan: both answer.
	list := remember(nil, []Nas{nimbus, locker}, now)
	if len(list) != 2 {
		t.Fatalf("got %d devices, want 2", len(list))
	}
	for _, n := range list {
		if !n.Online || !n.LastSeen.Equal(now.Truncate(time.Second)) || n.URL != "http://"+n.IP+":8000/" || n.SharesURL != "smb://"+n.IP+"/" {
			t.Errorf("online record: %+v", n)
		}
		if n.UptimeSeconds == nil || *n.UptimeSeconds != 1234 || n.UptimeSource != "snmp" || !n.SNMP {
			t.Errorf("uptime lost: %+v", n)
		}
	}
	if _, err := os.Stat(memoryPath()); err != nil {
		t.Fatalf("device memory not written: %v", err)
	}
	mem := loadMemory()
	if len(mem) != 2 || mem[0].Name != "Nimbustor" || mem[0].UptimeSeconds == nil || *mem[0].UptimeSeconds != 1234 || !mem[1].SNMP {
		t.Fatalf("device memory on disk: %+v", mem)
	}

	// Second scan an hour later: Nimbustor answers but not SNMP this
	// time, Lockerstor is missing.
	later := now.Add(time.Hour).Truncate(time.Second)
	again := Nas{Name: "Nimbustor", IP: "192.168.1.20", Port: 8000, MAC: "00:11:32:12:34:56", Initialized: true}
	list = remember(loadMemory(), []Nas{again}, later)
	if len(list) != 2 || list[0].Name != "Nimbustor" || list[1].Name != "Lockerstor" {
		t.Fatalf("got %v", list)
	}
	on, off := list[0], list[1]
	if !on.Online || on.UptimeSeconds != nil || on.UptimeSource != "" || !on.SNMP || !on.LastSeen.Equal(later) {
		t.Errorf("online without an SNMP answer: %+v", on)
	}
	if off.Online || off.UptimeSeconds != nil || off.UptimeSource != "" || !off.SNMP ||
		!off.LastSeen.Equal(now.Truncate(time.Second)) || off.URL != "http://192.168.1.21:8000/" || off.SharesURL != "smb://192.168.1.21/" {
		t.Errorf("offline record: %+v", off)
	}

	// A device that has not answered for longer than memoryMaxAge is
	// dropped; one without LastSeen (hand-edited memory) as well.
	stale := Nas{Name: "Old", MAC: "00:11:32:00:00:09", LastSeen: now.Add(-memoryMaxAge - time.Hour)}
	blank := Nas{Name: "Blank", MAC: "00:11:32:00:00:08"}
	recent := Nas{Name: "Recent", MAC: "00:11:32:00:00:07", LastSeen: now.Add(-memoryMaxAge + time.Hour)}
	if list = remember([]Nas{stale, blank, recent}, nil, now); len(list) != 1 || list[0].Name != "Recent" {
		t.Errorf("after ageing: %v", list)
	}

	// Offline devices come sorted by name after the online ones.
	list = remember([]Nas{{Name: "zeta", MAC: "00:00:00:00:00:01", LastSeen: now}, {Name: "Alpha", MAC: "00:00:00:00:00:02", LastSeen: now}}, []Nas{nimbus}, now)
	if len(list) != 3 || list[0].Name != "Nimbustor" || list[1].Name != "Alpha" || list[2].Name != "zeta" {
		t.Errorf("order: %v", list)
	}

	// forget by name, address and MAC, then everything.
	saveMemory(list)
	for _, tc := range []struct {
		target string
		want   int
	}{{"nobody", 0}, {"ALPHA", 1}, {"192.168.1.20", 1}, {"00-00-00-00-00-01", 1}, {"", 0}} {
		if got := forget(tc.target); got != tc.want {
			t.Errorf("forget(%q) = %d, want %d", tc.target, got, tc.want)
		}
	}
	saveMemory(list)
	if got := forget(""); got != 3 || len(loadMemory()) != 0 {
		t.Errorf("forget everything = %d, memory %v", got, loadMemory())
	}
}

func TestLoadMemoryTolerates(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if m := loadMemory(); m != nil {
		t.Errorf("missing file gave %v", m)
	}
	path := memoryPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, junk := range []string{"{not json", "", "null", `{"name":"not a list"}`} {
		if err := os.WriteFile(path, []byte(junk), 0o644); err != nil {
			t.Fatal(err)
		}
		if m := loadMemory(); m != nil {
			t.Errorf("%q gave %v", junk, m)
		}
	}
	// A scan still works and replaces the junk.
	list := remember(loadMemory(), []Nas{{Name: "N", IP: "10.0.0.2", MAC: "00:11:32:00:00:01"}}, time.Now())
	if len(list) != 1 || len(loadMemory()) != 1 {
		t.Errorf("memory not rebuilt: %v / %v", list, loadMemory())
	}
	// Memory in a legacy layout (the former online_since field) loads.
	legacy := `[{"name":"Old","ip":"10.0.0.3","port":8000,"mac":"00:11:32:00:00:02","online":true,"online_since":"2026-01-01T00:00:00Z","last_seen":"2026-01-01T00:00:00Z"}]`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	if m := loadMemory(); len(m) != 1 || m[0].Name != "Old" || m[0].UptimeSeconds != nil {
		t.Errorf("legacy memory: %v", m)
	}
}
