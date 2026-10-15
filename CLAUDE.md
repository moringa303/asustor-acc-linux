# Asustor Control Center for Linux

Unofficial Linux version of the ASUSTOR Control Center: ASUSTOR NAS
discovery over mDNS, ADM launcher, Wake-on-LAN. A Go CLI (`cmd/acc`) plus
a GTK4/PyGObject GUI (`gui/acc-gui.py`) that drives the CLI through
`acc scan --json --all`.

## Layout

- `cmd/acc/`: the CLI. `main.go` (commands, flags, table), `nas.go`
  (device record, TXT parsing, URL), `scan.go` (built-in mDNS and
  avahi-browse run together and merged), `avahi.go`, `cache.go` (device
  memory in `$XDG_CACHE_HOME/asustor-acc/devices.json`, online time),
  `snmp.go` (GET and GetNext client, uptime), `health.go` (`acc health`:
  the ASUSTOR MIB), `ports.go` (`acc ports`: TCP connect to the
  well-known service ports of one NAS), `subnet.go` (`--subnet`: unicast
  mDNS sweep of a subnet), `wol.go`, `doctor.go`.
- `cmd/fakenas/`: advertises a fake NAS so everything is testable without
  hardware (`-name`, `-model`, `-mac`, `-port`, `-init`).
- `gui/acc-gui.py`: GTK4 GUI, one file, no build step. libadwaita,
  WebKitGTK 6.0 and libsecret are optional at runtime. Settings (auto
  refresh, offline devices, accent, notifications, subnets, SSH user,
  ADM account, tags and notes per NAS) live in
  `~/.config/asustor-acc/gui.json`; the ADM API paths for restart and
  shutdown can be overridden in `adm.json` next to it.
- `assets/icon.svg`: icon source; the PNG sizes are rendered from it with
  `rsvg-convert`. `assets/screenshot.png` is the README screenshot.
- `packaging/`: desktop file, AUR PKGBUILD, tarball install script.
  `.github/workflows/release.yml` builds deb/rpm/tarball on `v*` tags.

## Build and check

- `go build ./... && go vet ./...`; keep `gofmt -l ./cmd` empty and
  `staticcheck ./...` clean.
- Release binary: `CGO_ENABLED=0 go build -trimpath
  -ldflags="-s -w -X main.version=<ver>" -o acc ./cmd/acc`.
- Manual test: `go run ./cmd/fakenas` in one terminal and
  `go run ./cmd/acc scan -v` in another; `ACC_BIN=./acc python3
  gui/acc-gui.py` runs the GUI against a locally built binary.
- The JSON of `acc scan --json` is the contract with the GUI: fields may
  be added, not renamed or removed.

## Conventions

- No cgo and no new Go dependencies without a strong reason: the CLI must
  stay a single static binary.
- The version lives in git tags only; `main.version` is set by ldflags
  and `GUI_VERSION` in `gui/acc-gui.py` is stamped by `sed` at packaging
  time (release.yml, PKGBUILD). A release is made by running the
  release workflow manually with the version as input (it creates the
  tag) or by pushing a `v*` tag.
- Commits are authored by the repository owner (`git config user.name
  "Frank Eckardt"`, `user.email frank@takindo.com`), never by a Claude
  identity, and are NOT signed from a Claude session (commit and tag
  with `-c commit.gpgsign=false -c tag.gpgsign=false`: the session's
  default signing key is unknown to GitHub and only produces
  "Unverified" badges). Commit messages, pull request titles and
  descriptions are plain: no `Co-Authored-By`, `Claude-Session`,
  "Generated with Claude Code" or any other AI attribution. The owner
  is the sole contributor on GitHub.
- Before pushing: `gofmt -l ./cmd` empty, `go vet ./...`, `go test
  ./...` and `staticcheck ./...` clean, `python3 -m pyflakes
  gui/acc-gui.py` clean.
