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
// ASUSTOR MIB (enterprise 44738): system, temperatures, fans, CPU and
// memory, the disks, the volumes and the UPS. The same service that
// answers the uptime.
//
// ADM has shipped two layouts of that MIB. The older one keeps the
// temperatures and the memory as scalars under asustorHardware (.2.2 to
// .2.6), the CPU and fan tables at .2.7 and .2.8, the disk temperature in
// column 6 of the disk table and the SMART verdict in column 8, and the
// UPS as scalars under .6. The newer one (ADM 4) has the processor name
// at .2.2, the CPU, fan, temperature and memory tables at .2.3 to .2.6,
// the SMART verdict in column 6 of the disk table with the temperature
// and size after it, and a UPS table at .6.1. Which layout an agent
// speaks is told by the kind of value at .2.2.0: a string (the processor
// name) is the new one, an integer (the system temperature) the old one.

func asustorOID(sub ...uint32) []uint32 {
	return append([]uint32{1, 3, 6, 1, 4, 1, 44738}, sub...)
}

var (
	// asustorSystem, both layouts
	oidASSerial     = asustorOID(1, 1, 0)
	oidASADM        = asustorOID(1, 2, 0)
	oidASBios       = asustorOID(1, 3, 0)
	oidASUptimeText = asustorOID(1, 4, 0)
	oidASUpgrade    = asustorOID(1, 7, 0) // "Available" or "Unavailable"
	oidASModel      = asustorOID(2, 1, 0)
	// asustorHardware, old layout
	oidOldSysTemp   = asustorOID(2, 2, 0)
	oidOldCPUTemp   = asustorOID(2, 3, 0)
	oidOldTotalMem  = asustorOID(2, 4, 0) // MB
	oidOldFreeMem   = asustorOID(2, 5, 0) // MB
	oidOldProcessor = asustorOID(2, 6, 0)
	oidOldCPUUsage  = asustorOID(2, 7, 1, 1, 2) // percent per core
	oidOldFanSpeed  = asustorOID(2, 8, 1, 1, 2) // RPM
	// asustorHardware, new layout
	oidNewProcessor = asustorOID(2, 2, 0)
	oidNewCPUUsage  = asustorOID(2, 3, 1, 1, 2)
	oidNewFanSpeed  = asustorOID(2, 4, 1, 1, 2)
	oidNewTemps     = asustorOID(2, 5, 1, 1) // 2 cpu, 3 system (per sensor)
	oidNewMemory    = asustorOID(2, 6, 1, 1) // 2 total, 3 free, 4 free percent
	// the tables that both layouts share (the disk columns differ)
	oidASDisks   = asustorOID(4, 1, 1)
	oidASVolumes = asustorOID(5, 1, 1)
	// the UPS: a table in the new layout, scalars in the old one
	oidNewUps        = asustorOID(6, 1, 1) // 2 manufacturer 3 model 7 status 8 remaining s 9 charge % 11 input V
	oidOldUpsMaker   = asustorOID(6, 1, 0)
	oidOldUpsModel   = asustorOID(6, 2, 0)
	oidOldUpsStatus  = asustorOID(6, 6, 0)
	oidOldUpsCharge  = asustorOID(6, 7, 0)
	oidOldUpsRuntime = asustorOID(6, 10, 0)
	oidOldUpsVoltage = asustorOID(6, 16, 0)
)

type healthDisk struct {
	Index  int    `json:"index"`
	Name   string `json:"name"`
	Model  string `json:"model,omitempty"`
	Type   string `json:"type,omitempty"`
	Status string `json:"status,omitempty"` // Healthy, Normal, Risky, Dangerous, Bad
	Smart  string `json:"smart,omitempty"`
	TempC  *int64 `json:"temp_c,omitempty"`
	SizeGB *int64 `json:"size_gb,omitempty"`
}

type healthVolume struct {
	Index      int    `json:"index"`
	Name       string `json:"name"`
	Level      string `json:"level,omitempty"`  // single, jbod, raid0, raid1, raid10, raid5, raid6
	Status     string `json:"status,omitempty"` // healthy, clean, active, resyncing, ..., degraded, failed, inactive
	FileSystem string `json:"filesystem,omitempty"`
	TotalGB    *int64 `json:"total_gb,omitempty"`
	FreeGB     *int64 `json:"free_gb,omitempty"`
}

