package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// `acc health`: what the NAS reports about itself over SNMP, from the
// ASUSTOR MIB (enterprise 44738): temperatures, fans, CPU and memory,
// the disks and the volumes. The same service that answers the uptime.

// Scalars of the ASUSTOR MIB, relative to 1.3.6.1.4.1.44738.
var (
	oidASSerial    = asustorOID(1, 1, 0)
	oidASADM       = asustorOID(1, 2, 0)
	oidASModel     = asustorOID(2, 1, 0)
	oidASSysTemp   = asustorOID(2, 2, 0)
	oidASCPUTemp   = asustorOID(2, 3, 0)
	oidASTotalMem  = asustorOID(2, 4, 0) // MB
	oidASFreeMem   = asustorOID(2, 5, 0) // MB
	oidASProcessor = asustorOID(2, 6, 0)
	// Tables: one column per core / fan, the disk and volume tables whole.
	oidASCPUUsage = asustorOID(2, 7, 1, 1, 2) // percent per core
	oidASFanSpeed = asustorOID(2, 8, 1, 1, 2) // RPM
	oidASDisks    = asustorOID(4, 1, 1)       // 1 index 2 name 3 model 4 interface 5 status 6 temperature 7 size GB 8 SMART
	oidASVolumes  = asustorOID(5, 1, 1)       // 1 index 2 name 3 level 4 status 5 filesystem 6 total GB 7 free GB
)

func asustorOID(sub ...uint32) []uint32 {
	return append([]uint32{1, 3, 6, 1, 4, 1, 44738}, sub...)
}

type healthDisk struct {
	Index     int    `json:"index"`
	Name      string `json:"name"`
	Model     string `json:"model,omitempty"`
	Interface string `json:"interface,omitempty"`
	Status    string `json:"status,omitempty"`
	Smart     string `json:"smart,omitempty"`
	TempC     *int64 `json:"temp_c,omitempty"`
	SizeGB    *int64 `json:"size_gb,omitempty"`
}

type healthVolume struct {
	Index      int    `json:"index"`
	Name       string `json:"name"`
	Level      string `json:"level,omitempty"`
	Status     string `json:"status,omitempty"`
	FileSystem string `json:"filesystem,omitempty"`
	TotalGB    *int64 `json:"total_gb,omitempty"`
	FreeGB     *int64 `json:"free_gb,omitempty"`
}

type healthReport struct {
	Name          string         `json:"name"`
	IP            string         `json:"ip"`
	Model         string         `json:"model,omitempty"`
	ADMVersion    string         `json:"adm_version,omitempty"`
	SerialNumber  string         `json:"serial_number,omitempty"`
	Processor     string         `json:"processor,omitempty"`
	UptimeSeconds *int64         `json:"uptime_seconds,omitempty"`
	SysTempC      *int64         `json:"sys_temp_c,omitempty"`
	CPUTempC      *int64         `json:"cpu_temp_c,omitempty"`
	TotalMemMB    *int64         `json:"total_mem_mb,omitempty"`
	FreeMemMB     *int64         `json:"free_mem_mb,omitempty"`
	CPUUsage      []int64        `json:"cpu_usage,omitempty"` // percent per core
	FanRPM        []int64        `json:"fan_rpm,omitempty"`
	Disks         []healthDisk   `json:"disks"`
	Volumes       []healthVolume `json:"volumes"`
}

var errNoAsustorMIB = errors.New("the NAS answers SNMP but not the ASUSTOR MIB")

