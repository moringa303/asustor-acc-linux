#!/usr/bin/env python3
"""GTK4 GUI of Asustor Control Center for Linux.

Discovery, Wake-on-LAN and the device memory live in the bundled `acc`
binary; this window presents its results like the Control Center device
list and keeps them fresh.
"""
import json
import os
import re
import shutil
import subprocess
import sys
import threading
import time
from datetime import datetime

import gi

gi.require_version("Gtk", "4.0")
gi.require_version("Gdk", "4.0")
gi.require_version("Gsk", "4.0")
gi.require_version("Graphene", "1.0")
from gi.repository import Gdk, Gio, GLib, GObject, Graphene, Gsk, Gtk, Pango  # noqa: E402

# libadwaita is optional. With it the window follows the system's
# light/dark preference and gets the current GNOME header bar look.
try:
    gi.require_version("Adw", "1")
    from gi.repository import Adw  # noqa: E402
except (ValueError, ImportError):
    Adw = None

# WebKitGTK is optional too: with it the compatibility list opens in a
# window of the app, without it in the browser.
try:
    gi.require_version("WebKit", "6.0")
    from gi.repository import WebKit  # noqa: E402
except (ValueError, ImportError):
    WebKit = None
WEBKIT_HINT = ("install the WebKitGTK bindings to open it inside the app: gir1.2-webkit-6.0 "
               "(Debian, Ubuntu), webkitgtk6.0 (Fedora), webkitgtk-6.0 (Arch)")


def _find_acc():
    override = os.environ.get("ACC_BIN")
    if override:
        return override
    sibling = os.path.join(os.path.dirname(os.path.abspath(__file__)), "acc")
    if os.access(sibling, os.X_OK):
        return sibling
    return "acc"  # rely on PATH (packaged installs put it in /usr/bin)


ACC_BIN = _find_acc()
ACC_PATH = shutil.which(ACC_BIN) or ACC_BIN

# Stamped by the packaging (release workflow, PKGBUILD); "dev" in a checkout.
GUI_VERSION = "dev"

APP_NAME = "Asustor Control Center"
APP_FULL_NAME = f"{APP_NAME} for Linux"
# Must match the installed .desktop file name and the hicolor icon name,
# or GNOME shows a generic gear icon for the window.
APP_ID = "io.github.moringa303.AsustorAcc"
# The accent colour: kiwi green. KIWI is the fill (dark text on it),
# KIWI_DEEP the same green for small marks on a light background.
KIWI = "#9EE12F"
KIWI_DEEP = "#86CC1E"
WEBSITE = "https://github.com/moringa303/asustor-acc-linux"

SCAN_TIMEOUT = 6  # seconds acc listens for answers
REFRESH_SECONDS = int(os.environ.get("ACC_GUI_REFRESH", "30"))
# A NAS that answered recently stays online in the list until it has been
# missing from scans for this long. On a busy segment single mDNS answers
# get lost; without the grace a NAS would blink offline and back.
PRESENCE_GRACE = 150

# Layout. The device list is the familiar table whenever the window is
# at least TABLE_WIDTH wide; the cells ellipsize (the full text is in the
# tooltip) so the table always fits and never needs a horizontal
# scrollbar. Narrower windows get a list of cards that wrap their text.
# Below COMPACT_WIDTH the header bar keeps only the Scan icon, Open ADM
# and the menu (Wake stays in the menus) and the summary moves from the
# title to the status bar, so the window can shrink to MIN_WIDTH. The
# compact threshold is a floor: the real width the full header bar needs
# is measured too, and it gives way LAYOUT_SLACK before that, because a
# window manager keeps the window at the current layout's minimum width
# and would otherwise never let it reach the breakpoint.
TABLE_WIDTH = 1000
COMPACT_WIDTH = 700
LAYOUT_SLACK = 24
MIN_WIDTH = 360

# Table columns: title, Device property, whether the column takes the
# spare width.
COLUMNS = [
    ("Model", "model", False),
    ("Name", "name", True),
    ("IP Address", "ip", False),
    ("Serial Number", "serial", False),
    ("MAC Address", "mac", False),
    ("ADM", "adm", False),
    ("Port", "port", False),
    ("State", "state", False),
]
TABULAR = {"ip", "serial", "mac", "adm", "port"}
# Fixed widths (px) for the columns that may shorten with an ellipsis
# when the window is narrow; Model and State are always shown whole, Name
# takes the rest. Tooltips carry the full text.
FIXED_WIDTHS = {"ip": 150, "serial": 150, "mac": 150, "adm": 100, "port": 56}

STATE_CLASSES = {
    "Ready": "ready",
    "Uninitialized": "uninitialized",
    "Not ready": "notready",
    "Offline": "offline",
}

SHARES_HINT = ("no application handles smb:// links; install gvfs-smb (GNOME, Cinnamon, "
               "XFCE) or kio-extras (KDE), or open the address in your file manager")

# File managers that browse smb:// on their own, by desktop (an entry of
# XDG_CURRENT_DESKTOP). Most of them register no handler for the smb
# scheme, so the desktop's default-app lookup finds nothing even though
# the file manager would open the address fine when started with it.
FILE_MANAGERS = [
    ("GNOME", "nautilus"), ("ubuntu", "nautilus"), ("Budgie", "nautilus"),
    ("Unity", "nautilus"), ("KDE", "dolphin"), ("XFCE", "thunar"),
    ("X-Cinnamon", "nemo"), ("MATE", "caja"), ("LXQt", "pcmanfm-qt"),
    ("LXDE", "pcmanfm"), ("Pantheon", "io.elementary.files"),
    ("Deepin", "dde-file-manager"),
]
LAUNCH_WAIT = 3  # seconds a launcher gets to fail before it counts as started


def file_managers():
    """Installed file managers that can show smb://, the running desktop's
    own first."""
    desktop = os.environ.get("XDG_CURRENT_DESKTOP", "").lower().split(":")
    ranked = sorted(FILE_MANAGERS, key=lambda d: d[0].lower() not in desktop)
    found = []
    for _desk, fm in ranked:
        if fm not in found and shutil.which(fm):
            found.append(fm)
    return found

# Link to the model's compatibility list on asustor.com. Without an exact
# address scheme the default goes through DuckDuckGo's "I'm feeling ducky"
# redirect to the first asustor.com hit; ~/.config/asustor-acc/links.json
# can replace the template ({model} is substituted, {media} is HDD or
# SSD) and override single models:
#   {"compat": "https://...{model}...{media}...",
#    "models": {"AS6704T": {"compat": "https://..."}}}
LINK_TEMPLATES = {
    "compat": "https://duckduckgo.com/?q=%5C+site%3Aasustor.com+{model}+{media}+compatibility+list",
}
LINKS_FILE = os.path.join(GLib.get_user_config_dir(), "asustor-acc", "links.json")


def _load_links():
    try:
        with open(LINKS_FILE, encoding="utf-8") as f:
            data = json.load(f)
        if isinstance(data, dict):
            return data
    except (OSError, ValueError):
        pass
    return {}


def model_links(model):
    """(compatibility-list URL, media) for a model. Flash (FS...) models
    are all-SSD, so they get the SSD list; everything else is an HDD NAS
    first, even with M.2 slots."""
    base = model.split()[0] if model else ""
    media = "SSD" if model_shape(model)[1] == "flash" else "HDD"
    links = _load_links()
    templates = {**LINK_TEMPLATES, **{k: v for k, v in links.items() if k in LINK_TEMPLATES}}
    override = links.get("models", {}).get(base, {}) if isinstance(links.get("models"), dict) else {}
    compat = override.get("compat") or templates["compat"].format(model=base, media=media)
    return compat, media