type healthUPS struct {
	Manufacturer   string `json:"manufacturer,omitempty"`
	Model          string `json:"model,omitempty"`
	Status         string `json:"status,omitempty"` // OL (on line), OB (on battery), LB (low battery), ...
	ChargePercent  *int64 `json:"charge_percent,omitempty"`
	RuntimeSeconds *int64 `json:"runtime_seconds,omitempty"`
	InputVoltage   *int64 `json:"input_voltage,omitempty"`
}

type healthReport struct {
	Name          string         `json:"name"`
	IP            string         `json:"ip"`
	Layout        string         `json:"mib_layout,omitempty"` // "old" or "new"
	Model         string         `json:"model,omitempty"`
	ADMVersion    string         `json:"adm_version,omitempty"`
	ADMUpdate     *bool          `json:"adm_update_available,omitempty"`
	BiosVersion   string         `json:"bios_version,omitempty"`
	SerialNumber  string         `json:"serial_number,omitempty"`
	Processor     string         `json:"processor,omitempty"`
	UptimeSeconds *int64         `json:"uptime_seconds,omitempty"`
	UptimeText    string         `json:"uptime_text,omitempty"`
	SysTempC      *int64         `json:"sys_temp_c,omitempty"`
	CPUTempC      *int64         `json:"cpu_temp_c,omitempty"`
	TotalMemMB    *int64         `json:"total_mem_mb,omitempty"`
	FreeMemMB     *int64         `json:"free_mem_mb,omitempty"`
	CPUUsage      []int64        `json:"cpu_usage,omitempty"` // percent per core
	FanRPM        []int64        `json:"fan_rpm,omitempty"`
	Disks         []healthDisk   `json:"disks"`
	Volumes       []healthVolume `json:"volumes"`
	UPS           *healthUPS     `json:"ups,omitempty"`
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

	scalars := [][]uint32{oidASSerial, oidASADM, oidASBios, oidASUptimeText, oidASUpgrade, oidASModel,
		oidOldSysTemp, oidOldCPUTemp, oidOldTotalMem, oidOldFreeMem, oidOldProcessor,
		oidHrSystemUptime, oidSysUpTime}
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
	values := map[string]snmpVarbind{}
	for _, vb := range resp.varbinds {
		if vb.tag == tagNoSuchObject || vb.tag == tagNoSuchInstance || vb.tag == tagEndOfMibView {
			continue
		}
		values[oidString(vb.oid)] = vb
	}
	get := func(oid []uint32) (snmpVarbind, bool) {
		vb, ok := values[oidString(oid)]
		return vb, ok
	}
	text := func(oid []uint32) string {
		vb, _ := get(oid)
		return vb.text()
	}
	num := func(oid []uint32) *int64 {
		if vb, ok := get(oid); ok {
			return number(vb)
		}
		return nil
	}
	if vb, ok := get(oidHrSystemUptime); ok {
		if ticks, ok := vb.number(); ok {
			secs := ticks / 100
			r.UptimeSeconds = &secs
		}
	}
	if vb, ok := get(oidSysUpTime); ok && r.UptimeSeconds == nil {
		if ticks, ok := vb.number(); ok {
			secs := ticks / 100
			r.UptimeSeconds = &secs
		}
	}
	asustor := false
	for _, oid := range scalars[:11] {
		if _, ok := get(oid); ok {
			asustor = true
		}
	}
	if !asustor {
		return r, errNoAsustorMIB
	}
	r.SerialNumber, r.ADMVersion, r.BiosVersion = text(oidASSerial), text(oidASADM), text(oidASBios)
	r.UptimeText, r.Model = text(oidASUptimeText), text(oidASModel)
	switch strings.ToLower(text(oidASUpgrade)) {
	case "available":
		yes := true
		r.ADMUpdate = &yes
	case "unavailable":
		no := false
		r.ADMUpdate = &no
	}

	walk := func(root []uint32) []snmpVarbind {
		vbs, err := snmpWalk(conn, snmpV2c, community, root, timeout)
		if err != nil {
			vlog("snmp walk %v: %v", root, err)
		}
		return vbs
	}
	numbers := func(root []uint32) []int64 {
		var out []int64
		for _, vb := range walk(root) {
			if n, ok := vb.number(); ok {
				out = append(out, n)
			}
		}
		return out
	}

	if vb, ok := get(oidNewProcessor); ok && vb.tag == tagOctetString {
		r.Layout = "new"
		r.Processor = vb.text()
		r.CPUUsage = numbers(oidNewCPUUsage)
		r.FanRPM = numbers(oidNewFanSpeed)
		for _, row := range tableRows(walk(oidNewTemps), oidNewTemps) {
			if r.CPUTempC == nil {
				r.CPUTempC = row.num(2)
			}
			if r.SysTempC == nil {
				r.SysTempC = row.num(3)
			}
		}
		for _, row := range tableRows(walk(oidNewMemory), oidNewMemory) {
			if r.TotalMemMB == nil {
				r.TotalMemMB, r.FreeMemMB = memoryMB(row.num(2)), memoryMB(row.num(3))
			}
		}
	} else {
		r.Layout = "old"
		r.Processor = text(oidOldProcessor)
		r.SysTempC, r.CPUTempC = num(oidOldSysTemp), num(oidOldCPUTemp)
		r.TotalMemMB, r.FreeMemMB = memoryMB(num(oidOldTotalMem)), memoryMB(num(oidOldFreeMem))
		r.CPUUsage = numbers(oldOrNew(get, oidOldCPUUsage))
		r.FanRPM = numbers(oidOldFanSpeed)
	}

	diskRows := tableRows(walk(oidASDisks), oidASDisks)
	// column 6 holds the SMART verdict (a string) in the new layout and
	// the temperature (an integer) in the old one
	newDisks := false
	for _, row := range diskRows {
		if vb, ok := row.cells[6]; ok && vb.tag == tagOctetString {
			newDisks = true
		}
	}
	for _, row := range diskRows {
		d := healthDisk{Index: row.index, Name: row.text(2), Model: row.text(3), Type: row.text(4),
			Status: row.text(5)}
		if newDisks {
			d.Smart, d.TempC, d.SizeGB = row.text(6), row.num(7), row.num(8)
		} else {
			d.TempC, d.SizeGB, d.Smart = row.num(6), row.num(7), row.text(8)
		}
		if d.Name == "" {
			d.Name = fmt.Sprintf("Disk %d", row.index)
		}
		r.Disks = append(r.Disks, d)
	}
	for _, row := range tableRows(walk(oidASVolumes), oidASVolumes) {
		v := healthVolume{Index: row.index, Name: row.text(2), Level: row.text(3), Status: row.text(4),
			FileSystem: row.text(5), TotalGB: row.num(6), FreeGB: row.num(7)}
		if v.Name == "" {
			v.Name = fmt.Sprintf("Volume %d", row.index)
		}
		r.Volumes = append(r.Volumes, v)
	}

	if r.Layout == "new" {
		for _, row := range tableRows(walk(oidNewUps), oidNewUps) {
			ups := &healthUPS{Manufacturer: row.text(2), Model: row.text(3), Status: row.text(7),
				RuntimeSeconds: row.num(8), ChargePercent: row.num(9), InputVoltage: row.num(11)}
			if ups.Model != "" || ups.Status != "" {
				r.UPS = ups
				break
			}
		}
	} else {
		oids := [][]uint32{oidOldUpsMaker, oidOldUpsModel, oidOldUpsStatus, oidOldUpsCharge,
			oidOldUpsRuntime, oidOldUpsVoltage}
		if resp, err := snmpGet(conn, snmpV2c, community, oids, timeout); err == nil && resp.errorStatus == 0 {
			ups := &healthUPS{}
			for _, vb := range resp.varbinds {
				switch {
				case oidEqual(vb.oid, oidOldUpsMaker):
					ups.Manufacturer = vb.text()
				case oidEqual(vb.oid, oidOldUpsModel):
					ups.Model = vb.text()
				case oidEqual(vb.oid, oidOldUpsStatus):
					ups.Status = vb.text()
				case oidEqual(vb.oid, oidOldUpsCharge):
					ups.ChargePercent = number(vb)
				case oidEqual(vb.oid, oidOldUpsRuntime):
					ups.RuntimeSeconds = number(vb)
				case oidEqual(vb.oid, oidOldUpsVoltage):
					ups.InputVoltage = number(vb)
				}
			}
			if ups.Model != "" || ups.Status != "" {
				r.UPS = ups
			}
		}
	}
	return r, nil
}