// snmpHealth asks the agent at addr (host:port) for everything above.
func snmpHealth(addr, community string, timeout time.Duration) (*healthReport, error) {
	conn, err := net.DialTimeout("udp", addr, timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	r := &healthReport{Disks: []healthDisk{}, Volumes: []healthVolume{}}

	scalars := [][]uint32{oidASSerial, oidASADM, oidASModel, oidASSysTemp, oidASCPUTemp,
		oidASTotalMem, oidASFreeMem, oidASProcessor, oidHrSystemUptime, oidSysUpTime}
	resp, err := snmpGet(conn, snmpV2c, community, scalars, timeout)
	if err != nil {
		// one retry: a single lost datagram must not look like "no SNMP"
		if resp, err = snmpGet(conn, snmpV2c, community, scalars, timeout); err != nil {
			return nil, err
		}
	}
	if resp.errorStatus != 0 {
		return nil, fmt.Errorf("snmp error status %d", resp.errorStatus)
	}
	found := false
	for _, vb := range resp.varbinds {
		if vb.tag == tagNoSuchObject || vb.tag == tagNoSuchInstance || vb.tag == tagEndOfMibView {
			continue
		}
		switch {
		case oidEqual(vb.oid, oidASSerial):
			r.SerialNumber, found = vb.text(), true
		case oidEqual(vb.oid, oidASADM):
			r.ADMVersion, found = vb.text(), true
		case oidEqual(vb.oid, oidASModel):
			r.Model, found = vb.text(), true
		case oidEqual(vb.oid, oidASProcessor):
			r.Processor, found = vb.text(), true
		case oidEqual(vb.oid, oidASSysTemp):
			r.SysTempC, found = number(vb), true
		case oidEqual(vb.oid, oidASCPUTemp):
			r.CPUTempC, found = number(vb), true
		case oidEqual(vb.oid, oidASTotalMem):
			r.TotalMemMB, found = number(vb), true
		case oidEqual(vb.oid, oidASFreeMem):
			r.FreeMemMB, found = number(vb), true
		case oidEqual(vb.oid, oidHrSystemUptime):
			if ticks, ok := vb.number(); ok {
				secs := ticks / 100
				r.UptimeSeconds = &secs
			}
		case oidEqual(vb.oid, oidSysUpTime):
			if ticks, ok := vb.number(); ok && r.UptimeSeconds == nil {
				secs := ticks / 100
				r.UptimeSeconds = &secs
			}
		}
	}
	if !found {
		return r, errNoAsustorMIB
	}

	walk := func(root []uint32) []snmpVarbind {
		vbs, err := snmpWalk(conn, snmpV2c, community, root, timeout)
		if err != nil {
			vlog("snmp walk %v: %v", root, err)
		}
		return vbs
	}
	for _, vb := range walk(oidASCPUUsage) {
		if n, ok := vb.number(); ok {
			r.CPUUsage = append(r.CPUUsage, n)
		}
	}
	for _, vb := range walk(oidASFanSpeed) {
		if n, ok := vb.number(); ok {
			r.FanRPM = append(r.FanRPM, n)
		}
	}
	disks := map[int]*healthDisk{}
	for _, vb := range walk(oidASDisks) {
		col, idx, ok := tableCell(vb.oid, oidASDisks)
		if !ok {
			continue
		}
		d := disks[idx]
		if d == nil {
			d = &healthDisk{Index: idx}
			disks[idx] = d
		}
		switch col {
		case 2:
			d.Name = vb.text()
		case 3:
			d.Model = vb.text()
		case 4:
			d.Interface = vb.text()
		case 5:
			d.Status = vb.text()
		case 6:
			d.TempC = number(vb)
		case 7:
			d.SizeGB = number(vb)
		case 8:
			d.Smart = vb.text()
		}
	}
	volumes := map[int]*healthVolume{}
	for _, vb := range walk(oidASVolumes) {
		col, idx, ok := tableCell(vb.oid, oidASVolumes)
		if !ok {
			continue
		}
		v := volumes[idx]
		if v == nil {
			v = &healthVolume{Index: idx}
			volumes[idx] = v
		}
		switch col {
		case 2:
			v.Name = vb.text()
		case 3:
			v.Level = vb.text()
		case 4:
			v.Status = vb.text()
		case 5:
			v.FileSystem = vb.text()
		case 6:
			v.TotalGB = number(vb)
		case 7:
			v.FreeGB = number(vb)
		}
	}
	for _, d := range disks {
		r.Disks = append(r.Disks, *d)
	}
	sort.Slice(r.Disks, func(i, j int) bool { return r.Disks[i].Index < r.Disks[j].Index })
	for _, v := range volumes {
		r.Volumes = append(r.Volumes, *v)
	}
	sort.Slice(r.Volumes, func(i, j int) bool { return r.Volumes[i].Index < r.Volumes[j].Index })
	return r, nil
}

// tableCell splits an object of a table (table.column.index) into its
// column and row index.
func tableCell(oid, table []uint32) (col, idx int, ok bool) {
	if !oidHasPrefix(oid, table) || len(oid) != len(table)+2 {
		return 0, 0, false
	}
	return int(oid[len(table)]), int(oid[len(table)+1]), true
}

func number(vb snmpVarbind) *int64 {
	if n, ok := vb.number(); ok {
		return &n
	}
	return nil
}

func printHealth(r *healthReport) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	line := func(k, v string) {
		if v != "" {
			fmt.Fprintf(w, "%s\t%s\n", k, v)
		}
	}
	line("Model", r.Model)
	line("ADM", r.ADMVersion)
	line("Serial", r.SerialNumber)
	line("Processor", r.Processor)
	if r.UptimeSeconds != nil {
		line("Uptime", formatDuration(time.Duration(*r.UptimeSeconds)*time.Second))
	}
	if r.SysTempC != nil {
		line("System temperature", fmt.Sprintf("%d °C", *r.SysTempC))
	}
	if r.CPUTempC != nil {
		line("CPU temperature", fmt.Sprintf("%d °C", *r.CPUTempC))
	}
	if len(r.CPUUsage) > 0 {
		parts := make([]string, len(r.CPUUsage))
		for i, u := range r.CPUUsage {
			parts[i] = fmt.Sprintf("%d%%", u)
		}
		line("CPU usage", strings.Join(parts, " "))
	}
	if r.TotalMemMB != nil && r.FreeMemMB != nil && *r.TotalMemMB > 0 {
		used := *r.TotalMemMB - *r.FreeMemMB
		line("Memory", fmt.Sprintf("%d of %d MB used (%d%%)", used, *r.TotalMemMB, used*100 / *r.TotalMemMB))
	}
	if len(r.FanRPM) > 0 {
		parts := make([]string, len(r.FanRPM))
		for i, f := range r.FanRPM {
			parts[i] = fmt.Sprintf("%d rpm", f)
		}
		line("Fans", strings.Join(parts, ", "))
	}
	w.Flush()
	if len(r.Disks) > 0 {
		fmt.Println("\nDISK\tMODEL\tSIZE\tTEMP\tSTATUS\tSMART")
		w = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, d := range r.Disks {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", d.Name, d.Model, gb(d.SizeGB), celsius(d.TempC), d.Status, d.Smart)
		}
		w.Flush()
	}
	if len(r.Volumes) > 0 {
		fmt.Println("\nVOLUME\tLEVEL\tFS\tUSED\tTOTAL\tSTATUS")
		w = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, v := range r.Volumes {
			used := ""
			if v.TotalGB != nil && v.FreeGB != nil {
				u := *v.TotalGB - *v.FreeGB
				used = gb(&u)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", v.Name, v.Level, v.FileSystem, used, gb(v.TotalGB), v.Status)
		}
		w.Flush()
	}
}

func gb(n *int64) string {
	if n == nil {
		return "-"
	}
	if *n >= 1000 {
		return fmt.Sprintf("%.1f TB", float64(*n)/1000)
	}
	return fmt.Sprintf("%d GB", *n)
}

func celsius(n *int64) string {
	if n == nil {
		return "-"
	}
	return fmt.Sprintf("%d °C", *n)
}