CSS = f"""
.state-dot {{ min-width: 10px; min-height: 10px; border-radius: 5px; background: #9a9996; }}
.state-dot.ready {{ background: {KIWI_DEEP}; }}
.state-dot.uninitialized {{ background: #3584e4; }}
.state-dot.notready {{ background: #e5a50a; }}
.state-dot.offline {{ background: #9a9996; }}
columnview > listview > row > cell {{ padding: 7px 8px; }}
/* The last column stays clear of the overlay scrollbar. */
columnview > listview > row > cell:last-child {{ padding-right: 24px; }}
listview.cards > row {{ padding: 8px 12px; }}
.card-name {{ font-weight: bold; }}
.tabular {{ font-variant-numeric: tabular-nums; }}
.empty-title {{ font-size: 1.4em; font-weight: bold; }}
.statusbar {{ padding: 6px 12px; }}
/* Kiwi green accents: the stripe under the header bar, the "for Linux"
   pill of the title, the main action button, the empty page's icon. */
.kiwi-stripe {{ min-height: 3px; background: {KIWI}; }}
.pill {{ background: {KIWI}; color: #15200a; border-radius: 999px; padding: 1px 8px;
         font-size: 0.72em; font-weight: bold; }}
button.kiwi {{ background-color: {KIWI}; background-image: none; color: #15200a; }}
button.kiwi:hover {{ background-color: #b3ea55; }}
button.kiwi:active, button.kiwi:checked {{ background-color: {KIWI_DEEP}; }}
button.kiwi:disabled {{ background-color: alpha({KIWI}, 0.35); color: alpha(#15200a, 0.5); }}
.kiwi-icon {{ color: {KIWI_DEEP}; }}
progressbar.kiwi trough {{ min-height: 3px; border: none; border-radius: 0; background: alpha({KIWI}, 0.25); }}
progressbar.kiwi progress {{ min-height: 3px; border: none; border-radius: 0; background: {KIWI}; }}
"""


def kiwi_stripe():
    stripe = Gtk.Box()
    stripe.add_css_class("kiwi-stripe")
    return stripe


def _parse_time(value):
    """RFC 3339 timestamp from acc -> epoch seconds, or None."""
    if not value:
        return None
    try:
        return datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()
    except ValueError:
        return None


def format_uptime(seconds):
    """274512 -> '3d 4h 15m'; 14700 -> '4h 05m'; 725 -> '12m 05s'."""
    s = max(0, int(seconds))
    days, s = divmod(s, 86400)
    hours, s = divmod(s, 3600)
    minutes, s = divmod(s, 60)
    if days:
        return f"{days}d {hours}h {minutes:02d}m"
    if hours:
        return f"{hours}h {minutes:02d}m"
    return f"{minutes}m {s:02d}s"


def format_seen(epoch):
    t = datetime.fromtimestamp(epoch)
    if t.date() == datetime.now().date():
        return t.strftime("%H:%M")
    return t.strftime("%-d %b")


def model_shape(model):
    """Bay count and form factor read off an ASUSTOR model number:
    AS5304T -> (4, 'tower'), AS7112RD -> (12, 'rack'), FS6712X -> (12, 'flash')."""
    name = model.upper()
    digits = re.search(r"\d+", name)
    bays = 0
    if digits:
        run = digits.group(0)
        bays = int(run[-2:]) if len(run) >= 3 else int(run)
    if "RD" in name:
        kind = "rack"
    elif name.startswith("FS"):
        kind = "flash"
    else:
        kind = "tower"
    return bays, kind