// oldOrNew is a hook for agents that mix the layouts: the old CPU table
// is asked for first; nothing else is needed today.
func oldOrNew(_ func([]uint32) (snmpVarbind, bool), old []uint32) []uint32 { return old }

// memoryMB normalizes a memory figure to megabytes: the old MIB says MB,
// and an agent that reports kilobytes is recognized by the magnitude.
func memoryMB(n *int64) *int64 {
	if n == nil {
		return nil
	}
	if *n > 4_000_000 { // more than 4 TB of RAM is not a NAS: kilobytes
		mb := *n / 1024
		return &mb
	}
	return n
}

// tableRow is one row of an SNMP table: the cells by column number.
type tableRow struct {
	index int
	cells map[int]snmpVarbind
}

func (r tableRow) text(col int) string {
	return r.cells[col].text()
}

func (r tableRow) num(col int) *int64 {
	if vb, ok := r.cells[col]; ok {
		return number(vb)
	}
	return nil
}

// tableRows groups the objects of a walk over a table (table.column.index)
// into rows, ordered by index.
func tableRows(objects []snmpVarbind, table []uint32) []tableRow {
	byIndex := map[int]*tableRow{}
	for _, vb := range objects {
		if !oidHasPrefix(vb.oid, table) || len(vb.oid) != len(table)+2 {
			continue
		}
		col, idx := int(vb.oid[len(table)]), int(vb.oid[len(table)+1])
		row := byIndex[idx]
		if row == nil {
			row = &tableRow{index: idx, cells: map[int]snmpVarbind{}}
			byIndex[idx] = row
		}
		row.cells[col] = vb
	}
	rows := make([]tableRow, 0, len(byIndex))
	for _, row := range byIndex {
		rows = append(rows, *row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].index < rows[j].index })
	return rows
}

