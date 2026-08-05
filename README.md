# Asustor ACC for Linux

**Native Linux replacement for ASUSTOR Control Center** — discover ASUSTOR
NAS devices on your LAN, open the ADM web UI, and send Wake-on-LAN, from a
dependency-free CLI or a GTK4 GUI.

> Unofficial community project. Not affiliated with, endorsed by, or
> supported by ASUSTOR Inc. "ASUSTOR" and "ADM" are trademarks of ASUSTOR
> Inc., used here only to describe compatibility.

![icon](assets/icon-128.png)

ASUSTOR builds some of the best NAS systems around — quiet, capable
hardware with the polished ADM operating system on top. The one gap: its
Control Center companion app only ships for Windows and macOS. This project
completes the picture, reimplementing Control Center's core functionality
natively for Linux and speaking the exact same discovery protocol the
official app uses: find every ASUSTOR NAS on your network in seconds, jump
straight into ADM, or wake a sleeping box with one command.

## Features

- **`acc`** — single static binary, zero dependencies, works on any x86-64
  Linux distribution:

  ```
  acc scan                  # discover ASUSTOR NAS devices (table)
  acc scan --json           # machine-readable output
  acc open  <name|ip>       # open the ADM web UI in your browser
  acc url   <name|ip>       # print the ADM URL
  acc wol   <name|ip|mac>   # send a Wake-on-LAN magic packet
  acc doctor                # diagnose "no device found" problems
  ```

- **`acc-gui`** — GTK4 front-end with the familiar device table
  (Name / IP / Model / Serial / MAC / ADM version) and Scan, Open ADM,
  Wake buttons.

- **`acc doctor`** — low-level mDNS health check that tells you whether
  nothing answers because of your firewall, your network topology, or a
  local mDNS daemon conflict, with the exact fix for each case.

## Install

### From a release

Download the latest release asset for your distro:

- **Debian / Ubuntu**: `sudo apt install ./asustor-acc-linux_*_amd64.deb`
- **Fedora**: `sudo dnf install ./asustor-acc-linux-*.x86_64.rpm`
- **Arch / EndeavourOS and others**: extract the `.tar.gz`, run `./install.sh`

### From source

```sh
git clone https://github.com/moringa303/asustor-acc-linux && cd asustor-acc-linux
go build -trimpath -ldflags="-s -w" -o acc ./cmd/acc   # CGO_ENABLED=0 for fully static
sudo install -Dm755 acc /usr/local/bin/acc
# GUI (optional): needs gtk4 + python-gobject
sudo install -Dm755 gui/acc-gui.py /usr/local/bin/acc-gui
```

## How it works (protocol)

ASUSTOR NAS devices announce themselves via standard **mDNS/DNS-SD**
(RFC 6762/6763):

| Service type | Meaning |
|---|---|
| `_ASUSTOR_ADM._tcp.local` | initialized NAS running ADM |
| `_ASUSTOR_INIT._tcp.local` | factory-fresh NAS awaiting initialization |

The **SRV** record carries the ADM web port (default 8000). **TXT** records
carry device details:

```
model=AS5304T  version=4.3.3.RC92  serialnumber=...  hostid=00-11-32-xx-xx-xx
state=inited   httpenabled=Yes     httpsport=8001    wol=Yes  admupdate=No
bios=...  appupdatable=...  hasabnormalvolume=...  ifnames=eth0
```

(`hostid` is the MAC address, dash-separated.) Discovery therefore works
with any mDNS stack — `avahi-browse -r _ASUSTOR_ADM._tcp` shows the same
data. `acc` uses its own mDNS resolver and automatically falls back to
`avahi-browse` when a local avahi-daemon owns the mDNS socket.

## Troubleshooting: "No devices found"

Run `acc doctor`. Most common cause is the firewall dropping mDNS
(UDP 5353 multicast):

- **firewalld** (EndeavourOS, Arch, Fedora, RHEL, openSUSE):
  `sudo firewall-cmd --add-service=mdns --permanent && sudo firewall-cmd --reload`
- **ufw** (Ubuntu, Mint, Pop!_OS): `sudo ufw allow 5353/udp`
- **nftables/iptables**: allow UDP dport 5353 in your input chain.

mDNS does not cross routers: the NAS must be on the same subnet/VLAN.

## Not implemented

ADM firmware push-updates, NAS network reconfiguration, and USB-over-IP.
The first two are done in the ADM web UI anyway. For USB-over-IP, Linux has
native kernel support (`usbip`, `modprobe vhci-hcd`) — the Windows app
merely bundles a driver for what the kernel already provides.

## Development

- `cmd/acc/` — Go CLI (discovery, doctor, WOL)
- `cmd/fakenas/` — test helper that advertises a fake NAS over mDNS, so
  everything is testable without hardware: `go run ./cmd/fakenas` in one
  terminal, `acc scan` in another
- `gui/acc-gui.py` — GTK4 GUI (calls `acc scan --json`)
- `assets/icon.svg` — original icon (source of all rendered sizes)

The discovery protocol was determined by inspecting the official Control
Center's scanner library. No ASUSTOR code or assets are included in this
project.

## Support

This project is provided **as-is**, with no support and no active
maintenance. Issues are disabled on purpose; there is no place to ask
questions or request features. Fork freely — the MIT license lets you do
anything you want with it.

## License

[MIT](LICENSE). Uses [grandcat/zeroconf](https://github.com/grandcat/zeroconf) (MIT).
