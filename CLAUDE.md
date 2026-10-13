# Asustor ACC for Linux

Unofficial Linux version of the ASUSTOR Control Center: ASUSTOR NAS
discovery over mDNS, ADM launcher, Wake-on-LAN. A Go CLI (`cmd/acc`) plus
a GTK4/PyGObject GUI (`gui/acc-gui.py`) that drives the CLI through
`acc scan --json --all`.

## Layout

- `cmd/acc/`: the CLI. `main.go` (commands, flags, table), `nas.go`
  (device record, TXT parsing, URL), `scan.go` (built-in mDNS and
  avahi-browse run together and merged), `avahi.go`, `cache.go` (device
  memory in `$XDG_CACHE_HOME/asustor-acc/devices.json`, online time),
  `wol.go`, `doctor.go`.
- `cmd/fakenas/`: advertises a fake NAS so everything is testable without
  hardware (`-name`, `-model`, `-mac`, `-port`, `-init`).
- `gui/acc-gui.py`: GTK4 GUI, one file, no build step. libadwaita is
  optional at runtime.
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
- The version lives in git tags only; `main.version` is set by ldflags.
- Commits are authored by the repository owner (`git config user.name
  "Frank Eckardt"`, `user.email frank@takindo.com`), never by a Claude
  identity, and commit messages are plain: no `Co-Authored-By`,
  `Claude-Session` or other AI attribution trailers. The owner is the
  sole contributor on GitHub.
