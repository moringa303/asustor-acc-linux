package main

import (
	"fmt"
	"net"
	"strings"
	"time"
)

// Nas is one discovered (or remembered) ASUSTOR NAS. The JSON form is the
// contract between `acc scan --json` and the GUI.
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
	// URL of the ADM web UI (or of the setup wizard for an uninitialized
	// NAS), derived from the fields above.
	URL string `json:"url"`

	// Presence, maintained by the device memory (cache.go). Online is true
	// when the NAS answered the scan that produced this record. OnlineSince
	// is when the current run of consecutive scans that saw the NAS began;
	// this is what acc has observed, not the uptime of the NAS, which the
	// discovery protocol does not expose.
	Online      bool      `json:"online"`
	OnlineSince time.Time `json:"online_since"`
	LastSeen    time.Time `json:"last_seen"`
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
			n.MAC = normalizeMAC(v)
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

// normalizeMAC renders a MAC address the way acc displays and compares
// them: upper case, colon separated. The TXT record uses dashes.
func normalizeMAC(s string) string {
	if hw, err := net.ParseMAC(s); err == nil {
		return strings.ToUpper(hw.String())
	}
	return strings.ToUpper(strings.ReplaceAll(s, "-", ":"))
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

// key identifies a NAS across scans: the MAC when the NAS reported one,
// otherwise the instance name.
func (n *Nas) key() string {
	if n.MAC != "" {
		return n.MAC
	}
	return strings.ToLower(n.Name)
}

// matches reports whether target (a name, IPv4/IPv6 address or MAC in any
// common notation) refers to this NAS.
func (n *Nas) matches(target string) bool {
	if strings.EqualFold(n.Name, target) {
		return true
	}
	if ip := net.ParseIP(target); ip != nil {
		return ip.Equal(net.ParseIP(n.IP)) || ip.Equal(net.ParseIP(n.IPv6))
	}
	if hw, err := net.ParseMAC(target); err == nil {
		return n.MAC != "" && strings.EqualFold(n.MAC, hw.String())
	}
	return false
}

// webURL is where a browser should go for this NAS, or "" when no address
// is known.
func (n *Nas) webURL() string {
	host := n.IP
	if host == "" && n.IPv6 != "" {
		host = "[" + n.IPv6 + "]"
	}
	if host == "" {
		return ""
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

// mergeNas records n in found, keyed by identity. When the same NAS was
// already seen (through another service type, resolver or address family),
// the more informative record wins: an ADM announcement over an INIT one,
// one with an IPv4 address over one without. Addresses missing from the
// winner are filled in from the other record.
func mergeNas(found map[string]Nas, n Nas) {
	prev, dup := found[n.key()]
	if !dup {
		found[n.key()] = n
		return
	}
	better := (n.Initialized && !prev.Initialized) || (n.IP != "" && prev.IP == "")
	if !better {
		n, prev = prev, n
	}
	if n.IP == "" {
		n.IP = prev.IP
	}
	if n.IPv6 == "" {
		n.IPv6 = prev.IPv6
	}
	found[n.key()] = n
}
