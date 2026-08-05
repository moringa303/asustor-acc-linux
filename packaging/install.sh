#!/usr/bin/env bash
# Installs Asustor ACC for Linux to /usr/local and pulls GUI dependencies
# with your distro's package manager. Run from an extracted release tarball
# (expects usr/bin/acc, usr/bin/acc-gui next to this script) or after
# `go build -o usr/bin/acc ./cmd/acc` in a source checkout.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

if command -v pacman >/dev/null; then
    sudo pacman -S --needed --noconfirm gtk4 python-gobject xdg-utils
elif command -v apt-get >/dev/null; then
    sudo apt-get install -y python3-gi gir1.2-gtk-4.0 xdg-utils
elif command -v dnf >/dev/null; then
    sudo dnf install -y python3-gobject gtk4 xdg-utils
else
    echo "Unknown distro — install GTK4 + PyGObject + xdg-utils manually." >&2
fi

sudo install -Dm755 usr/bin/acc /usr/local/bin/acc
sudo install -Dm755 usr/bin/acc-gui /usr/local/bin/acc-gui
sudo install -Dm644 usr/share/pixmaps/asustor-acc.png /usr/local/share/pixmaps/asustor-acc.png
sudo install -Dm644 usr/share/applications/asustor-acc.desktop /usr/local/share/applications/asustor-acc.desktop
echo "Installed: acc (CLI) and acc-gui (GUI)."
echo "If scan finds nothing, run: acc doctor"
