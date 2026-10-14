# <img src="assets/icon-128.png" alt="" width="48" align="absmiddle" /> Asustor Control Center for Linux

A Linux version of the ASUSTOR Control Center (ACC): find ASUSTOR NAS
devices on your LAN, open the ADM web UI or the shared folders, send
Wake-on-LAN, and see each NAS's uptime. Comes as a dependency-free CLI
(`acc`) and a GTK4 GUI (`acc-gui`).

> Unofficial community project. Not affiliated with, endorsed by, or
> supported by ASUSTOR Inc. "ASUSTOR" and "ADM" are trademarks of ASUSTOR
> Inc., used here only to describe compatibility.

![acc-gui in dark mode: device table with four NAS online and one remembered offline device](assets/screenshot.png)

ASUSTOR ships Control Center for Windows and macOS only. This project
speaks the same discovery protocol, natively on Linux.

## Download

[![Latest release](https://img.shields.io/github/v/release/moringa303/asustor-acc-linux?label=latest%20release&style=for-the-badge)](https://github.com/moringa303/asustor-acc-linux/releases/latest)

| Distribution | Package | Install |
|---|---|---|
| Debian, Ubuntu, Mint, Pop!_OS | [**asustor-acc-linux_amd64.deb**](https://github.com/moringa303/asustor-acc-linux/releases/latest/download/asustor-acc-linux_amd64.deb) | `sudo apt install ./asustor-acc-linux_amd64.deb` |
| Fedora, RHEL, openSUSE | [**asustor-acc-linux.x86_64.rpm**](https://github.com/moringa303/asustor-acc-linux/releases/latest/download/asustor-acc-linux.x86_64.rpm) | `sudo dnf install ./asustor-acc-linux.x86_64.rpm` |
| Arch, EndeavourOS, Manjaro, others | [**asustor-acc-linux-linux-amd64.tar.gz**](https://github.com/moringa303/asustor-acc-linux/releases/latest/download/asustor-acc-linux-linux-amd64.tar.gz) | extract, then `./install.sh` |
| CLI only, any x86-64 Linux | [**acc**](https://github.com/moringa303/asustor-acc-linux/releases/latest/download/acc) (static binary) | `chmod +x acc && sudo install acc /usr/local/bin/` |

Checksums: [SHA256SUMS](https://github.com/moringa303/asustor-acc-linux/releases/latest/download/SHA256SUMS).
All versions: [releases page](https://github.com/moringa303/asustor-acc-linux/releases).

## Features

- `acc`: single static binary, works on any x86-64 Linux distribution:

  ```
  acc scan                  # discover ASUSTOR NAS devices (table)
  acc scan --json           # machine-readable output
  acc scan --all            # also list remembered devices that are offline
  acc scan --watch          # live view, rescans every 30 s
  acc open   <name|ip|mac>  # open the ADM web UI in your browser
  acc shares <name|ip|mac>  # open the shared folders (smb://) in your file manager
  acc url    <name|ip|mac>  # print the ADM URL
  acc wol    <name|ip|mac>  # send a Wake-on-LAN magic packet
  acc doctor                # diagnose "no device found" problems
  ```

- `acc-gui`: GTK4 front-end with the familiar device table
  (Model / Name / IP / Serial / MAC / ADM / Port / State), each model
  with a small glyph of its form factor (tower, rack, flash), Scan, Ping,
  Open ADM and Wake buttons, a link button that opens the selected
  model's HDD compatibility list (SSD list for the all-flash FS models)
  from asustor.com in a window of the app (see
  [Links to asustor.com](#links-to-asustorcom)), a right-click menu (open shared folders,
  copy the IP or MAC, forget a device), sortable columns, a search bar
  (Ctrl+F, or just start typing: name, IP, model, serial, MAC and state
  are matched), and automatic refresh every 30 seconds. The table is the
  layout from 1000 px of window width up (cells shorten with an ellipsis
  and show the full text as a tooltip, never a horizontal scrollbar; the
  Model and State columns are always shown whole); narrower windows get a
  card list. Offline devices stay in the list, dimmed, so you can wake
  them. A NAS whose answer to one scan got lost (it happens on a segment
  with hundreds of devices) keeps its place as online until it has been
  missing for 2½ minutes, so the list does not flicker.

- Uptime: read from the NAS via SNMP, shown in the CLI's UPTIME column
  and as the tooltip of the GUI's State cell (see [Uptime](#uptime)).

- Device memory: `acc` remembers every NAS it has seen. A NAS that is
  asleep can still be woken with `acc wol <name>` or the GUI's Wake
  button, and shows up as Offline with the time it was last seen. A
  remembered NAS that misses a scan gets a second look, and one that
  still answers SNMP counts as online even when its mDNS answer was lost.

- `acc doctor`: mDNS health check that tells you whether nothing answers
  because of your firewall, your network topology, or a local mDNS daemon
  conflict, with the fix for each case.

## Install

### From a release

Use the links under [Download](#download):

- Debian / Ubuntu: `sudo apt install ./asustor-acc-linux_amd64.deb`
- Fedora: `sudo dnf install ./asustor-acc-linux.x86_64.rpm`
- Arch / EndeavourOS and others: extract the `.tar.gz`, run `./install.sh`

### From source

```sh
git clone https://github.com/moringa303/asustor-acc-linux && cd asustor-acc-linux
go build -trimpath -ldflags="-s -w" -o acc ./cmd/acc   # CGO_ENABLED=0 for fully static
sudo install -Dm755 acc /usr/local/bin/acc
# GUI (optional): needs gtk4 + python-gobject; libadwaita is used when present
sudo install -Dm755 gui/acc-gui.py /usr/local/bin/acc-gui
```

## Build and usage

### Building

Requires Go 1.25 or newer. No cgo, no system libraries.

```sh
go build ./...                                          # build everything
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
    -o acc ./cmd/acc                                    # static release binary
go vet ./... && go test ./...                           # checks
```

The GUI is a single Python script and needs no build step; it requires
GTK4 and PyGObject at runtime, and the `acc` binary next to it or on your
`PATH` (or in `ACC_BIN`).

### Usage

```
acc <command> [options] [argument]
```

| Command | What it does |
|---|---|
| `scan` | Discover NAS devices on the LAN and print a table |
| `open <name\|ip\|mac>` | Open the ADM web UI (or setup wizard) in the browser |
| `shares <name\|ip\|mac>` | Open the NAS's shared folders (`smb://<ip>/`) in the file manager |
| `url <name\|ip\|mac>` | Print the ADM URL |
| `wol <name\|ip\|mac>` | Send a Wake-on-LAN magic packet; works for a sleeping NAS that `acc` has seen before |
| `forget <name\|ip\|mac>` | Drop a NAS from the device memory (`--all`: every one) |
| `doctor` | Diagnose why discovery finds nothing |
| `version` | Print the version |

| Option | Meaning |
|---|---|
| `--json` | Machine-readable output for `scan` |
| `--all` | Also list remembered devices that did not answer |
| `--watch` | Rescan continuously and redraw the table |
| `--interval <seconds>` | Rescan interval for `--watch` (default 30) |
| `--timeout <seconds>` | How long to listen for answers (default 4) |
| `--iface <name>` | Restrict scanning and Wake-on-LAN to one network interface |
| `--community <string>` | SNMP community used to read the uptime (default `public`, or `$ACC_SNMP_COMMUNITY`) |
| `--no-snmp` | Do not ask the NAS for its uptime |
| `--verbose`, `-v` | Explain what is going on, on stderr |

Examples:

```sh
acc scan --timeout 8 --iface eth0
acc scan --all              # includes offline devices and when they were last seen
acc open mynas              # device name as shown by `acc scan`
acc shares mynas            # smb://<ip>/ in your file manager
acc wol mynas               # no scan needed: the MAC comes from the device memory
acc wol 00:11:32:aa:bb:cc
acc-gui                     # GUI; also available from the app menu
```

To try it without hardware, run `go run ./cmd/fakenas` (add `-init` for
an uninitialized NAS, `-name`/`-model`/`-mac` for a second one) in one
terminal and `go run ./cmd/acc scan` in another.

### Uptime

The discovery protocol does not carry the uptime, so `acc` asks the NAS
over SNMP: `hrSystemUptime` (HOST-RESOURCES-MIB), falling back to
`sysUpTime`. The CLI prints it in the UPTIME column; the GUI shows it
when you hover over a device's State. For that, enable SNMP on the NAS once: ADM › Services ›
SNMP, tick SNMP v1/v2c, community `public` (or pass yours with
`--community`). Without SNMP the column shows `-` and everything else
works as before. The query runs alongside discovery, so a NAS without
SNMP does not slow the scan down; `--no-snmp` turns it off entirely.

The device memory lives in `~/.cache/asustor-acc/devices.json`
(`$XDG_CACHE_HOME`). `acc forget` removes entries; devices not seen for
180 days drop out by themselves.

## How it works (protocol)

ASUSTOR NAS devices announce themselves via standard mDNS/DNS-SD
(RFC 6762/6763):

| Service type | Meaning |
|---|---|
| `_ASUSTOR_ADM._tcp.local` | initialized NAS running ADM |
| `_ASUSTOR_INIT._tcp.local` | factory-fresh NAS awaiting initialization |

The SRV record carries the ADM web port (default 8000). TXT records
carry device details:

```
model=AS5304T  version=4.3.3.RC92  serialnumber=...  hostid=00-11-32-xx-xx-xx
state=inited   httpenabled=Yes     httpsport=8001    wol=Yes  admupdate=No
bios=...  appupdatable=...  hasabnormalvolume=...  ifnames=eth0
```

`hostid` is the MAC address, dash-separated. The wire state is shown as
one of three statuses: `inited` is "Ready", `uninited` is
"Uninitialized", anything else is "Not ready". For an uninitialized NAS,
`acc open` and the GUI's Open button go to its setup wizard at
`http://<ip>:8000`. Discovery works with any mDNS stack;
`avahi-browse -r _ASUSTOR_ADM._tcp` shows the same data. `acc` runs its
own mDNS resolver and, when `avahi-browse` is installed, asks the local
avahi-daemon at the same time and merges both answers, because a daemon
that owns the mDNS socket swallows the unicast replies a standalone
listener would need. Hosts with IPv6 disabled are scanned over IPv4.

Uptime comes from a single SNMP GET (v2c, then v1) to UDP port 161 of
the NAS; `acc` carries its own minimal SNMP client, no library needed.

Wake-on-LAN sends the magic packet to the limited broadcast address, to
the directed broadcast of every interface (or only `--iface`), and to the
last known address of the NAS, so a NAS on a second network card is
reached too.

## Troubleshooting: an old version keeps running

The About dialog shows the versions of both `acc-gui` and the `acc`
binary it found, and the status bar warns when they differ. A leftover
copy from an earlier `install` to `/usr/local/bin` is started before a
packaged one in `/usr/bin`: `which -a acc acc-gui` lists every copy,
remove the old ones.

"Open shared folders" (and `acc shares`) opens `smb://<ip>/` in your file
manager: through the desktop's registered handler when there is one,
otherwise by starting the running desktop's file manager (Nautilus,
Dolphin, Thunar, Nemo, Caja, PCManFM, elementary Files, Deepin) with the
address, then any other installed one, then `xdg-open`. Browsing the
share needs the SMB backend of your desktop (`gvfs-smb` on GNOME,
Cinnamon and XFCE, `kio-extras` on KDE), which the packages recommend.
When nothing can open it, the status bar says why and the address is on
the clipboard, ready to paste into the file manager's location bar
(Ctrl+L).

## Links to asustor.com

The link button (and the matching entry of the right-click menu) opens
the selected model's compatibility list: HDD for AS models, SSD for the
all-flash FS models. It opens in a window of the app, with back, forward
and reload buttons and one that hands the page to your browser; the
window works at any width from 360 px up. That needs the WebKitGTK 6.0
bindings, which the packages recommend (`gir1.2-webkit-6.0` on Debian
and Ubuntu, `webkitgtk6.0` on Fedora, `webkitgtk-6.0` on Arch); without
them the list opens in the browser. The addresses are
`https://www.asustor.com/service/hd?id=hd` (HDD) and
`https://www.asustor.com/service/m2ssd?id=m2ssd` (M.2 SSD); pick the NAS
series in the page's own filter. To use other addresses, create
`~/.config/asustor-acc/links.json`:

```json
{
  "hdd": "https://www.asustor.com/service/hd?id=hd&series_id=1",
  "ssd": "https://www.asustor.com/service/m2ssd?id=m2ssd",
  "models": {"AS6704T": {"compat": "https://..."}}
}
```

`{model}` and `{media}` (`HDD` or `SSD`) are substituted in any of the
addresses; entries under `models` override the address for one model.

## Troubleshooting: "No devices found"

Run `acc doctor`. The most common cause is the firewall dropping mDNS
(UDP 5353 multicast):

- firewalld (EndeavourOS, Arch, Fedora, RHEL, openSUSE):
  `sudo firewall-cmd --add-service=mdns --permanent && sudo firewall-cmd --reload`
- ufw (Ubuntu, Mint, Pop!_OS): `sudo ufw allow 5353/udp`
- nftables/iptables: allow UDP dport 5353 in your input chain.

mDNS does not cross routers: the NAS must be on the same subnet/VLAN.

## Not implemented

ADM firmware push-updates, NAS network reconfiguration, and USB-over-IP.
The first two are done in the ADM web UI anyway. For USB-over-IP, Linux
has native kernel support (`usbip`, `modprobe vhci-hcd`); the Windows
app bundles a driver for what the kernel already provides.

## Development

- `cmd/acc/`: Go CLI (discovery, device memory, SNMP uptime, doctor, WOL)
- `cmd/fakenas/`: advertises a fake NAS over mDNS so everything is
  testable without hardware. `go run ./cmd/fakenas` in one terminal,
  `acc scan` in another. `-init` advertises a factory-fresh NAS.
- `gui/acc-gui.py`: GTK4 GUI (calls `acc scan --json --all`). Run it
  from the checkout with `ACC_BIN=./acc python3 gui/acc-gui.py`.
- `assets/icon.svg`: icon source, rendered to the PNG sizes with
  `rsvg-convert`
- `go test ./...` runs the unit tests; CI runs them with `go vet`,
  `gofmt`, `staticcheck` and `pyflakes` on every push.

The discovery protocol was determined by inspecting the official Control
Center's scanner library. No ASUSTOR code or assets are included in this
project.

## Support

Provided as-is, no support, no active maintenance. Issues are disabled
on purpose. Fork freely, the MIT license lets you do anything you want
with it.

## License

[MIT](LICENSE). Uses [grandcat/zeroconf](https://github.com/grandcat/zeroconf) (MIT).
