package main

import (
	"reflect"
	"testing"
)

// Lines as `avahi-browse --parsable --resolve --terminate` prints them.
const (
	avahiBrowseLine = `+;enp3s0;IPv4;Nimbustor;_ASUSTOR_ADM._tcp;local`
	avahiADMLine    = `=;enp3s0;IPv4;Nimbustor;_ASUSTOR_ADM._tcp;local;Nimbustor.local;192.168.178.20;8000;"ifnames=eth0" "hasabnormalvolume=No" "appupdatable=No" "wol=Yes" "serialnumber=AS2025TEST01" "hostid=00-11-32-12-34-56" "bios=1.0" "admupdate=No" "version=4.3.3.RC92" "model=AS5304T" "httpsport=8001" "httpenabled=Yes" "state=inited"`
	avahiADMv6Line  = `=;enp3s0;IPv6;Nimbustor;_ASUSTOR_ADM._tcp;local;Nimbustor.local;fe80::211:32ff:fe12:3456;8000;"state=inited" "model=AS5304T" "hostid=00-11-32-12-34-56"`
	avahiInitLine   = `=;wlan0;IPv4;Office\032NAS\.v2;_ASUSTOR_INIT._tcp;local;AS6704T-1234.local;10.0.0.7;8000;"model=AS6704T" "hostid=00-11-32-aa-bb-cc"`
)

func TestParseAvahiLine(t *testing.T) {
	tests := []struct {
		name        string
		line        string
		initialized bool
		wantOK      bool
		wantIface   string
		want        Nas
	}{
		{"ADM over IPv4", avahiADMLine, true, true, "enp3s0", Nas{
			Name: "Nimbustor", IP: "192.168.178.20", Port: 8000, Initialized: true, State: "Ready",
			HTTPEnabled: "Yes", HTTPSPort: "8001", Model: "AS5304T", ADMVersion: "4.3.3.RC92", ADMUpdate: "No",
			MAC: "00:11:32:12:34:56", SerialNumber: "AS2025TEST01", WOL: "Yes",
		}},
		{"ADM over IPv6", avahiADMv6Line, true, true, "enp3s0", Nas{
			Name: "Nimbustor", IPv6: "fe80::211:32ff:fe12:3456", Port: 8000, Initialized: true, State: "Ready",
			Model: "AS5304T", MAC: "00:11:32:12:34:56",
		}},
		{"INIT with escaped name and no state", avahiInitLine, false, true, "wlan0", Nas{
			Name: "Office NAS.v2", IP: "10.0.0.7", Port: 8000, State: "Uninitialized", Model: "AS6704T", MAC: "00:11:32:AA:BB:CC",
		}},
		{"TXT value with the field separator", `=;eth0;IPv4;N;_ASUSTOR_ADM._tcp;local;n.local;10.0.0.8;8000;"serialnumber=AB;CD" "state=inited"`, true, true, "eth0", Nas{
			Name: "N", IP: "10.0.0.8", Port: 8000, Initialized: true, State: "Ready", SerialNumber: "AB;CD",
		}},
		{"bad port", `=;eth0;IPv4;N;_ASUSTOR_ADM._tcp;local;n.local;10.0.0.8;99999;"state=inited"`, true, true, "eth0", Nas{
			Name: "N", IP: "10.0.0.8", Initialized: true, State: "Ready",
		}},
		{"browse event", avahiBrowseLine, true, false, "", Nas{}},
		{"removal event", `-;enp3s0;IPv4;Nimbustor;_ASUSTOR_ADM._tcp;local`, true, false, "", Nas{}},
		{"empty line", ``, true, false, "", Nas{}},
		{"short resolved line", `=;eth0;IPv4;N;_ASUSTOR_ADM._tcp;local;n.local;10.0.0.8`, true, false, "", Nas{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, iface, ok := parseAvahiLine(tc.line, tc.initialized)
			if ok != tc.wantOK || iface != tc.wantIface || got != tc.want {
				t.Errorf("got %+v, %q, %v\nwant %+v, %q, %v", got, iface, ok, tc.want, tc.wantIface, tc.wantOK)
			}
		})
	}
}

func TestParseAvahiTxt(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{`"k=v" "k2=v2"`, []string{"k=v", "k2=v2"}},
		{`"k=v"`, []string{"k=v"}},
		{`"serialnumber=AB;CD" "x=1"`, []string{"serialnumber=AB;CD", "x=1"}},
		{`"" "k=v"`, []string{"", "k=v"}},
		{`"k=v" "unterminated`, []string{"k=v"}},
		{`no quotes`, nil},
		{``, nil},
	}
	for _, tc := range tests {
		if got := parseAvahiTxt(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseAvahiTxt(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAvahiUnescape(t *testing.T) {
	tests := map[string]string{
		`Nimbustor`:          "Nimbustor",
		`Office\032NAS`:      "Office NAS",
		`a\.b`:               "a.b",
		`a\\b`:               `a\b`,
		`caf\195\169`:        "café",
		`\032\032`:           "  ",
		`tail\`:              `tail\`,
		`\12x`:               "12x",
		`\999`:               "999",
		`\255\000`:           "\xff\x00",
		`Nimbustor\032(2nd)`: "Nimbustor (2nd)",
	}
	for in, want := range tests {
		if got := avahiUnescape(in); got != want {
			t.Errorf("avahiUnescape(%q) = %q, want %q", in, got, want)
		}
	}
}
