package main

import (
	"errors"
	"sort"
	"testing"
	"time"
)

// tableAgent is a fake agent over a list of objects: GetRequest answers
// exact matches (noSuchObject otherwise), GetNextRequest the first
// object after the asked one (endOfMibView past the last).
func tableAgent(objects []snmpVarbind) func(req *snmpResponse) [][]byte {
	sort.Slice(objects, func(i, j int) bool { return oidCompare(objects[i].oid, objects[j].oid) < 0 })
	encode := func(vb snmpVarbind) []byte {
		return berTLV(tagSequence, append(berOID(vb.oid), berTLV(vb.tag, vb.value)...))
	}
	return func(req *snmpResponse) [][]byte {
		var vbs [][]byte
		for _, asked := range req.varbinds {
			var hit *snmpVarbind
			for i := range objects {
				c := oidCompare(objects[i].oid, asked.oid)
				if (req.pduTag == tagGetRequest && c == 0) || (req.pduTag == tagGetNextRequest && c > 0) {
					hit = &objects[i]
					break
				}
			}
			switch {
			case hit != nil:
				vbs = append(vbs, encode(*hit))
			case req.pduTag == tagGetRequest:
				vbs = append(vbs, vbEmpty(asked.oid, tagNoSuchObject))
			default:
				vbs = append(vbs, vbEmpty(asked.oid, tagEndOfMibView))
			}
		}
		return answer(req, vbs...)
	}
}

func intVB(oid []uint32, v int64) snmpVarbind {
	return snmpVarbind{oid: oid, tag: tagInteger, value: berInt(tagInteger, v)[2:]}
}

func strVB(oid []uint32, s string) snmpVarbind {
	return snmpVarbind{oid: oid, tag: tagOctetString, value: []byte(s)}
}

func cell(table []uint32, col, idx uint32) []uint32 {
	return append(append([]uint32{}, table...), col, idx)
}

const healthTimeout = 300 * time.Millisecond

