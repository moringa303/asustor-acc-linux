package main

import "testing"

func TestApplyTxt(t *testing.T) {
	tests := []struct {
		name string
		txt  []string
		want Nas
	}{
		{"real ADM record", []string{
			"state=inited", "httpenabled=Yes", "httpsport=8001", "model=AS5304T",
			"version=4.3.3.RC92", "admupdate=No", "bios=1.0", "hostid=00-11-32-aa-bb-cc",
			"serialnumber=AS2025TEST01", "wol=Yes", "appupdatable=No", "hasabnormalvolume=No", "ifnames=eth0",
		}, Nas{
			State: "Ready", HTTPEnabled: "Yes", HTTPSPort: "8001", Model: "AS5304T", ADMVersion: "4.3.3.RC92",
			ADMUpdate: "No", MAC: "00:11:32:AA:BB:CC", SerialNumber: "AS2025TEST01", WOL: "Yes",
		}},
		{"keys in any case", []string{"MODEL=AS6704T", "HostID=00-11-32-01-02-03"}, Nas{Model: "AS6704T", MAC: "00:11:32:01:02:03"}},
		{"junk ignored", []string{"novalue", "", "=x", "model="}, Nas{}},
		{"factory fresh", []string{"state=uninited"}, Nas{State: "Uninitialized"}},
		{"booting", []string{"state=booting"}, Nas{State: "Not ready"}},
		{"value keeps its own =", []string{"version=a=b"}, Nas{ADMVersion: "a=b"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var n Nas
			n.applyTxt(tc.txt)
			if n != tc.want {
				t.Errorf("got  %+v\nwant %+v", n, tc.want)
			}
		})
	}
}