func oidString(oid []uint32) string {
	parts := make([]string, len(oid))
	for i, n := range oid {
		parts[i] = fmt.Sprint(n)
	}
	return strings.Join(parts, ".")
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
	line("Processor", r.Processor)
	adm := r.ADMVersion
	if r.ADMUpdate != nil && *r.ADMUpdate {
		adm += " (an update is available)"
	}
	line("ADM", adm)
	line("BIOS", r.BiosVersion)
	line("Serial", r.SerialNumber)
	if r.UptimeSeconds != nil {
		line("Uptime", formatDuration(time.Duration(*r.UptimeSeconds)*time.Second))
	} else {
		line("Uptime", r.UptimeText)
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
	line("System temperature", celsius(r.SysTempC))
	line("CPU temperature", celsius(r.CPUTempC))
	if len(r.FanRPM) > 0 {
		parts := make([]string, len(r.FanRPM))
		for i, f := range r.FanRPM {
			parts[i] = fmt.Sprintf("%d rpm", f)
		}
		line("Fans", strings.Join(parts, ", "))
	}
	if u := r.UPS; u != nil {
		line("UPS", strings.TrimSpace(u.Manufacturer+" "+u.Model))
		line("UPS status", upsStatusText(u.Status))
		if u.ChargePercent != nil {
			charge := fmt.Sprintf("%d%%", *u.ChargePercent)
			if u.RuntimeSeconds != nil {
				charge += fmt.Sprintf(", %s left", formatDuration(time.Duration(*u.RuntimeSeconds)*time.Second))
			}
			line("UPS battery", charge)
		}
		if u.InputVoltage != nil {
			line("UPS input", fmt.Sprintf("%d V", *u.InputVoltage))
		}
	}
	w.Flush()
	if len(r.Disks) > 0 {
		w = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "\nDISK\tMODEL\tTYPE\tSIZE\tTEMP\tSTATUS\tSMART")
		for _, d := range r.Disks {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", d.Name, d.Model, d.Type, gb(d.SizeGB), celsius(d.TempC), d.Status, d.Smart)
		}
		w.Flush()
	}
	if len(r.Volumes) > 0 {
		w = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "\nVOLUME\tLEVEL\tFS\tUSED\tTOTAL\tSTATUS")
		for _, v := range r.Volumes {
			used := "-"
			if v.TotalGB != nil && v.FreeGB != nil {
				u := *v.TotalGB - *v.FreeGB
				used = gb(&u)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", v.Name, v.Level, v.FileSystem, used, gb(v.TotalGB), v.Status)
		}
		w.Flush()
	}
}

// upsStatusText spells out the NUT status flags ADM reports.
func upsStatusText(status string) string {
	names := map[string]string{"OL": "on line", "OB": "on battery", "LB": "low battery",
		"HB": "high battery", "RB": "replace battery", "CHRG": "charging", "DISCHRG": "discharging",
		"BYPASS": "bypass", "CAL": "calibrating", "OFF": "off", "OVER": "overloaded", "TRIM": "trimming",
		"BOOST": "boosting", "FSD": "forced shutdown"}
	var parts []string
	for _, flag := range strings.Fields(status) {
		if name, ok := names[strings.ToUpper(flag)]; ok {
			parts = append(parts, name)
		} else {
			parts = append(parts, flag)
		}
	}
	return strings.Join(parts, ", ")
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
		return ""
	}
	return fmt.Sprintf("%d °C", *n)
}