// The newer MIB (ADM 4): processor at .2.2, tables for CPU, fans,
// temperatures and memory, SMART in disk column 6, a UPS table.
func TestSNMPHealthNewLayout(t *testing.T) {
	objects := []snmpVarbind{
		strVB(oidASSerial, "AS2026LOCK01"), strVB(oidASADM, "5.0.0.RAB1"), strVB(oidASBios, "1.23"),
		strVB(oidASUptimeText, "14 days 6:56"), strVB(oidASUpgrade, "Available"),
		strVB(oidASModel, "AS6704T"), strVB(oidNewProcessor, "Intel Celeron N5105"),
		intVB(cell(asustorOID(2, 3, 1, 1), 2, 1), 7), intVB(cell(asustorOID(2, 3, 1, 1), 2, 2), 3),
		intVB(cell(asustorOID(2, 4, 1, 1), 2, 1), 850),
		intVB(cell(oidNewTemps, 2, 1), 52), intVB(cell(oidNewTemps, 3, 1), 41),
		intVB(cell(oidNewMemory, 2, 1), 8192), intVB(cell(oidNewMemory, 3, 1), 6000), intVB(cell(oidNewMemory, 4, 1), 73),
		strVB(cell(oidASDisks, 2, 1), "Disk 1"), strVB(cell(oidASDisks, 3, 1), "WD80EFZZ"),
		strVB(cell(oidASDisks, 4, 1), "SATA"), strVB(cell(oidASDisks, 5, 1), "Healthy"),
		strVB(cell(oidASDisks, 6, 1), "Normal"), intVB(cell(oidASDisks, 7, 1), 36), intVB(cell(oidASDisks, 8, 1), 8000),
		strVB(cell(oidASDisks, 2, 2), "Disk 2"), strVB(cell(oidASDisks, 3, 2), "WD80EFZZ"),
		strVB(cell(oidASDisks, 4, 2), "SATA"), strVB(cell(oidASDisks, 5, 2), "Risky"),
		strVB(cell(oidASDisks, 6, 2), "Bad"), intVB(cell(oidASDisks, 7, 2), 48), intVB(cell(oidASDisks, 8, 2), 8000),
		strVB(cell(oidASVolumes, 2, 1), "Volume 1"), strVB(cell(oidASVolumes, 3, 1), "raid1"),
		strVB(cell(oidASVolumes, 4, 1), "healthy"), strVB(cell(oidASVolumes, 5, 1), "ext4"),
		intVB(cell(oidASVolumes, 6, 1), 7800), intVB(cell(oidASVolumes, 7, 1), 3100),
		strVB(cell(oidNewUps, 2, 1), "APC"), strVB(cell(oidNewUps, 3, 1), "Back-UPS 700"),
		strVB(cell(oidNewUps, 7, 1), "OL CHRG"), intVB(cell(oidNewUps, 8, 1), 1500),
		intVB(cell(oidNewUps, 9, 1), 85), intVB(cell(oidNewUps, 11, 1), 230),
		{oid: oidHrSystemUptime, tag: tagTimeTicks, value: []byte{0x07, 0x5B, 0xCD, 0x15}},
	}
	addr, _ := fakeAgent(t, tableAgent(objects))
	r, err := snmpHealth(addr, "public", healthTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if r.Layout != "new" || r.Model != "AS6704T" || r.Processor != "Intel Celeron N5105" ||
		r.ADMVersion != "5.0.0.RAB1" || r.BiosVersion != "1.23" || r.SerialNumber != "AS2026LOCK01" ||
		r.UptimeText != "14 days 6:56" || r.ADMUpdate == nil || !*r.ADMUpdate {
		t.Errorf("system: %+v", r)
	}
	if r.UptimeSeconds == nil || *r.UptimeSeconds != 1234567 {
		t.Errorf("uptime: %v", r.UptimeSeconds)
	}
	if r.SysTempC == nil || *r.SysTempC != 41 || r.CPUTempC == nil || *r.CPUTempC != 52 {
		t.Errorf("temperatures: %v %v", r.SysTempC, r.CPUTempC)
	}
	if r.TotalMemMB == nil || *r.TotalMemMB != 8192 || r.FreeMemMB == nil || *r.FreeMemMB != 6000 {
		t.Errorf("memory: %v %v", r.TotalMemMB, r.FreeMemMB)
	}
	if len(r.CPUUsage) != 2 || r.CPUUsage[0] != 7 || len(r.FanRPM) != 1 || r.FanRPM[0] != 850 {
		t.Errorf("cpu %v fans %v", r.CPUUsage, r.FanRPM)
	}
	if len(r.Disks) != 2 || r.Disks[1].Status != "Risky" || r.Disks[1].Smart != "Bad" ||
		r.Disks[1].TempC == nil || *r.Disks[1].TempC != 48 || r.Disks[0].SizeGB == nil || *r.Disks[0].SizeGB != 8000 ||
		r.Disks[0].Type != "SATA" {
		t.Errorf("disks: %+v", r.Disks)
	}
	if len(r.Volumes) != 1 || r.Volumes[0].Level != "raid1" || r.Volumes[0].Status != "healthy" ||
		r.Volumes[0].FreeGB == nil || *r.Volumes[0].FreeGB != 3100 {
		t.Errorf("volumes: %+v", r.Volumes)
	}
	if r.UPS == nil || r.UPS.Model != "Back-UPS 700" || r.UPS.Status != "OL CHRG" ||
		r.UPS.ChargePercent == nil || *r.UPS.ChargePercent != 85 || r.UPS.InputVoltage == nil || *r.UPS.InputVoltage != 230 {
		t.Errorf("ups: %+v", r.UPS)
	}
	if got := upsStatusText("OL CHRG"); got != "on line, charging" {
		t.Errorf("upsStatusText = %q", got)
	}
}

// The older MIB: temperatures and memory as scalars, the CPU and fan
// tables at .2.7 and .2.8, the disk temperature in column 6 and the
// SMART verdict in column 8, UPS scalars.
func TestSNMPHealthOldLayout(t *testing.T) {
	objects := []snmpVarbind{
		strVB(oidASSerial, "AS2019OLD007"), strVB(oidASADM, "3.5.9.RHG1"), strVB(oidASUpgrade, "Unavailable"),
		strVB(oidASModel, "AS3102T"), intVB(oidOldSysTemp, 39), intVB(oidOldCPUTemp, 47),
		intVB(oidOldTotalMem, 2048), intVB(oidOldFreeMem, 900), strVB(oidOldProcessor, "Intel Celeron N3050"),
		intVB(cell(asustorOID(2, 7, 1, 1), 2, 1), 12), intVB(cell(asustorOID(2, 8, 1, 1), 2, 1), 1200),
		strVB(cell(oidASDisks, 2, 1), "Disk 1"), strVB(cell(oidASDisks, 3, 1), "ST4000VN008"),
		strVB(cell(oidASDisks, 4, 1), "SATA"), strVB(cell(oidASDisks, 5, 1), "Healthy"),
		intVB(cell(oidASDisks, 6, 1), 34), intVB(cell(oidASDisks, 7, 1), 4000), strVB(cell(oidASDisks, 8, 1), "Good"),
		strVB(cell(oidASVolumes, 2, 1), "Volume 1"), strVB(cell(oidASVolumes, 3, 1), "single"),
		strVB(cell(oidASVolumes, 4, 1), "degraded"), strVB(cell(oidASVolumes, 5, 1), "ext4"),
		intVB(cell(oidASVolumes, 6, 1), 3900), intVB(cell(oidASVolumes, 7, 1), 100),
		strVB(oidOldUpsMaker, "CyberPower"), strVB(oidOldUpsModel, "CP900"), strVB(oidOldUpsStatus, "OB LB"),
		intVB(oidOldUpsCharge, 12), intVB(oidOldUpsRuntime, 240), intVB(oidOldUpsVoltage, 0),
	}
	addr, _ := fakeAgent(t, tableAgent(objects))
	r, err := snmpHealth(addr, "public", healthTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if r.Layout != "old" || r.Processor != "Intel Celeron N3050" || r.ADMUpdate == nil || *r.ADMUpdate {
		t.Errorf("system: %+v", r)
	}
	if r.SysTempC == nil || *r.SysTempC != 39 || r.CPUTempC == nil || *r.CPUTempC != 47 ||
		r.TotalMemMB == nil || *r.TotalMemMB != 2048 || r.FreeMemMB == nil || *r.FreeMemMB != 900 {
		t.Errorf("scalars: %v %v %v %v", r.SysTempC, r.CPUTempC, r.TotalMemMB, r.FreeMemMB)
	}
	if len(r.CPUUsage) != 1 || r.CPUUsage[0] != 12 || len(r.FanRPM) != 1 || r.FanRPM[0] != 1200 {
		t.Errorf("cpu %v fans %v", r.CPUUsage, r.FanRPM)
	}
	if len(r.Disks) != 1 || r.Disks[0].TempC == nil || *r.Disks[0].TempC != 34 ||
		r.Disks[0].SizeGB == nil || *r.Disks[0].SizeGB != 4000 || r.Disks[0].Smart != "Good" {
		t.Errorf("disks: %+v", r.Disks)
	}
	if len(r.Volumes) != 1 || r.Volumes[0].Status != "degraded" {
		t.Errorf("volumes: %+v", r.Volumes)
	}
	if r.UPS == nil || r.UPS.Manufacturer != "CyberPower" || r.UPS.Status != "OB LB" ||
		r.UPS.RuntimeSeconds == nil || *r.UPS.RuntimeSeconds != 240 {
		t.Errorf("ups: %+v", r.UPS)
	}
}

// --mib forces a layout: the old agent read as old, the new one as new,
// and a wrong forced layout still gives a report rather than an error.
func TestSNMPHealthForcedLayout(t *testing.T) {
	defer func() { mibLayout = "auto" }()
	newAgent := []snmpVarbind{
		strVB(oidASModel, "AS6704T"), strVB(oidNewProcessor, "Intel Celeron N5105"),
		intVB(cell(oidNewTemps, 2, 1), 52), intVB(cell(oidNewTemps, 3, 1), 41),
		strVB(cell(oidASDisks, 2, 1), "Disk 1"), strVB(cell(oidASDisks, 6, 1), "Normal"),
		intVB(cell(oidASDisks, 7, 1), 36), intVB(cell(oidASDisks, 8, 1), 8000),
	}
	oldAgent := []snmpVarbind{
		strVB(oidASModel, "AS3102T"), intVB(oidOldSysTemp, 39), intVB(oidOldCPUTemp, 47),
		strVB(oidOldProcessor, "Intel Celeron N3050"),
		strVB(cell(oidASDisks, 2, 1), "Disk 1"), intVB(cell(oidASDisks, 6, 1), 34),
		intVB(cell(oidASDisks, 7, 1), 4000), strVB(cell(oidASDisks, 8, 1), "Good"),
	}
	for _, tc := range []struct {
		name, force string
		agent       []snmpVarbind
		wantLayout  string
		wantCPUTemp int64
		wantSmart   string
	}{
		{"new forced new", "new", newAgent, "new", 52, "Normal"},
		{"old forced old", "old", oldAgent, "old", 47, "Good"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mibLayout = tc.force
			addr, _ := fakeAgent(t, tableAgent(tc.agent))
			r, err := snmpHealth(addr, "public", healthTimeout)
			if err != nil {
				t.Fatal(err)
			}
			if r.Layout != tc.wantLayout || r.CPUTempC == nil || *r.CPUTempC != tc.wantCPUTemp ||
				len(r.Disks) != 1 || r.Disks[0].Smart != tc.wantSmart {
				t.Errorf("forced %s: %+v disks %+v", tc.force, r, r.Disks)
			}
		})
	}
	t.Run("old agent forced new still answers", func(t *testing.T) {
		mibLayout = "new"
		addr, _ := fakeAgent(t, tableAgent(oldAgent))
		r, err := snmpHealth(addr, "public", healthTimeout)
		if err != nil || r.Layout != "new" || r.Model != "AS3102T" {
			t.Errorf("got %+v, %v", r, err)
		}
	})
}

func TestSNMPHealthPlainAgent(t *testing.T) {
	// An agent without the ASUSTOR MIB (net-snmp on a PC, say).
	addr, _ := fakeAgent(t, tableAgent([]snmpVarbind{{oid: oidSysUpTime, tag: tagTimeTicks, value: []byte{0x01, 0x00}}}))
	r, err := snmpHealth(addr, "public", healthTimeout)
	if !errors.Is(err, errNoAsustorMIB) {
		t.Fatalf("plain agent: err = %v, want errNoAsustorMIB", err)
	}
	if r == nil || r.UptimeSeconds == nil || *r.UptimeSeconds != 2 {
		t.Errorf("plain agent: uptime still reported: %+v", r)
	}
}

func TestMemoryMB(t *testing.T) {
	mb, kb := int64(8192), int64(8192*1024)
	if got := memoryMB(&mb); *got != 8192 {
		t.Errorf("MB kept: %d", *got)
	}
	if got := memoryMB(&kb); *got != 8192 {
		t.Errorf("KB normalized: %d", *got)
	}
	if memoryMB(nil) != nil {
		t.Error("nil kept")
	}
}

func TestSubnetHosts(t *testing.T) {
	hosts, err := subnetHosts("192.168.1.0/30")
	if err != nil || len(hosts) != 2 || hosts[0].String() != "192.168.1.1" || hosts[1].String() != "192.168.1.2" {
		t.Errorf("/30: %v %v", hosts, err)
	}
	hosts, err = subnetHosts("10.0.0.7/32")
	if err != nil || len(hosts) != 1 || hosts[0].String() != "10.0.0.7" {
		t.Errorf("/32: %v %v", hosts, err)
	}
	if hosts, err := subnetHosts("10.0.0.0/24"); err != nil || len(hosts) != 254 {
		t.Errorf("/24: %d hosts, %v", len(hosts), err)
	}
	for _, bad := range []string{"10.0.0.0/16", "fe80::/64", "not a subnet"} {
		if _, err := subnetHosts(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestInstanceLabel(t *testing.T) {
	cases := map[string]string{
		"Lockerstor._ASUSTOR_ADM._tcp.local.":       "Lockerstor",
		"My\\032NAS._ASUSTOR_ADM._tcp.local.":       "My NAS",
		"Office\\.1._ASUSTOR_INIT._tcp.local.":      "Office.1",
		"AS1104T-1234._ASUSTOR_ADM._tcp.local.":     "AS1104T-1234",
		"Plain":                                     "Plain",
		"Umlaut\\195\\164._ASUSTOR_ADM._tcp.local.": "Umlautä",
	}
	for in, want := range cases {
		if got := instanceLabel(in); got != want {
			t.Errorf("instanceLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