func TestDisplayState(t *testing.T) {
	tests := map[string]string{
		"inited": "Ready", "INITED": "Ready", "uninited": "Uninitialized", "": "",
		"booting": "Not ready", "shutdown": "Not ready",
	}
	for in, want := range tests {
		if got := displayState(in); got != want {
			t.Errorf("displayState(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeMAC(t *testing.T) {
	tests := map[string]string{
		"00-11-32-aa-bb-cc": "00:11:32:AA:BB:CC",
		"00:11:32:aa:bb:cc": "00:11:32:AA:BB:CC",
		"0011.32aa.bbcc":    "00:11:32:AA:BB:CC",
		"00-11-32-AA-BB-CC": "00:11:32:AA:BB:CC",
		// Not a MAC at all: dashes become colons, nothing else is assumed.
		"zz-yy": "ZZ:YY",
		"":      "",
	}
	for in, want := range tests {
		if got := normalizeMAC(in); got != want {
			t.Errorf("normalizeMAC(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMatches(t *testing.T) {
	n := Nas{Name: "Nimbustor", IP: "192.168.1.20", IPv6: "fe80::211:32ff:fe12:3456", MAC: "00:11:32:12:34:56"}
	tests := map[string]bool{
		"Nimbustor": true, "nimbustor": true, "NIMBUSTOR": true, "Nimbus": false,
		"192.168.1.20": true, "192.168.1.21": false, "::ffff:192.168.1.20": true,
		"fe80::211:32ff:fe12:3456": true, "FE80:0:0:0:211:32FF:FE12:3456": true, "fe80::1": false,
		"00:11:32:12:34:56": true, "00-11-32-12-34-56": true, "0011.3212.3456": true,
		"00:11:32:12:34:57": false, "": false,
	}
	for target, want := range tests {
		if got := n.matches(target); got != want {
			t.Errorf("matches(%q) = %v, want %v", target, got, want)
		}
	}
	noMAC := Nas{Name: "X"}
	if noMAC.matches("00:11:32:12:34:56") || noMAC.matches("192.168.1.20") {
		t.Error("a NAS without MAC or address matched one")
	}
}

func TestKey(t *testing.T) {
	if k := (&Nas{Name: "Nimbustor", MAC: "00:11:32:12:34:56"}).key(); k != "00:11:32:12:34:56" {
		t.Errorf("key = %q, want the MAC", k)
	}
	if k := (&Nas{Name: "Nimbustor"}).key(); k != "nimbustor" {
		t.Errorf("key = %q, want the lower-cased name", k)
	}
}

func TestWebURL(t *testing.T) {
	tests := []struct {
		name string
		n    Nas
		want string
	}{
		{"no address", Nas{Port: 8000, Initialized: true}, ""},
		{"http on the SRV port", Nas{IP: "192.168.1.20", Port: 8000, Initialized: true, HTTPEnabled: "Yes", HTTPSPort: "8001"}, "http://192.168.1.20:8000/"},
		{"custom port", Nas{IP: "192.168.1.20", Port: 8080, Initialized: true}, "http://192.168.1.20:8080/"},
		{"https when http is off", Nas{IP: "192.168.1.20", Port: 8000, Initialized: true, HTTPEnabled: "No", HTTPSPort: "8001"}, "https://192.168.1.20:8001/"},
		{"http off but no https port", Nas{IP: "192.168.1.20", Port: 8000, Initialized: true, HTTPEnabled: "No"}, "http://192.168.1.20:8000/"},
		{"no SRV port", Nas{IP: "192.168.1.20", Initialized: true}, "http://192.168.1.20:8000/"},
		{"setup wizard", Nas{IP: "192.168.1.20", Port: 8000, HTTPEnabled: "No", HTTPSPort: "8001"}, "http://192.168.1.20:8000/"},
		{"setup wizard without port", Nas{IP: "192.168.1.20"}, "http://192.168.1.20:8000/"},
		{"IPv6 only", Nas{IPv6: "fe80::1", Port: 8000, Initialized: true}, "http://[fe80::1]:8000/"},
		{"IPv4 preferred", Nas{IP: "192.168.1.20", IPv6: "fe80::1", Port: 8000, Initialized: true}, "http://192.168.1.20:8000/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.n.webURL(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSharesURL(t *testing.T) {
	tests := []struct {
		n    Nas
		want string
	}{
		{Nas{IP: "192.168.1.20"}, "smb://192.168.1.20/"},
		{Nas{IP: "192.168.1.20", IPv6: "fe80::1"}, "smb://192.168.1.20/"},
		{Nas{IPv6: "fe80::1"}, ""},
		{Nas{IP: "fe80::1"}, ""},
		{Nas{IP: "not an address"}, ""},
		{Nas{}, ""},
	}
	for _, tc := range tests {
		if got := tc.n.sharesURL(); got != tc.want {
			t.Errorf("sharesURL(%+v) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestMergeNas(t *testing.T) {
	adm := Nas{Name: "N", MAC: "00:11:32:AA:BB:CC", IP: "192.168.1.5", Port: 8000, Initialized: true, State: "Ready"}
	setup := Nas{Name: "N", MAC: "00:11:32:AA:BB:CC", IPv6: "fe80::1", Port: 8000, State: "Uninitialized"}
	v6 := Nas{Name: "N", MAC: "00:11:32:AA:BB:CC", IPv6: "fe80::2", Port: 8000, Initialized: true, State: "Ready"}

	// Whatever the order, the ADM record wins and the other one's
	// address fills the gap.
	for _, order := range [][]Nas{{adm, setup}, {setup, adm}, {adm, v6}, {v6, adm}} {
		found := map[string]Nas{}
		for _, n := range order {
			mergeNas(found, n)
		}
		got := found[adm.key()]
		want := adm
		want.IPv6 = order[0].IPv6 + order[1].IPv6
		if len(found) != 1 || got != want {
			t.Errorf("merge of %v:\ngot  %+v\nwant %+v", order, got, want)
		}
	}

	// Two different devices stay apart, keyed by MAC or, without one, by
	// name in any case.
	found := map[string]Nas{}
	mergeNas(found, adm)
	mergeNas(found, Nas{Name: "N", MAC: "00:11:32:00:00:01", IP: "192.168.1.6", Initialized: true})
	mergeNas(found, Nas{Name: "Other", IP: "192.168.1.7"})
	mergeNas(found, Nas{Name: "OTHER", IPv6: "fe80::7"})
	if len(found) != 3 {
		t.Fatalf("got %d records, want 3: %v", len(found), found)
	}
	if o := found["other"]; o.IP != "192.168.1.7" || o.IPv6 != "fe80::7" {
		t.Errorf("name-keyed merge: %+v", o)
	}
}
