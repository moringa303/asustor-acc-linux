# Asustor ACC for Linux

A Linux version of the ASUSTOR Control Center (ACC): find ASUSTOR NAS
devices on your LAN, open the ADM web UI, and send Wake-on-LAN. Comes as
a dependency-free CLI (`acc`) and a GTK4 GUI (`acc-gui`).

> Unofficial community project. Not affiliated with, endorsed by, or
> supported by ASUSTOR Inc. "ASUSTOR" and "ADM" are trademarks of ASUSTOR
> Inc., used here only to describe compatibility.

![icon](assets/icon-128.png)

ASUSTOR ships Control Center for Windows and macOS only. This project
speaks the same discovery protocol, natively on Linux.

## Features

- `acc`: single static binary, works on any x86-64 Linux distribution:

  ```
  acc scan                  # discover ASUSTOR NAS devices (table)
  acc scan --json           # machine-readable output
  acc open  <name|ip>       # open the ADM web UI in your browser
  acc url   <name|ip>       # print the ADM URL
  acc wol   <name|ip|mac>   # send a Wake-on-LAN magic packet
  acc doctor                # diagnose "no device found" problems
  ```

- `acc-gui`: GTK4 front-end with the familiar device table
  (Name / IP / Model / Serial / MAC / ADM version / State) and Scan,
  Open ADM, Wake buttons.

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
# GUI (optional): needs gtk4 + python-gobject
sudo install -Dm755 gui/acc-gui.py /usr/local/bin/acc-gui
```

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
`acc open` and the GUI's link icon go to its setup wizard at
`http://<ip>:8000`. Discovery works with any mDNS stack;
`avahi-browse -r _ASUSTOR_ADM._tcp` shows the same data. `acc` uses its
own mDNS resolver and falls back to `avahi-browse` when a local
avahi-daemon owns the mDNS socket.

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

- `cmd/acc/`: Go CLI (discovery, doctor, WOL)
- `cmd/fakenas/`: advertises a fake NAS over mDNS so everything is
  testable without hardware. `go run ./cmd/fakenas` in one terminal,
  `acc scan` in another. `-init` advertises a factory-fresh NAS.
- `gui/acc-gui.py`: GTK4 GUI (calls `acc scan --json`)
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
