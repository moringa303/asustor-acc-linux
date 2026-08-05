#!/usr/bin/env python3
"""Native GTK4 front-end for the `acc` ASUSTOR NAS scanner.

Discovery, WOL and URL handling are done by the bundled `acc` Go binary;
this window just presents the results Control Center-style.
"""
import json
import os
import subprocess
import sys
import threading

import gi

gi.require_version("Gtk", "4.0")
from gi.repository import Gtk, GLib, Gio  # noqa: E402

def _find_acc():
    override = os.environ.get("ACC_BIN")
    if override:
        return override
    sibling = os.path.join(os.path.dirname(os.path.abspath(__file__)), "acc")
    if os.access(sibling, os.X_OK):
        return sibling
    return "acc"  # rely on PATH (packaged installs put it in /usr/bin)


ACC_BIN = _find_acc()

COLUMNS = [
    ("Name", "name"),
    ("IP", "ip"),
    ("Model", "model"),
    ("Serial Number", "serial_number"),
    ("MAC Address", "mac"),
    ("ADM Version", "adm_version"),
    ("Port", "port"),
    ("State", "state"),
]


class AccWindow(Gtk.ApplicationWindow):
    def __init__(self, app):
        super().__init__(application=app, title="Asustor ACC for Linux")
        self.set_default_size(980, 480)
        self.devices = []

        header = Gtk.HeaderBar()
        self.set_titlebar(header)

        self.scan_btn = Gtk.Button(label="Scan")
        self.scan_btn.add_css_class("suggested-action")
        self.scan_btn.connect("clicked", lambda *_: self.scan())
        header.pack_start(self.scan_btn)

        self.open_btn = Gtk.Button(label="Open ADM")
        self.open_btn.connect("clicked", lambda *_: self.open_selected())
        header.pack_end(self.open_btn)

        self.wol_btn = Gtk.Button(label="Wake (WOL)")
        self.wol_btn.connect("clicked", lambda *_: self.wol_selected())
        header.pack_end(self.wol_btn)

        self.store = Gtk.ListStore(*([str] * len(COLUMNS)))
        self.view = Gtk.TreeView(model=self.store)
        for i, (title, _) in enumerate(COLUMNS):
            renderer = Gtk.CellRendererText()
            col = Gtk.TreeViewColumn(title, renderer, text=i)
            col.set_resizable(True)
            self.view.append_column(col)
        self.view.connect("row-activated", lambda *_: self.open_selected())

        scroll = Gtk.ScrolledWindow(vexpand=True)
        scroll.set_child(self.view)

        self.status = Gtk.Label(label="Press Scan to search for ASUSTOR NAS devices.",
                                xalign=0, margin_start=12, margin_top=6, margin_bottom=6)

        box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL)
        box.append(scroll)
        box.append(self.status)
        self.set_child(box)

        self.scan()

    # -- actions ---------------------------------------------------------
    def scan(self):
        self.scan_btn.set_sensitive(False)
        self.status.set_text("Scanning…")

        def worker():
            try:
                out = subprocess.run(
                    [ACC_BIN, "scan", "--json", "--timeout", "4"],
                    capture_output=True, text=True, check=True,
                ).stdout
                devices = json.loads(out) or []
                err = None
            except Exception as exc:  # noqa: BLE001
                devices, err = [], str(exc)
            GLib.idle_add(self.scan_done, devices, err)

        threading.Thread(target=worker, daemon=True).start()

    def scan_done(self, devices, err):
        self.devices = devices
        self.store.clear()
        for dev in devices:
            self.store.append([str(dev.get(key, "") or "") for _, key in COLUMNS])
        if err:
            self.status.set_text(f"Scan failed: {err}")
        elif devices:
            self.status.set_text(f"Found {len(devices)} device(s).")
        else:
            self.status.set_text("No ASUSTOR NAS devices found on the local network.")
        self.scan_btn.set_sensitive(True)
        return False

    def selected(self):
        model, it = self.view.get_selection().get_selected()
        if it is None:
            self.status.set_text("Select a NAS first.")
            return None
        return self.devices[model.get_path(it).get_indices()[0]]

    def open_selected(self):
        dev = self.selected()
        if dev:
            subprocess.run([ACC_BIN, "url", dev["name"]], capture_output=True, text=True)
            https_off = str(dev.get("http_enabled", "")).lower() == "no"
            port = dev.get("https_port") if https_off else dev.get("port")
            scheme = "https" if https_off else "http"
            Gio.AppInfo.launch_default_for_uri(f"{scheme}://{dev['ip']}:{port}/")

    def wol_selected(self):
        dev = self.selected()
        if not dev:
            return
        mac = dev.get("mac")
        if not mac:
            self.status.set_text(f"{dev['name']} did not report a MAC address.")
            return
        res = subprocess.run([ACC_BIN, "wol", mac], capture_output=True, text=True)
        self.status.set_text(res.stdout.strip() or res.stderr.strip())


class AccApp(Gtk.Application):
    def __init__(self):
        super().__init__(application_id="com.asustor.acc.native")

    def do_activate(self):
        win = self.get_active_window() or AccWindow(self)
        win.present()


if __name__ == "__main__":
    sys.exit(AccApp().run(sys.argv))
