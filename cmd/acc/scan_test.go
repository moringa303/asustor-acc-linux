package main

import "testing"

func TestStillUp(t *testing.T) {
	known := []Nas{
		{Name: "lost", MAC: "aa", IP: "10.0.0.1", Online: true},  // SNMP answers: kept
		{Name: "quiet", MAC: "bb", IP: "10.0.0.2", Online: true}, // no SNMP: not kept
		{Name: "gone", MAC: "cc", IP: "10.0.0.3", Online: false}, // offline before: not kept
		{Name: "found", MAC: "dd", IP: "10.0.0.4", Online: true}, // answered mDNS: not duplicated
		{Name: "noip", MAC: "ee", IP: "", Online: true},          // nothing to ask
	}
	found := []Nas{{Name: "found", MAC: "dd", IP: "10.0.0.4"}}
	var asked []string
	answered := func(ip string) bool {
		asked = append(asked, ip)
		return ip != "10.0.0.2"
	}
	up := stillUp(known, found, answered)
	if len(up) != 1 || up[0].Name != "lost" {
		t.Fatalf("stillUp = %+v, want only lost", up)
	}
	if len(asked) != 2 {
		t.Errorf("asked %v, want only the two online devices that are missing", asked)
	}
}