class ModelGlyph(Gtk.Widget):
    """Small line drawing of the NAS: a tower or rack unit with its drive
    bays, drawn from the model number in the widget's text colour. Drawn
    with GTK's own snapshot API, so it needs no pycairo."""

    SIZE = 28
    LINE = 1.5
    LED = 3  # the power LED, kiwi green while the NAS is online

    def __init__(self):
        super().__init__(valign=Gtk.Align.CENTER)
        self.set_size_request(self.SIZE, self.SIZE)
        self.bays, self.kind = 0, "tower"
        self.lit = False

    def set_model(self, model):
        shape = model_shape(model) if model else (0, "tower")
        if shape != (self.bays, self.kind):
            self.bays, self.kind = shape
            self.queue_draw()

    def set_lit(self, lit):
        if lit != self.lit:
            self.lit = lit
            self.queue_draw()

    def do_snapshot(self, snapshot):
        fg = self.get_color()
        color = Gdk.RGBA()
        color.red, color.green, color.blue, color.alpha = fg.red, fg.green, fg.blue, fg.alpha * 0.85
        size, line, led = self.SIZE, self.LINE, self.LED
        if self.kind == "rack":
            x, y, w, h = 1.0, size / 2 - 6, size - 2, 12
            slots = min(max(self.bays, 2), 8)
        else:
            x, y, w, h = size / 2 - 8, 2.0, 16, size - 4
            slots = min(max(self.bays, 1), 6)
        outline = Gsk.RoundedRect()
        outline.init_from_rect(Graphene.Rect().init(x, y, w, h), 3.0)
        snapshot.append_border(outline, [line] * 4, [color] * 4)

        def slot(sx, sy, sw, sh):
            snapshot.append_color(color, Graphene.Rect().init(sx, sy, sw, sh))

        # The right-hand edge is kept for the LED.
        if self.kind == "rack":
            # vertical slots across the unit
            gap = (w - 6 - led - 2) / slots
            for i in range(slots):
                slot(x + 3 + gap * i + gap / 2 - line / 2, y + 3, line, h - 6)
            led_x, led_y = x + w - 3 - led, y + h / 2 - led / 2
        elif self.kind == "flash":
            # 2.5" drives: short vertical slots in two columns
            rows = max(1, (slots + 1) // 2)
            gap = (h - 6) / rows
            for i in range(slots):
                slot(x + 4 + (i % 2) * 4.5 - line / 2, y + 3 + gap * (i // 2) + gap / 2 - 1.5, line, 3)
            led_x, led_y = x + w - 3 - led, y + h - 3 - led
        else:
            # 3.5" drives: horizontal slots down the front
            gap = (h - 6) / slots
            for i in range(slots):
                slot(x + 4, y + 3 + gap * i + gap / 2 - line / 2, w - 8 - led - 1, line)
            led_x, led_y = x + w - 3 - led, y + h - 3 - led
        if self.lit:
            kiwi = Gdk.RGBA()
            kiwi.parse(KIWI_DEEP)
            snapshot.append_color(kiwi, Graphene.Rect().init(led_x, led_y, led, led))


class Device(GObject.Object):
    """One NAS as reported by `acc scan --json --all`.

    Everything the views show is a property, so cells stay bound to the
    object and update in place when a later scan changes it.
    """

    name = GObject.Property(type=str, default="")
    ip = GObject.Property(type=str, default="")
    model = GObject.Property(type=str, default="")
    serial = GObject.Property(type=str, default="")
    mac = GObject.Property(type=str, default="")
    adm = GObject.Property(type=str, default="")
    port = GObject.Property(type=str, default="")
    state = GObject.Property(type=str, default="")
    online = GObject.Property(type=bool, default=False)
    # Tooltip of the State cell: the uptime the NAS reports over SNMP
    # while online, when it was last seen while offline.
    state_tip = GObject.Property(type=str, default="")
    # Card lines: "Ready · 192.168.1.10 · AS5304T · ADM 4.3.3 · port 8000"
    # and "00:11:32:AA:BB:CC · AS2025TEST01".
    details = GObject.Property(type=str, default="")
    ids = GObject.Property(type=str, default="")

    def __init__(self, data):
        super().__init__()
        self.data = {}
        self.answered = False  # did the NAS answer the last scan
        self.known_state = ""  # its state when it last answered
        self.uptime = None  # seconds the NAS reported, as of last_seen
        self.last_seen = None
        self.seen_tip = ""
        self.text = ""  # every field in one lower-case string, for the search
        self.update(data)

    @staticmethod
    def key_of(data):
        return data.get("mac") or (data.get("name") or "").lower()

    @property
    def key(self):
        return self.key_of(self.data)

    @property
    def url(self):
        return self.data.get("url") or ""

    @property
    def shares_url(self):
        url = self.data.get("shares_url") or ""
        ip = self.data.get("ip") or ""
        if not url and re.fullmatch(r"\d+\.\d+\.\d+\.\d+", ip):
            url = f"smb://{ip}/"  # an older acc without the field
        return url

    @property
    def initialized(self):
        return bool(self.data.get("initialized"))

    def update(self, data):
        self.data = data
        self.name = data.get("name") or ""
        self.ip = data.get("ip") or data.get("ipv6") or ""
        self.model = data.get("model") or ""
        self.serial = data.get("serial_number") or ""
        self.mac = data.get("mac") or ""
        self.adm = data.get("adm_version") or ""
        self.port = str(data.get("port") or "")
        self.answered = bool(data.get("online"))
        # acc remembers the state a NAS had when it last answered.
        self.known_state = data.get("state") or self.known_state or "Ready"
        self.last_seen = _parse_time(data.get("last_seen"))
        uptime = data.get("uptime_seconds")
        self.uptime = int(uptime) if self.answered and isinstance(uptime, (int, float)) else None
        self.ids = " · ".join(p for p in (self.mac, self.serial) if p)
        self._set_online(self.answered or self.in_grace())

    def in_grace(self):
        """Missing from the last scan, but seen recently enough to still
        count as online: one lost mDNS answer must not flip a NAS offline."""
        return (not self.answered and self.last_seen is not None
                and 0 <= time.time() - self.last_seen < PRESENCE_GRACE)

    def _set_online(self, online):
        """Derive everything that depends on whether the NAS counts as
        online: the state, the card lines, the tooltips, the search text."""
        self.online = online
        self.state = self.known_state if online else "Offline"
        state = self.state
        self.seen_tip = ""
        if self.last_seen and not self.answered:
            seen = datetime.fromtimestamp(self.last_seen).strftime("%-d %b %Y %H:%M:%S")
            if online:
                self.seen_tip = f"Did not answer the last scan; last answered on {seen}"
            else:
                state = f"Offline, last seen {format_seen(self.last_seen)}"
                self.seen_tip = f"Last answered a scan on {seen}"
        parts = [state, self.ip,
                 f"ADM {self.adm}" if self.adm else "",
                 f"port {self.port}" if self.port else ""]
        self.details = " · ".join(p for p in parts if p)
        self.text = " ".join((self.name, self.ip, self.model, self.serial, self.mac,
                              self.adm, self.port, self.state)).lower()
        self._tip()

    def matches(self, query):
        """True when every word of query occurs somewhere in the device."""
        return all(word in self.text for word in query.lower().split())

    def uptime_now(self):
        """Seconds the NAS has been up right now, or None if it did not say."""
        if not self.online or self.uptime is None:
            return None
        elapsed = time.time() - self.last_seen if self.last_seen else 0
        return self.uptime + max(0, elapsed)

    def tick(self):
        """Once a second: let the grace period run out and keep the State
        tooltip current. True when the NAS has just gone offline."""
        if self.online and not self.answered and not self.in_grace():
            self._set_online(False)
            return True
        self._tip()
        return False

    def _tip(self):
        up = self.uptime_now()
        if up is not None:
            tip = f"Up {format_uptime(up)} (reported by the NAS over SNMP)"
        else:
            tip = self.seen_tip
        if tip != self.state_tip:
            self.state_tip = tip


class ContentLayout(Gtk.BoxLayout):
    """Box layout that tells its box the width it was really allocated.

    A Gtk.Box hands allocation to its layout manager, so the box's own
    size_allocate is never called; the allocate of the layout is where the
    real width shows up. It lays the box out as usual first.
    """

    def do_allocate(self, widget, width, height, baseline):
        Gtk.BoxLayout.do_allocate(self, widget, width, height, baseline)
        widget.allocated(width)


class ContentBox(Gtk.Box):
    """Vertical box that reports layout changes from its allocated width.

    classify(width) names the layout for the width the box really got
    (not the window's default size). When that name changes, apply(name)
    runs from an idle handler: swapping stack pages during allocation is
    not allowed.
    """

    def __init__(self, classify, apply):
        super().__init__(orientation=Gtk.Orientation.VERTICAL)
        self.set_layout_manager(ContentLayout(orientation=Gtk.Orientation.VERTICAL))
        self._classify, self._apply = classify, apply
        self._layout = None
        self._pending = False

    def allocated(self, width):
        layout = self._classify(width)
        if layout != self._layout:
            self._layout = layout
            if not self._pending:
                self._pending = True
                GLib.idle_add(self._flush)

    def _flush(self):
        self._pending = False
        self._apply(self._layout)
        return GLib.SOURCE_REMOVE


class AccWindow(Gtk.ApplicationWindow):
    def __init__(self, app):
        super().__init__(application=app, title=APP_FULL_NAME)
        self.set_icon_name(APP_ID)
        self.set_default_size(1100, 560)
        self.set_size_request(MIN_WIDTH, 300)
        self.scanning = False
        self.auto_refresh = True
        self.show_offline = True
        self.last_scan = None
        self.last_error = None
        self.acc_version = ""
        self.summary = ""
        self.view_name = "table"
        self.compact = False
        self.query = ""  # the search text; "" shows everything
        self._header_width = 0  # widest the full header bar measured so far
        self._table_width = 0  # widest the table measured so far
        self._state_px = None  # width of the widest State text
        self._status_timer = None
        self._rows = {}  # view -> {list item: (device, row widget)}

        self._build_actions()
        self._build_header()
        self._build_views()
        self._build_body()

        GLib.timeout_add_seconds(1, self._tick)
        GLib.timeout_add_seconds(REFRESH_SECONDS, self._auto_scan)
        self._run_acc(["version"], self._got_version)
        self.scan()

    # -- construction ----------------------------------------------------
    def _build_actions(self):
        plain = [
            ("scan", self.scan),
            ("search", self.start_search),
            ("open", self.open_selected),
            ("shares", self.open_shares),
            ("wake", self.wake_selected),
            ("ping", self.ping_selected),
            ("compat-hdd", self.open_compat),
            ("compat-ssd", self.open_compat),
            ("copy-ip", lambda: self._copy("ip")),
            ("copy-mac", lambda: self._copy("mac")),
            ("forget", self.forget_selected),
            ("forget-offline", self.forget_offline),
            ("doctor", self.run_doctor),
            ("about", self.show_about),
        ]
        for name, callback in plain:
            action = Gio.SimpleAction.new(name, None)
            action.connect("activate", lambda _a, _p, cb=callback: cb())
            self.add_action(action)
        for name, attr in (("auto-refresh", "auto_refresh"), ("show-offline", "show_offline")):
            action = Gio.SimpleAction.new_stateful(
                name, None, GLib.Variant.new_boolean(getattr(self, attr)))
            action.connect("change-state", self._toggle, attr)
            self.add_action(action)

    def _toggle(self, action, value, attr):
        action.set_state(value)
        setattr(self, attr, value.get_boolean())
        if attr == "show_offline":
            self.filter.changed(Gtk.FilterChange.DIFFERENT)
            self._refresh_summary()

    def _build_header(self):
        header = self.header = Gtk.HeaderBar()
        self.set_titlebar(header)

        scan_btn = Gtk.Button(action_name="win.scan")
        content = Gtk.Box(spacing=6)
        content.append(Gtk.Image.new_from_icon_name("view-refresh-symbolic"))
        self.scan_label = Gtk.Label(label="Scan")
        content.append(self.scan_label)
        scan_btn.set_child(content)
        scan_btn.set_tooltip_text("Search the local network for ASUSTOR NAS devices (F5)")
        header.pack_start(scan_btn)
        self.spinner = Gtk.Spinner()
        header.pack_start(self.spinner)
        self.search_btn = Gtk.ToggleButton(icon_name="edit-find-symbolic")
        self.search_btn.set_tooltip_text("Search the list (Ctrl+F, or just start typing)")
        header.pack_start(self.search_btn)

        self.titles = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, valign=Gtk.Align.CENTER)
        title_row = Gtk.Box(spacing=6, halign=Gtk.Align.CENTER)
        title = Gtk.Label(label=APP_NAME)
        title.add_css_class("title")
        pill = Gtk.Label(label="for Linux", valign=Gtk.Align.CENTER)
        pill.add_css_class("pill")
        title_row.append(title)
        title_row.append(pill)
        self.subtitle = Gtk.Label()
        self.subtitle.add_css_class("subtitle")
        self.titles.append(title_row)
        self.titles.append(self.subtitle)
        header.set_title_widget(self.titles)
        self._show_summary("Scanning the local network…")

        menu = Gio.Menu()
        section = Gio.Menu()
        section.append("Open ADM", "win.open")
        section.append("Open shared folders", "win.shares")
        section.append("Wake (WOL)", "win.wake")
        section.append("Ping", "win.ping")
        menu.append_section(None, section)
        menu.append_section(None, self._link_menu())
        section = Gio.Menu()
        section.append(f"Refresh every {REFRESH_SECONDS} s", "win.auto-refresh")
        section.append("Show offline devices", "win.show-offline")
        menu.append_section(None, section)
        section = Gio.Menu()
        section.append("Diagnose discovery…", "win.doctor")
        section.append("Forget offline devices", "win.forget-offline")
        menu.append_section(None, section)
        section = Gio.Menu()
        section.append(f"About {APP_NAME}", "win.about")
        menu.append_section(None, section)
        menu_btn = Gtk.MenuButton(icon_name="open-menu-symbolic", menu_model=menu, primary=True)
        menu_btn.set_tooltip_text("Menu (F10)")
        header.pack_end(menu_btn)

        self.open_btn = Gtk.Button(label="Open ADM", action_name="win.open")
        self.open_btn.add_css_class("kiwi")
        self.open_btn.set_tooltip_text("Open the web interface of the selected NAS (Ctrl+O)")
        header.pack_end(self.open_btn)
        self.wake_btn = Gtk.Button(label="Wake", action_name="win.wake")
        self.wake_btn.set_tooltip_text("Send a Wake-on-LAN magic packet to the selected NAS")
        header.pack_end(self.wake_btn)
        self.ping_btn = Gtk.Button(label="Ping", action_name="win.ping")
        self.ping_btn.set_tooltip_text("Ping the selected NAS three times (Ctrl+P)")
        header.pack_end(self.ping_btn)

        # The HDD or SSD compatibility list on asustor.com, whichever
        # applies to the selected model.
        theme = Gtk.IconTheme.get_for_display(self.get_display())

        def icon(*names):
            # First name the icon theme has; symbolic sets differ per theme.
            return next((n for n in names if theme.has_icon(n)), names[-1])

        self.hdd_btn = Gtk.Button(action_name="win.compat-hdd", icon_name=icon(
            "drive-harddisk-symbolic", "drive-harddisk"))
        self.ssd_btn = Gtk.Button(action_name="win.compat-ssd", icon_name=icon(
            "drive-harddisk-solidstate-symbolic", "media-flash-symbolic",
            "drive-removable-media-symbolic", "drive-harddisk-symbolic"))
        header.pack_end(self.hdd_btn)
        header.pack_end(self.ssd_btn)

    @staticmethod
    def _link_menu():
        """Menu section with the asustor.com compatibility list; the entry
        that does not apply to the selected model hides itself."""
        section = Gio.Menu()
        for label, action in (("HDD compatibility list", "win.compat-hdd"),
                              ("SSD compatibility list", "win.compat-ssd")):
            item = Gio.MenuItem.new(label, action)
            item.set_attribute_value("hidden-when", GLib.Variant("s", "action-disabled"))
            section.append_item(item)
        return section

    def _build_views(self):
        """The table and the card list: two views of one selection."""
        self.store = Gio.ListStore.new(Device)
        self.filter = Gtk.CustomFilter.new(self._shown)
        filtered = Gtk.FilterListModel.new(self.store, self.filter)
        self.sorted = Gtk.SortListModel.new(filtered, None)
        self.selection = Gtk.SingleSelection.new(self.sorted)
        self.selection.set_autoselect(False)
        self.selection.set_can_unselect(True)
        self.selection.connect("notify::selected-item", lambda *_: self._selection_changed())

        self.view = Gtk.ColumnView.new(self.selection)
        self.sorted.set_sorter(self.view.get_sorter())
        self.view.connect("activate", lambda _v, pos: self._open(self.sorted.get_item(pos)))
        name_col = None
        for title, prop, expand in COLUMNS:
            factory = Gtk.SignalListItemFactory()
            factory.connect("setup", self._setup_cell, prop)
            factory.connect("bind", self._bind_cell, prop)
            factory.connect("unbind", self._unbind)
            col = Gtk.ColumnViewColumn.new(title, factory)
            col.set_expand(expand)
            if prop in FIXED_WIDTHS:
                col.set_fixed_width(FIXED_WIDTHS[prop])
            col.set_sorter(Gtk.CustomSorter.new(lambda a, b, *_, p=prop: self._compare(a, b, p)))
            self.view.append_column(col)
            if prop == "name":
                name_col = col
        self.view.sort_by_column(name_col, Gtk.SortType.ASCENDING)

        factory = Gtk.SignalListItemFactory()
        factory.connect("setup", self._setup_card)
        factory.connect("bind", self._bind_card)
        factory.connect("unbind", self._unbind)
        self.cards = Gtk.ListView.new(self.selection, factory)
        self.cards.set_show_separators(True)
        self.cards.add_css_class("cards")
        self.cards.connect("activate", lambda _v, pos: self._open(self.sorted.get_item(pos)))

        # Right click: select the row under the pointer and show its menu.
        model = Gio.Menu()
        section = Gio.Menu()
        section.append("Open ADM", "win.open")
        section.append("Open shared folders", "win.shares")
        section.append("Wake (WOL)", "win.wake")
        section.append("Ping", "win.ping")
        model.append_section(None, section)
        model.append_section(None, self._link_menu())
        section = Gio.Menu()
        section.append("Copy IP address", "win.copy-ip")
        section.append("Copy MAC address", "win.copy-mac")
        model.append_section(None, section)
        section = Gio.Menu()
        section.append("Forget this device", "win.forget")
        model.append_section(None, section)
        self.row_menus = {}
        for view in (self.view, self.cards):
            self._rows[view] = {}
            popover = Gtk.PopoverMenu.new_from_model(model)
            popover.set_parent(view)
            popover.set_has_arrow(False)
            self.row_menus[view] = popover
            click = Gtk.GestureClick(button=3)
            click.connect("pressed", self._row_right_click)
            view.add_controller(click)

    def _build_body(self):
        # Horizontal scrollbars are never shown: the table is only used at
        # widths where every column fits, and the cards wrap their text.
        never = Gtk.PolicyType.NEVER
        self.stack = Gtk.Stack(vexpand=True, hhomogeneous=False)
        self.table_page = Gtk.ScrolledWindow(child=self.view, hscrollbar_policy=never)
        self.stack.add_named(self.table_page, "table")
        self.stack.add_named(Gtk.ScrolledWindow(child=self.cards, hscrollbar_policy=never), "cards")
        self.stack.add_named(self._build_empty_page(), "empty")

        self.status = Gtk.Label(xalign=0, wrap=True, wrap_mode=Pango.WrapMode.WORD_CHAR,
                                lines=3, ellipsize=Pango.EllipsizeMode.END)
        self.status.add_css_class("dim-label")
        self.status.add_css_class("statusbar")

        # The search bar sits above the list; typing anywhere in the window
        # opens it, Escape closes it and shows everything again.
        self.search_entry = Gtk.SearchEntry(hexpand=True, max_width_chars=60,
                                            placeholder_text="Name, IP, model, serial, MAC…")
        self.search_entry.connect("search-changed", self._search_changed)
        self.search_bar = Gtk.SearchBar(child=self.search_entry, show_close_button=True)
        self.search_bar.connect_entry(self.search_entry)
        self.search_bar.set_key_capture_widget(self)
        self.search_bar.connect("notify::search-mode-enabled", self._search_mode)
        self.search_bar.bind_property("search-mode-enabled", self.search_btn, "active",
                                      GObject.BindingFlags.BIDIRECTIONAL
                                      | GObject.BindingFlags.SYNC_CREATE)

        box = ContentBox(self._classify_layout, self._apply_layout)
        box.append(kiwi_stripe())
        box.append(self.search_bar)
        box.append(self.stack)
        box.append(Gtk.Separator())
        box.append(self.status)
        self.set_child(box)
        self._set_hint()

    def _build_empty_page(self):
        page = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=12,
                       valign=Gtk.Align.CENTER, halign=Gtk.Align.CENTER,
                       margin_start=24, margin_end=24)
        icon = Gtk.Image.new_from_icon_name("network-server-symbolic")
        icon.set_pixel_size(96)
        icon.add_css_class("kiwi-icon")
        self.empty_title = Gtk.Label(label="No ASUSTOR NAS found", wrap=True,
                                     justify=Gtk.Justification.CENTER)
        self.empty_title.add_css_class("empty-title")
        self.empty_text = Gtk.Label(wrap=True, max_width_chars=52, justify=Gtk.Justification.CENTER)
        self.empty_text.add_css_class("dim-label")
        buttons = Gtk.Box(spacing=12, halign=Gtk.Align.CENTER, margin_top=12)
        again = Gtk.Button(label="Scan again", action_name="win.scan")
        again.add_css_class("kiwi")
        buttons.append(again)
        buttons.append(Gtk.Button(label="Diagnose discovery…", action_name="win.doctor"))
        for w in (icon, self.empty_title, self.empty_text, buttons):
            page.append(w)
        return page

    # -- responsive layout -----------------------------------------------
    def _classify_layout(self, width):
        """Layout for the content width: (view name, compact header)."""
        # The full header bar can only be measured while it is shown, so
        # its width is remembered from then.
        if not self.compact:
            minimum = self.header.measure(Gtk.Orientation.HORIZONTAL, -1)[0]
            self._header_width = max(self._header_width, minimum + LAYOUT_SLACK)
        # The table's own minimum (long model names, a large font, a
        # scrollbar that is not an overlay) can be more than TABLE_WIDTH;
        # measured, so its last column is never cut off but the cards take
        # over instead.
        minimum = self.table_page.measure(Gtk.Orientation.HORIZONTAL, -1)[0]
        self._table_width = max(self._table_width, minimum + LAYOUT_SLACK)
        return ("table" if width >= max(TABLE_WIDTH, self._table_width) else "cards",
                width < max(COMPACT_WIDTH, self._header_width))

    def _apply_layout(self, layout):
        self.view_name, self.compact = layout
        if self.stack.get_visible_child_name() != "empty":
            self.stack.set_visible_child_name(self.view_name)
        for widget in (self.wake_btn, self.ping_btn, self.scan_label, self.titles):
            widget.set_visible(not self.compact)
        if self._status_timer is None and not self.last_error:
            self._set_hint()

    def _show_summary(self, text):
        """Device summary: the header subtitle, or the status bar when compact."""
        self.summary = text
        self.subtitle.set_text(text)
        if self.compact and self._status_timer is None and not self.last_error:
            self._set_hint()

    # -- table cells -----------------------------------------------------
    def _setup_cell(self, _factory, item, prop):
        # Cells ellipsize so the table fits any width from TABLE_WIDTH up;
        # the tooltip carries the full text. Model and State are always
        # shown whole (the table gives way to the cards instead).
        whole = prop in ("model", "state")
        label = Gtk.Label(xalign=0, ellipsize=Pango.EllipsizeMode.NONE if whole
                          else Pango.EllipsizeMode.END)
        if prop in TABULAR:
            label.add_css_class("tabular")
        if not whole:
            item.set_child(label)
            return
        box = Gtk.Box(spacing=8)
        if prop == "model":
            box.glyph = ModelGlyph()
            box.append(box.glyph)
        else:
            box.dot = Gtk.Box(valign=Gtk.Align.CENTER)
            box.dot.add_css_class("state-dot")
            box.append(box.dot)
            # As wide as the widest state, so the column does not change
            # width with the states that happen to be on screen.
            label.set_size_request(self._state_width(), -1)
        box.append(label)
        box.label = label
        item.set_child(box)

    def _state_width(self):
        """Pixel width of the widest state text in the table's font."""
        if self._state_px is None:
            self._state_px = max(self.view.create_pango_layout(s).get_pixel_size()[0]
                                 for s in STATE_CLASSES)
        return self._state_px

    def _bind_cell(self, _factory, item, prop):
        dev, widget = item.get_item(), item.get_child()
        label = getattr(widget, "label", widget)
        flags = GObject.BindingFlags.SYNC_CREATE
        item.bindings = [dev.bind_property(prop, label, "label", flags)]
        if prop == "state":
            item.bindings.append(dev.bind_property("state_tip", widget, "tooltip-text", flags))
        else:
            item.bindings.append(dev.bind_property(prop, label, "tooltip-text", flags))
        item.handlers = [dev.connect("notify::online", lambda *_: self._style(widget, dev))]
        if prop == "model":
            widget.glyph.set_model(dev.model)
            item.handlers.append(dev.connect(
                "notify::model", lambda *_: widget.glyph.set_model(dev.model)))
        if prop == "state":
            item.handlers.append(dev.connect("notify::state", lambda *_: self._style(widget, dev)))
        self._style(widget, dev)
        if prop == "name":
            self._rows[self.view][item] = (dev, widget)

    # -- cards -----------------------------------------------------------
    def _setup_card(self, _factory, item):
        # Model (with its glyph) first, then the name.
        row = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=2)
        top = Gtk.Box(spacing=8)
        row.glyph = ModelGlyph()
        row.dot = Gtk.Box(valign=Gtk.Align.CENTER)
        row.dot.add_css_class("state-dot")
        row.model = Gtk.Label(xalign=0)
        row.model.add_css_class("card-name")
        row.name = Gtk.Label(xalign=0, hexpand=True, wrap=True, wrap_mode=Pango.WrapMode.WORD_CHAR)
        for w in (row.glyph, row.dot, row.model, row.name):
            top.append(w)
        row.details = Gtk.Label(xalign=0, wrap=True, wrap_mode=Pango.WrapMode.WORD_CHAR)
        row.ids = Gtk.Label(xalign=0, wrap=True, wrap_mode=Pango.WrapMode.WORD_CHAR)
        row.ids.add_css_class("tabular")
        for w in (top, row.details, row.ids):
            row.append(w)
        item.set_child(row)

    def _bind_card(self, _factory, item):
        dev, row = item.get_item(), item.get_child()
        flags = GObject.BindingFlags.SYNC_CREATE
        row.glyph.set_model(dev.model)
        item.bindings = [
            dev.bind_property("model", row.model, "label", flags),
            dev.bind_property("name", row.name, "label", flags),
            dev.bind_property("state_tip", row, "tooltip-text", flags),
            dev.bind_property("details", row.details, "label", flags),
            dev.bind_property("ids", row.ids, "label", flags),
        ]
        item.handlers = [dev.connect("notify::online", lambda *_: self._style(row, dev)),
                         dev.connect("notify::state", lambda *_: self._style(row, dev)),
                         dev.connect("notify::model", lambda *_: row.glyph.set_model(dev.model))]
        self._style(row, dev)
        self._rows[self.cards][item] = (dev, row)

    def _unbind(self, _factory, item):
        dev = item.get_item()
        for binding in item.bindings:
            binding.unbind()
        for handler in item.handlers:
            dev.disconnect(handler)
        item.bindings, item.handlers = [], []
        for rows in self._rows.values():
            rows.pop(item, None)

    @staticmethod
    def _style(widget, dev):
        """Dim offline devices and colour the state dot (cells and cards)."""
        target = getattr(widget, "label", widget)
        if dev.online:
            target.remove_css_class("dim-label")
        else:
            target.add_css_class("dim-label")
        glyph = getattr(widget, "glyph", None)
        if glyph is not None:
            glyph.set_opacity(1.0 if dev.online else 0.45)
            glyph.set_lit(dev.online)
        dot = getattr(widget, "dot", None)
        if dot is not None:
            for cls in STATE_CLASSES.values():
                dot.remove_css_class(cls)
            dot.add_css_class(STATE_CLASSES.get(dev.state, "offline"))

    @staticmethod
    def _compare(a, b, prop):
        # Offline devices always sink to the bottom.
        if a.online != b.online:
            return -1 if a.online else 1
        if prop == "port":
            ka, kb = int(a.port or 0), int(b.port or 0)
        else:
            ka, kb = getattr(a, prop).lower(), getattr(b, prop).lower()
        return (ka > kb) - (ka < kb)

    # -- scanning --------------------------------------------------------
    def scan(self):
        if self.scanning:
            return
        self.scanning = True
        self.lookup_action("scan").set_enabled(False)
        self.spinner.start()
        self._show_summary("Scanning the local network…")

        def worker():
            try:
                out = subprocess.run(
                    [ACC_BIN, "scan", "--json", "--all", "--timeout", str(SCAN_TIMEOUT)],
                    capture_output=True, text=True, check=True, timeout=SCAN_TIMEOUT * 5,
                ).stdout
                devices, err = json.loads(out) or [], None
            except FileNotFoundError:
                devices, err = [], (f"{ACC_BIN} was not found. Install the acc binary next to "
                                    "acc-gui or on your PATH, or point ACC_BIN at it.")
            except subprocess.CalledProcessError as exc:
                devices, err = [], exc.stderr.strip() or f"acc exited with status {exc.returncode}"
            except Exception as exc:  # noqa: BLE001
                devices, err = [], str(exc)
            GLib.idle_add(self._scan_done, devices, err)

        threading.Thread(target=worker, daemon=True).start()

    def _scan_done(self, devices, err):
        self.scanning = False
        self.spinner.stop()
        self.lookup_action("scan").set_enabled(True)
        self.last_error = err
        if err:
            self.set_status(f"Scan failed: {err}", sticky=True)
        else:
            self._apply(devices)
            self.last_scan = time.time()
        self._refresh_summary()
        return GLib.SOURCE_REMOVE

    def _apply(self, devices):
        """Update the views in place so selection and scroll position stay."""
        fresh = {Device.key_of(d): d for d in devices}
        i = 0
        while i < self.store.get_n_items():
            dev = self.store.get_item(i)
            data = fresh.pop(dev.key, None)
            if data is None:
                self.store.remove(i)
            else:
                dev.update(data)
                i += 1
        # One splice instead of one append per device: every insert into
        # the sorted model shifts rows and makes the views rebind, which
        # with a few hundred devices takes seconds of main-loop time.
        if fresh:
            self.store.splice(self.store.get_n_items(), 0, [Device(d) for d in fresh.values()])
        self.filter.changed(Gtk.FilterChange.DIFFERENT)
        sorter = self.view.get_sorter()
        if sorter is not None:
            sorter.changed(Gtk.SorterChange.DIFFERENT)

    def _auto_scan(self):
        if self.auto_refresh and not self.scanning:
            self.scan()
        return GLib.SOURCE_CONTINUE

    def _tick(self):
        changed = False
        for i in range(self.store.get_n_items()):
            changed = self.store.get_item(i).tick() or changed
        if changed:
            # A grace period ran out: re-filter, re-sort and recount.
            self.filter.changed(Gtk.FilterChange.DIFFERENT)
            sorter = self.view.get_sorter()
            if sorter is not None:
                sorter.changed(Gtk.SorterChange.DIFFERENT)
            self._refresh_summary()
        return GLib.SOURCE_CONTINUE

    def _devices(self):
        return [self.store.get_item(i) for i in range(self.store.get_n_items())]

    def _refresh_summary(self):
        devices = self._devices()
        online = sum(1 for d in devices if d.online)
        offline = len(devices) - online
        shown = self.sorted.get_n_items()
        parts = []
        if self.query:
            parts.append(f"{shown} of {len(devices)} match")
        if online and not self.query:
            parts.append(f"{online} online")
        if offline and not self.query:
            parts.append(f"{offline} offline")
        summary = " · ".join(parts) if parts else "No devices"
        if self.last_scan:
            summary += " · scanned " + time.strftime("%H:%M:%S", time.localtime(self.last_scan))
        self._show_summary(summary)

        if shown:
            self.stack.set_visible_child_name(self.view_name)
        else:
            if self.last_error:
                self.empty_title.set_text("Scan failed")
                self.empty_text.set_text(self.last_error)
            elif self.query:
                self.empty_title.set_text("No matching device")
                self.empty_text.set_text(f"None of the {len(devices)} devices matches "
                                         f"“{self.query}”. Press Escape to show all.")
            elif offline and not self.show_offline:
                self.empty_title.set_text("No ASUSTOR NAS is answering")
                self.empty_text.set_text(f"{offline} remembered device(s) are hidden. "
                                         "Enable “Show offline devices” in the menu to wake one.")
            else:
                self.empty_title.set_text("No ASUSTOR NAS found")
                self.empty_text.set_text(
                    "Make sure the NAS is powered on and on the same network as this "
                    "computer, then scan again. If it still does not appear, the "
                    "diagnosis tells whether a firewall is blocking mDNS.")
            self.stack.set_visible_child_name("empty")
        self._selection_changed()
        if self._status_timer is None and not self.last_error:
            self._set_hint()

    def _got_version(self, out, err):
        """Record the acc version; warn when an old copy of acc is being used
        (a leftover in /usr/local/bin next to a packaged install, say)."""
        self.acc_version = out.strip().removeprefix("acc ").strip()
        if not self.acc_version:
            self.set_status(f"{ACC_PATH} is too old for this GUI (no 'version' command): "
                            f"{err.splitlines()[0] if err else 'no output'}. Remove it or "
                            "reinstall; 'which -a acc acc-gui' lists every copy.", sticky=True)
        elif "dev" not in (GUI_VERSION, self.acc_version) and self.acc_version != GUI_VERSION:
            self.set_status(f"acc {self.acc_version} at {ACC_PATH} does not match acc-gui "
                            f"{GUI_VERSION}: an older copy is being used. Remove it or "
                            "reinstall; 'which -a acc acc-gui' lists every copy.", sticky=True)

    # -- search ----------------------------------------------------------
    def _shown(self, dev, *_):
        """The list filter: offline devices on request, the search's matches."""
        return (self.show_offline or dev.online) and (not self.query or dev.matches(self.query))

    def start_search(self):
        self.search_bar.set_search_mode(True)
        self.search_entry.grab_focus()

    def _search_mode(self, bar, _pspec):
        if bar.get_search_mode():
            self.search_entry.grab_focus()
        else:
            self.search_entry.set_text("")
            (self.view if self.view_name == "table" else self.cards).grab_focus()

    def _search_changed(self, entry):
        query = entry.get_text().strip()
        if query == self.query:
            return
        self.query = query
        self.filter.changed(Gtk.FilterChange.DIFFERENT)
        self._refresh_summary()

    # -- selection and actions -------------------------------------------
    def selected(self):
        return self.selection.get_selected_item()

    def _selection_changed(self):
        dev = self.selected()
        self.lookup_action("open").set_enabled(dev is not None and dev.online and bool(dev.url))
        self.lookup_action("shares").set_enabled(
            dev is not None and dev.online and bool(dev.shares_url))
        self.lookup_action("wake").set_enabled(dev is not None and bool(dev.mac))
        self.lookup_action("ping").set_enabled(dev is not None and bool(dev.ip))
        has_model = dev is not None and bool(dev.model)
        media = model_links(dev.model)[1] if has_model else ""
        self.lookup_action("compat-hdd").set_enabled(has_model and media == "HDD")
        self.lookup_action("compat-ssd").set_enabled(has_model and media == "SSD")
        self.hdd_btn.set_visible(media != "SSD")
        self.ssd_btn.set_visible(media == "SSD")
        model = dev.model if has_model else "the selected model"
        self.hdd_btn.set_tooltip_text(f"{model}: HDD compatibility list on asustor.com")
        self.ssd_btn.set_tooltip_text(f"{model}: SSD compatibility list on asustor.com")
        self.lookup_action("copy-ip").set_enabled(dev is not None and bool(dev.ip))
        self.lookup_action("copy-mac").set_enabled(dev is not None and bool(dev.mac))
        self.lookup_action("forget").set_enabled(dev is not None and not dev.online)
        self.lookup_action("forget-offline").set_enabled(any(not d.online for d in self._devices()))
        setup = dev is not None and not dev.initialized
        self.open_btn.set_label("Set up NAS" if setup else "Open ADM")
        self.open_btn.set_tooltip_text(
            "Open the setup wizard of the selected NAS (Ctrl+O)" if setup else
            "Open the web interface of the selected NAS (Ctrl+O)")

    def _device_at(self, view, y):
        """The device of the row at y in view, from its bound row widgets."""
        for dev, widget in self._rows[view].values():
            # Hit-test the whole list row, not just the bound label, so
            # the padding above and below the text counts as well.
            row = widget
            for _ in range(6):
                parent = row.get_parent()
                if parent is None or parent is view or row.get_css_name() == "row":
                    break
                row = parent
            ok, bounds = row.compute_bounds(view)
            if ok and bounds.get_y() <= y < bounds.get_y() + bounds.get_height():
                return dev
        return None

    def _row_right_click(self, gesture, _n_press, x, y):
        view = gesture.get_widget()
        dev = self._device_at(view, y)
        if dev is not None:
            for i in range(self.sorted.get_n_items()):
                if self.sorted.get_item(i).key == dev.key:
                    self.selection.set_selected(i)
                    break
        if self.selected() is None:
            return
        rect = Gdk.Rectangle()
        rect.x, rect.y, rect.width, rect.height = int(x), int(y), 1, 1
        menu = self.row_menus[view]
        menu.set_pointing_to(rect)
        menu.popup()

    def _launch(self, uri):
        """Open uri with the desktop's registered handler. When there is
        none (usual for smb://: file managers register no handler for the
        scheme), the running desktop's file manager is started with the
        address, then any other installed one, then xdg-open. A launcher
        that exits with an error within LAUNCH_WAIT counts as failed and
        the next one is tried; if all fail the address is put on the
        clipboard so it can be pasted into a location bar."""
        try:
            Gio.AppInfo.launch_default_for_uri(uri, None)
            return
        except GLib.Error as exc:
            reason = exc.message
        commands = []
        if uri.startswith("smb://"):
            commands += [[fm, uri] for fm in file_managers()]
        commands.append(["xdg-open", uri])

        def worker(reason):
            for cmd in commands:
                try:
                    proc = subprocess.Popen(cmd, stdout=subprocess.DEVNULL,
                                            stderr=subprocess.DEVNULL)
                except OSError as exc:
                    reason = f"{cmd[0]}: {exc.strerror or exc}"
                    continue
                try:
                    code = proc.wait(timeout=LAUNCH_WAIT)
                except subprocess.TimeoutExpired:
                    code = 0  # still running: it has a window up
                if code == 0:
                    GLib.idle_add(lambda: self.set_status(f"Opened {uri} with {cmd[0]}") and False)
                    return
                reason = f"{cmd[0]} exited with status {code}"
            GLib.idle_add(lambda: self._launch_failed(uri, reason) and False)

        self.set_status(f"Opening {uri}…")
        threading.Thread(target=worker, args=(reason,), daemon=True).start()

    def _launch_failed(self, uri, reason):
        provider = Gdk.ContentProvider.new_for_bytes(
            "text/plain;charset=utf-8", GLib.Bytes.new(uri.encode()))
        self.get_clipboard().set_content(provider)
        hint = SHARES_HINT if uri.startswith("smb://") else "no application handles the address"
        self.set_status(f"Could not open {uri} ({reason}): {hint}. The address is on the "
                        "clipboard: paste it into the file manager's location bar (Ctrl+L).",
                        sticky=True)

    def _open(self, dev):
        if dev is None:
            return
        if not dev.online:
            self.set_status(f"{dev.name} is not answering; wake it first.")
            return
        if not dev.url:
            self.set_status(f"{dev.name} did not report an address.")
            return
        self._launch(dev.url)

    def open_selected(self):
        self._open(self.selected())

    def open_shares(self):
        dev = self.selected()
        if dev is None:
            return
        if not dev.online:
            self.set_status(f"{dev.name} is not answering; wake it first.")
            return
        if not dev.shares_url:
            self.set_status(f"{dev.name} did not report an IPv4 address.")
            return
        self._launch(dev.shares_url)

    def wake_selected(self):
        dev = self.selected()
        if dev is None:
            return
        if not dev.mac:
            self.set_status(f"{dev.name} did not report a MAC address.")
            return
        self.set_status(f"Waking {dev.name}…")
        self._run_acc(["wol", dev.mac], lambda out, err: self.set_status(out or err))

    def open_compat(self):
        """Open the model's HDD or SSD compatibility list on asustor.com."""
        dev = self.selected()
        if dev is None or not dev.model:
            return
        compat, media = model_links(dev.model)
        title = f"{dev.model} {media} compatibility list"
        if WebKit is None:
            self.set_status(f"Opening the {title} in the browser ({WEBKIT_HINT})…")
            self._launch(compat)
            return
        self.set_status(f"Opening the {title}…")
        WebWindow(self, compat, title).present()

    def ping_selected(self):
        dev = self.selected()
        if dev is None or not dev.ip:
            return
        name, ip = dev.name, dev.ip
        self.set_status(f"Pinging {name} ({ip})…", sticky=True)

        def worker():
            try:
                res = subprocess.run(["ping", "-c", "3", "-W", "1", ip],
                                     capture_output=True, text=True, timeout=15)
                out = res.stdout
            except FileNotFoundError:
                GLib.idle_add(lambda: self.set_status("ping is not installed (package iputils).") and False)
                return
            except subprocess.TimeoutExpired:
                out = ""
            got = re.search(r"(\d+) packets transmitted, (\d+) (?:packets )?received", out)
            rtt = re.search(r"= [\d.]+/([\d.]+)/", out)
            if got and int(got.group(2)) > 0:
                text = f"{name} ({ip}): {got.group(2)}/{got.group(1)} replies"
                if rtt:
                    text += f", {float(rtt.group(1)):.1f} ms average"
            else:
                text = f"{name} ({ip}): no reply to ping"
            GLib.idle_add(lambda: self.set_status(text) and False)

        threading.Thread(target=worker, daemon=True).start()

    def forget_selected(self):
        dev = self.selected()
        if dev is not None:
            self._forget([dev])

    def forget_offline(self):
        self._forget([d for d in self._devices() if not d.online])

    def _forget(self, devices):
        if not devices:
            return

        def worker():
            failed = []
            for dev in devices:
                res = subprocess.run([ACC_BIN, "forget", dev.mac or dev.name],
                                     capture_output=True, text=True)
                if res.returncode != 0:
                    failed.append(res.stderr.strip() or dev.name)
            GLib.idle_add(done, failed)

        def done(failed):
            keys = {d.key for d in devices}
            i = 0
            while i < self.store.get_n_items():
                if self.store.get_item(i).key in keys:
                    self.store.remove(i)
                else:
                    i += 1
            if failed:
                self.set_status("Could not forget: " + "; ".join(failed))
            else:
                self.set_status(f"Forgot {len(devices)} device(s).")
            self._refresh_summary()
            return GLib.SOURCE_REMOVE

        threading.Thread(target=worker, daemon=True).start()

    def _copy(self, prop):
        dev = self.selected()
        if dev is None:
            return
        value = getattr(dev, prop)
        provider = Gdk.ContentProvider.new_for_bytes(
            "text/plain;charset=utf-8", GLib.Bytes.new(value.encode()))
        self.get_clipboard().set_content(provider)
        self.set_status(f"Copied {value}")

    def run_doctor(self):
        win = Gtk.Window(transient_for=self, title="Discovery diagnosis",
                         default_width=760, default_height=520)
        win.set_titlebar(Gtk.HeaderBar())
        text = Gtk.TextView(editable=False, monospace=True, cursor_visible=False,
                            wrap_mode=Gtk.WrapMode.WORD_CHAR,
                            left_margin=12, right_margin=12, top_margin=12, bottom_margin=12)
        text.get_buffer().set_text("Running acc doctor, this takes a few seconds…\n")
        win.set_child(Gtk.ScrolledWindow(child=text, hscrollbar_policy=Gtk.PolicyType.NEVER))
        escape = Gtk.ShortcutController()
        escape.add_shortcut(Gtk.Shortcut.new(
            Gtk.ShortcutTrigger.parse_string("Escape"),
            Gtk.CallbackAction.new(lambda *_: win.close() or True)))
        win.add_controller(escape)
        win.present()
        self._run_acc(["doctor"], lambda out, err: text.get_buffer().set_text(out + err))

    def show_about(self):
        about = Gtk.AboutDialog(
            transient_for=self, modal=True,
            program_name=APP_FULL_NAME,
            version=GUI_VERSION if GUI_VERSION != "dev" else (self.acc_version or "dev"),
            comments="Unofficial Linux version of the ASUSTOR Control Center: "
                     "NAS discovery, ADM launcher and Wake-on-LAN.\n\n"
                     f"acc-gui {GUI_VERSION} · acc {self.acc_version or '?'} at {ACC_PATH}",
            website=WEBSITE,
            license_type=Gtk.License.MIT_X11,
        )
        # Installed: the themed icon. Run from a source checkout: the PNG.
        if Gtk.IconTheme.get_for_display(self.get_display()).has_icon(APP_ID):
            about.set_logo_icon_name(APP_ID)
        else:
            png = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                               "..", "assets", "icon-256.png")
            if os.path.exists(png):
                about.set_logo(Gdk.Texture.new_from_filename(png))
        about.present()

    # -- helpers ---------------------------------------------------------
    def _run_acc(self, args, done):
        """Run acc in the background; done(stdout, stderr) runs on the UI thread."""
        def worker():
            try:
                res = subprocess.run([ACC_BIN, *args], capture_output=True, text=True, timeout=60)
                out, err = res.stdout.strip(), res.stderr.strip()
            except Exception as exc:  # noqa: BLE001
                out, err = "", str(exc)
            GLib.idle_add(lambda: done(out, err) and False)

        threading.Thread(target=worker, daemon=True).start()

    def _set_hint(self):
        if self.compact:
            self.status.set_text(self.summary)
        elif self.sorted.get_n_items():
            self.status.set_text("Double-click a NAS to open ADM. Right-click for more. "
                                 "Type to search.")
        else:
            self.status.set_text("Press Scan to search for ASUSTOR NAS devices.")
        self._status_timer = None
        return GLib.SOURCE_REMOVE

    def set_status(self, text, sticky=False):
        self.status.set_text(text)
        self.status.set_tooltip_text(text)  # the label shows three lines at most
        if self._status_timer:
            GLib.source_remove(self._status_timer)
            self._status_timer = None
        if not sticky:
            self._status_timer = GLib.timeout_add_seconds(8, self._set_hint)


