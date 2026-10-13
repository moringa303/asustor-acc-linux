# Asustor ACC for Linux

A Linux version of the ASUSTOR Control Center (ACC): find ASUSTOR NAS
devices on your LAN, open the ADM web UI, send Wake-on-LAN, and see for
how long each NAS has been online. Comes as a dependency-free CLI
(`acc`) and a GTK4 GUI (`acc-gui`).

> Unofficial community project. Not affiliated with, endorsed by, or
> supported by ASUSTOR Inc. "ASUSTOR" and "ADM" are trademarks of ASUSTOR
> Inc., used here only to describe compatibility.

![icon](assets/icon-128.png)

![acc-gui: device table with three NAS online and two remembered offline devices](assets/screenshot.png)

ASUSTOR ships Control Center for Windows and macOS only. This project
speaks the same discovery protocol, natively on Linux.

## Features

- `acc`: single static binary, works on any x86-64 Linux distribution:

  ```
  acc scan                  # discover ASUSTOR NAS devices (table)
  acc scan --json           # machine-readable output
  acc scan --all            # also list remembered devices that are offline
  acc scan --watch          # live view, rescans every 30 s
  acc open  <name|ip|mac>   # open the ADM web UI in your browser
  acc url   <name|ip|mac>   # print the ADM URL
  acc wol   <name|ip|mac>   # send a Wake-on-LAN magic packet
  acc doctor                # diagnose "no device found" problems
  ```

- `acc-gui`: GTK4 front-end with the familiar device table
  (Name / IP / Model / Serial / MAC / ADM / Port / State / Online), Scan,
  Open ADM and Wake buttons, a right-click menu (copy the IP or MAC,
  forget a device), sortable columns, and automatic refresh every 30
  seconds. Offline devices stay in the list, dimmed, so you can wake them.

- Device memory: `acc` remembers every NAS it has seen. A NAS that is
  asleep can still be woken with `acc wol <name>` or the GUI's Wake
  button, and shows up as Offline with the time it was last seen.

- Online time: for every NAS, for how long it has answered every scan
  without a miss (the ONLINE column; see [Online time](#online-time)).

- `acc doctor`: mDNS health check that tells you whether nothing answers
  because of your firewall, your network topology, or a local mDNS daemon
  conflict, with the fix for each case.

## Install

### From a release

Download the latest release asset for your distro:

- Debian / Ubuntu: `sudo apt install ./asustor-acc-linux_*_amd64.deb`
- Fedora: `sudo dnf install ./asustor-acc-linux-*.x86_64.rpm`
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
go vet ./...                                            # checks
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
| `--verbose`, `-v` | Explain what is going on, on stderr |

Examples:

```sh
acc scan --timeout 8 --iface eth0
acc scan --all              # includes offline devices and when they were last seen
acc open mynas              # device name as shown by `acc scan`
acc wol mynas               # no scan needed: the MAC comes from the device memory
acc wol 00:11:32:aa:bb:cc
acc-gui                     # GUI; also available from the app menu
```

To try it without hardware, run `go run ./cmd/fakenas` (add `-init` for
an uninitialized NAS, `-name`/`-model`/`-mac` for a second one) in one
terminal and `go run ./cmd/acc scan` in another.

### Online time

ADM does not announce its uptime, so `acc` keeps track itself. The
ONLINE column (Online in the GUI) is for how long the NAS has answered
every scan, counted from the first scan of an unbroken run. A scan that
gets no answer ends the run; `acc` looks a second time before concluding
that, because mDNS answers do get lost occasionally. With `acc scan
--watch` or the GUI's automatic refresh this is a live "reachable for"
counter; with occasional manual scans it means "present at every scan
since". Offline devices show when they were last seen instead.

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

Wake-on-LAN sends the magic packet to the limited broadcast address, to
the directed broadcast of every interface (or only `--iface`), and to the
last known address of the NAS, so a NAS on a second network card is
reached too.

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

- `cmd/acc/`: Go CLI (discovery, device memory, doctor, WOL)
- `cmd/fakenas/`: advertises a fake NAS over mDNS so everything is
  testable without hardware. `go run ./cmd/fakenas` in one terminal,
  `acc scan` in another. `-init` advertises a factory-fresh NAS.
- `gui/acc-gui.py`: GTK4 GUI (calls `acc scan --json --all`). Run it
  from the checkout with `ACC_BIN=./acc python3 gui/acc-gui.py`.
- `assets/icon.svg`: icon source, rendered to the PNG sizes with
  `rsvg-convert`

The discovery protocol was determined by inspecting the official Control
Center's scanner library. No ASUSTOR code or assets are included in this
project.

## Support

Provided as-is, no support, no active maintenance. Issues are disabled
on purpose. Fork freely, the MIT license lets you do anything you want
with it.

## License

[MIT](LICENSE). Uses [grandcat/zeroconf](https://github.com/grandcat/zeroconf) (MIT).
