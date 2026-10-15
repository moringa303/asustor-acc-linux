# ASUSTOR SNMP MIBs

The MIB files ASUSTOR ships with ADM, unmodified, in the two layouts
that exist. `acc health` and the GUI's Details window read both; they
are kept here as the reference for the object ids in `cmd/acc/health.go`
and for loading into a network monitoring system.

| Directory | ADM | Layout |
|---|---|---|
| `older/` | older ADM releases | Temperatures and memory are single values under `asustorHardware` (`.2.2` to `.2.6`), the CPU and fan tables sit at `.2.7` and `.2.8`, the disk table has the temperature in column 6 and the SMART verdict in column 8, the UPS is a set of single values under `.6`. |
| `newer/` | ADM 4 and later | The processor name is at `.2.2`; CPU, fans, temperatures and memory are tables at `.2.3` to `.2.6`; the disk table has the SMART verdict in column 6 with the temperature and size after it; the UPS is a table at `.6.1`. |

Both share `asustorSystem` (`.1`: serial number, ADM version, BIOS,
uptime, time, time zone, whether an ADM update is available, ASUSTOR ID)
and the volume table (`.5.1.1`).

`acc health` tells the layouts apart by the kind of value at
`1.3.6.1.4.1.44738.2.2.0`: a string (the processor name) means the newer
layout, an integer (the system temperature) the older one. `--mib old`
or `--mib new` (the "SNMP MIB layout" setting of the GUI's Preferences)
forces one when a NAS answers unexpectedly.

The files are ASUSTOR's; the enterprise number 44738 is theirs.
