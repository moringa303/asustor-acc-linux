package main

import (
	"errors"
	"testing"
	"time"
)

// tableAgent is a fake agent over a sorted list of objects: GetRequest
// answers exact matches (noSuchObject otherwise), GetNextRequest the
// first object after the asked one (endOfMibView past the last).
func tableAgent(objects []snmpVarbind) func(req *snmpResponse) [][]byte {
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

func TestSNMPHealth(t *testing.T) {
	disk := func(col, idx uint32) []uint32 { return append(append([]uint32{}, oidASDisks...), col, idx) }
	vol := func(col, idx uint32) []uint32 { return append(append([]uint32{}, oidASVolumes...), col, idx) }
	objects := []snmpVarbind{
		strVB(oidASSerial, "AS2026LOCK01"),
		strVB(oidASADM, "5.0.0.RAB1"),
		strVB(oidASModel, "AS6704T"),
		intVB(oidASSysTemp, 41),
		intVB(oidASCPUTemp, 52),
		intVB(oidASTotalMem, 8192),
		intVB(oidASFreeMem, 6000),
		strVB(oidASProcessor, "Intel Celeron N5105"),
		intVB(append(append([]uint32{}, oidASCPUUsage...), 1), 7),
		intVB(append(append([]uint32{}, oidASCPUUsage...), 2), 3),
		intVB(append(append([]uint32{}, oidASFanSpeed...), 1), 850),
		strVB(disk(2, 1), "Disk 1"), strVB(disk(2, 2), "Disk 2"),
		strVB(disk(3, 1), "WD80EFZZ"), strVB(disk(3, 2), "WD80EFZZ"),
		strVB(disk(5, 1), "normal"), strVB(disk(5, 2), "normal"),
		intVB(disk(6, 1), 36), intVB(disk(6, 2), 38),
		intVB(disk(7, 1), 8000), intVB(disk(7, 2), 8000),
		strVB(disk(8, 1), "good"), strVB(disk(8, 2), "good"),
		strVB(vol(2, 1), "Volume 1"), strVB(vol(3, 1), "RAID 1"), strVB(vol(4, 1), "normal"),
		strVB(vol(5, 1), "ext4"), intVB(vol(6, 1), 7800), intVB(vol(7, 1), 3100),
	}
	// GetNext walks in OID order; keep the list sorted the same way.
	for i := 1; i < len(objects); i++ {
		for j := i; j > 0 && oidCompare(objects[j-1].oid, objects[j].oid) > 0; j-- {
			objects[j-1], objects[j] = objects[j], objects[j-1]
		}
	}
	addr, _ := fakeAgent(t, tableAgent(objects))
	r, err := snmpHealth(addr, "public", 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if r.Model != "AS6704T" || r.ADMVersion != "5.0.0.RAB1" || r.SerialNumber != "AS2026LOCK01" {
		t.Errorf("scalars: %+v", r)
	}
	if r.SysTempC == nil || *r.SysTempC != 41 || r.CPUTempC == nil || *r.CPUTempC != 52 {
		t.Errorf("temperatures: %+v %+v", r.SysTempC, r.CPUTempC)
	}
	if r.TotalMemMB == nil || *r.TotalMemMB != 8192 || r.FreeMemMB == nil || *r.FreeMemMB != 6000 {
		t.Errorf("memory: %+v %+v", r.TotalMemMB, r.FreeMemMB)
	}
	if len(r.CPUUsage) != 2 || r.CPUUsage[0] != 7 || r.CPUUsage[1] != 3 || len(r.FanRPM) != 1 || r.FanRPM[0] != 850 {
		t.Errorf("cpu %v fans %v", r.CPUUsage, r.FanRPM)
	}
	if len(r.Disks) != 2 || r.Disks[1].Name != "Disk 2" || r.Disks[1].TempC == nil || *r.Disks[1].TempC != 38 ||
		r.Disks[0].Smart != "good" || r.Disks[0].SizeGB == nil || *r.Disks[0].SizeGB != 8000 {
		t.Errorf("disks: %+v", r.Disks)
	}
	if len(r.Volumes) != 1 || r.Volumes[0].Name != "Volume 1" || r.Volumes[0].Level != "RAID 1" ||
		r.Volumes[0].FreeGB == nil || *r.Volumes[0].FreeGB != 3100 {
		t.Errorf("volumes: %+v", r.Volumes)
	}

	// An agent without the ASUSTOR MIB (net-snmp on a PC, say).
	addr, _ = fakeAgent(t, tableAgent([]snmpVarbind{{oid: oidSysUpTime, tag: tagTimeTicks, value: []byte{0x01, 0x00}}}))
	r, err = snmpHealth(addr, "public", 300*time.Millisecond)
	if !errors.Is(err, errNoAsustorMIB) {
		t.Fatalf("plain agent: err = %v, want errNoAsustorMIB", err)
	}
	if r == nil || r.UptimeSeconds == nil || *r.UptimeSeconds != 2 {
		t.Errorf("plain agent: uptime still reported: %+v", r)
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