class WebWindow(Gtk.Window):
    """A page of asustor.com inside the app: back, forward and reload
    buttons, the address under the title, a button that hands the page to
    the browser, and a kiwi progress stripe. Works down to MIN_WIDTH: the
    buttons are icons and the title shortens."""

    def __init__(self, parent, url, title):
        super().__init__(title=title, application=parent.get_application(),
                         default_width=1000, default_height=720)
        self.set_size_request(MIN_WIDTH, 300)
        self.set_icon_name(APP_ID)
        self.start_title = title
        self.web = WebKit.WebView(hexpand=True, vexpand=True)
        self.web.connect("create", self._create)
        self.web.connect("notify::uri", lambda *_: self._update())
        self.web.connect("notify::title", lambda *_: self._update())
        self.web.connect("notify::estimated-load-progress", lambda *_: self._progress())
        self.web.connect("load-changed", self._load_changed)
        self.web.connect("load-failed", self._load_failed)

        header = Gtk.HeaderBar()
        self.set_titlebar(header)
        for icon, tip, action in (("go-previous-symbolic", "Back (Alt+Left)", self.web.go_back),
                                  ("go-next-symbolic", "Forward (Alt+Right)", self.web.go_forward),
                                  ("view-refresh-symbolic", "Reload (F5)", self.web.reload)):
            btn = Gtk.Button(icon_name=icon, tooltip_text=tip)
            btn.connect("clicked", lambda _b, action=action: action())
            header.pack_start(btn)
        browser = Gtk.Button(icon_name="web-browser-symbolic",
                             tooltip_text="Open this page in the browser")
        browser.connect("clicked", lambda *_: parent._launch(self.web.get_uri() or url))
        header.pack_end(browser)
        titles = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, valign=Gtk.Align.CENTER)
        self.title_label = Gtk.Label(label=title, ellipsize=Pango.EllipsizeMode.END)
        self.title_label.add_css_class("title")
        self.address = Gtk.Label(label=url, ellipsize=Pango.EllipsizeMode.MIDDLE)
        self.address.add_css_class("subtitle")
        titles.append(self.title_label)
        titles.append(self.address)
        header.set_title_widget(titles)

        self.progress = Gtk.ProgressBar()
        self.progress.add_css_class("kiwi")
        self.error = Gtk.Label(xalign=0, wrap=True, visible=False)
        self.error.add_css_class("statusbar")
        box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL)
        box.append(self.progress)
        box.append(self.web)
        box.append(self.error)
        self.set_child(box)

        keys = Gtk.EventControllerKey(propagation_phase=Gtk.PropagationPhase.CAPTURE)
        keys.connect("key-pressed", self._key)
        self.add_controller(keys)
        self.web.load_uri(url)

    def _create(self, _web, action):
        # Links that want a new window open in this one.
        self.web.load_uri(action.get_request().get_uri())
        return None

    def _update(self):
        self.title_label.set_text(self.web.get_title() or self.start_title)
        self.address.set_text(self.web.get_uri() or "")

    def _progress(self):
        if self.web.is_loading():
            self.progress.set_fraction(self.web.get_estimated_load_progress())
        else:
            self.progress.set_fraction(1.0)  # the stripe

    def _load_changed(self, _web, event):
        if event == WebKit.LoadEvent.STARTED:
            self.error.set_visible(False)
        self._progress()

    def _load_failed(self, _web, _event, uri, error):
        self.error.set_text(f"Could not load {uri}: {error.message}. The button on the right "
                            "opens the page in the browser.")
        self.error.set_visible(True)
        return False  # WebKit shows its own error page too

    def _key(self, _controller, keyval, _keycode, state):
        alt = state & Gdk.ModifierType.ALT_MASK
        if keyval == Gdk.KEY_Escape:
            self.close()
        elif keyval == Gdk.KEY_F5:
            self.web.reload()
        elif alt and keyval == Gdk.KEY_Left:
            self.web.go_back()
        elif alt and keyval == Gdk.KEY_Right:
            self.web.go_forward()
        else:
            return False
        return True


