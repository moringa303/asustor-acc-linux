#!/usr/bin/env python3
"""GTK4 GUI of Asustor ACC for Linux.

Discovery, WOL and URL handling are done by the bundled `acc` binary;
this window presents the results like the Control Center device list.
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

# Must match the installed .desktop file name and the hicolor icon name,
# or GNOME shows a generic gear icon for the window.
APP_ID = "io.github.moringa303.AsustorAcc"

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
STATE_COL = len(COLUMNS) - 1

# Wire values an older acc binary may still pass through, mapped to the
# display names.
STATE_NAMES = {"inited": "Ready", "uninited": "Uninitialized"}
STATE_COLORS = {"Ready": "#2ec27e", "Uninitialized": "#3584e4", "Not ready": "#e5a50a"}


class AccWindow(Gtk.ApplicationWindow):
    def __init__(self, app):
        super().__init__(application=app, title="Asustor ACC for Linux")
        self.set_icon_name(APP_ID)
        self.set_default_size(980, 480)
        self.devices = []

        header = Gtk.HeaderBar()
        self.set_titlebar(header)

        self.scan_btn = Gtk.Button(label="Scan")
        self.scan_btn.add_css_class("suggested-action")
        self.scan_btn.set_tooltip_text("Search the local network for ASUSTOR NAS devices")
        self.scan_btn.connect("clicked", lambda *_: self.scan())
        header.pack_start(self.scan_btn)

        self.spinner = Gtk.Spinner()
        header.pack_start(self.spinner)

        self.open_btn = Gtk.Button(label="Open ADM")
        self.open_btn.set_tooltip_text("Open the web interface of the selected NAS")
        self.open_btn.connect("clicked", lambda *_: self.open_selected())
        header.pack_end(self.open_btn)

        self.wol_btn = Gtk.Button(label="Wake (WOL)")
        self.wol_btn.set_tooltip_text("Send a Wake-on-LAN packet to the selected NAS")
        self.wol_btn.connect("clicked", lambda *_: self.wol_selected())
        header.pack_end(self.wol_btn)

        self.store = Gtk.ListStore(*([str] * len(COLUMNS)))
        self.view = Gtk.TreeView(model=self.store)
        for i, (title, _) in enumerate(COLUMNS):
            renderer = Gtk.CellRendererText()
            renderer.set_padding(10, 5)
            col = Gtk.TreeViewColumn(title, renderer, text=i)
            col.set_resizable(True)
            if i == 0:
                col.set_expand(True)
            if i == STATE_COL:
                col.set_cell_data_func(renderer, self._state_cell)
            self.view.append_column(col)

        # Uninitialized NAS rows get a link icon that opens the ADM setup
        # wizard (plain HTTP, port 8000).
        setup_renderer = Gtk.CellRendererPixbuf()
        self.setup_col = Gtk.TreeViewColumn("Setup", setup_renderer)
        self.setup_col.set_cell_data_func(setup_renderer, self._setup_cell)
        self.view.append_column(self.setup_col)

        self.view.connect("row-activated", lambda *_: self.open_selected())
        click = Gtk.GestureClick()
        click.connect("released", self._view_clicked)
        self.view.add_controller(click)

        scroll = Gtk.ScrolledWindow(vexpand=True)
        scroll.set_child(self.view)

        self.status = Gtk.Label(label="Press Scan to search for ASUSTOR NAS devices.",
                                xalign=0, margin_start=12, margin_top=6, margin_bottom=6)

        box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL)
        box.append(scroll)
        box.append(self.status)
        self.set_child(box)

        self.scan()

    @staticmethod
    def _state_cell(_col, renderer, model, it, _data):
        color = STATE_COLORS.get(model.get_value(it, STATE_COL))
        if color:
            renderer.set_property("foreground", color)
        renderer.set_property("foreground-set", color is not None)

    @staticmethod
    def _setup_cell(_col, renderer, model, it, _data):
        uninitialized = model.get_value(it, STATE_COL) == "Uninitialized"
        renderer.set_property("icon-name", "web-browser-symbolic" if uninitialized else None)

    def _view_clicked(self, _gesture, _n_press, x, y):
        bx, by = self.view.convert_widget_to_bin_window_coords(int(x), int(y))
        hit = self.view.get_path_at_pos(bx, by)
        if not hit or hit[1] is not self.setup_col:
            return
        dev = self.devices[hit[0].get_indices()[0]]
        if not dev.get("initialized"):
            Gio.AppInfo.launch_default_for_uri(self._device_url(dev))

    @staticmethod
    def _device_url(dev):
        if not dev.get("initialized"):
            return f"http://{dev['ip']}:{dev.get('port') or 8000}/"
        https_only = str(dev.get("http_enabled", "")).lower() == "no"
        port = dev.get("https_port") if https_only else dev.get("port")
        scheme = "https" if https_only else "http"
        return f"{scheme}://{dev['ip']}:{port}/"

    # -- actions ---------------------------------------------------------
    def scan(self):
        self.scan_btn.set_sensitive(False)
        self.spinner.start()
        self.status.set_text("Scanning the local network…")

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
            row = [str(dev.get(key, "") or "") for _, key in COLUMNS]
            row[STATE_COL] = STATE_NAMES.get(row[STATE_COL], row[STATE_COL])
            self.store.append(row)
        if err:
            self.status.set_text(f"Scan failed: {err}")
        elif devices:
            text = f"Found {len(devices)} device(s). Double-click a row to open ADM."
            if any(not d.get("initialized") for d in devices):
                text += " Click the browser icon on an uninitialized NAS to set it up."
            self.status.set_text(text)
        else:
            self.status.set_text("No ASUSTOR NAS found on the local network. "
                                 "Run 'acc doctor' in a terminal if you expected one.")
        self.spinner.stop()
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
            Gio.AppInfo.launch_default_for_uri(self._device_url(dev))

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
        super().__init__(application_id=APP_ID)

    def do_activate(self):
        win = self.get_active_window() or AccWindow(self)
        win.present()


if __name__ == "__main__":
    sys.exit(AccApp().run(sys.argv))