AppBase = Adw.Application if Adw else Gtk.Application


class AccApp(AppBase):
    def __init__(self):
        super().__init__(application_id=APP_ID)

    def do_startup(self):
        AppBase.do_startup(self)
        css = Gtk.CssProvider()
        if hasattr(css, "load_from_string"):
            css.load_from_string(CSS)
        else:
            css.load_from_data(CSS.encode())
        Gtk.StyleContext.add_provider_for_display(
            Gdk.Display.get_default(), css, Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)

        quit_action = Gio.SimpleAction.new("quit", None)
        quit_action.connect("activate", lambda *_: self.quit())
        self.add_action(quit_action)
        self.set_accels_for_action("app.quit", ["<Control>q"])
        self.set_accels_for_action("win.scan", ["F5", "<Control>r"])
        self.set_accels_for_action("win.search", ["<Control>f"])
        self.set_accels_for_action("win.open", ["<Control>o"])
        self.set_accels_for_action("win.shares", ["<Control><Shift>o"])
        self.set_accels_for_action("win.ping", ["<Control>p"])

    def do_activate(self):
        win = self.get_active_window() or AccWindow(self)
        win.present()


if __name__ == "__main__":
    if "--version" in sys.argv[1:]:
        print(f"acc-gui {GUI_VERSION}")
        sys.exit(0)
    sys.exit(AccApp().run(sys.argv))
