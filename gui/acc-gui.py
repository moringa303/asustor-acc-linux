#!/usr/bin/env python3
"""GTK4 GUI of Asustor Control Center for Linux.

Discovery, Wake-on-LAN and the device memory live in the bundled `acc`
binary; this window presents its results like the Control Center device
list and keeps them fresh.
"""
import csv
import ipaddress
import json
import os
import re
import shutil
import ssl
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import weakref
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

# libsecret keeps the ADM password in the desktop's keyring; without it
# the password is asked for once per session and never written anywhere.
try:
    gi.require_version("Secret", "1")
    from gi.repository import Secret  # noqa: E402
except (ValueError, ImportError):
    Secret = None


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
# Accent colours: (fill, deep). The fill carries dark text (the main
# button, the "for Linux" pill, the stripe); deep is the same hue for
# small marks (the Ready dot, the LED of the model glyphs). Kiwi green
# is the default; the choice is remembered in the settings file.
ACCENTS = {
    "Kiwi": ("#9EE12F", "#86CC1E"),
    "Cyan": ("#3DDCEB", "#1FB8C8"),
    "Amber": ("#FFC233", "#E0A416"),
    "Coral": ("#FF7A66", "#E85B46"),
    "Violet": ("#B98CFF", "#9A6BEA"),
}
ACCENT = {"name": "Kiwi", "fill": ACCENTS["Kiwi"][0], "deep": ACCENTS["Kiwi"][1]}
WEBSITE = "https://github.com/moringa303/asustor-acc-linux"

# Remembered choices: automatic refresh, offline devices, the accent,
# notifications, extra subnets, the SSH user, the ADM account, and the
# tags and note of each NAS (by MAC).
SETTINGS_FILE = os.path.join(GLib.get_user_config_dir(), "asustor-acc", "gui.json")
DEFAULT_SETTINGS = {"auto_refresh": False, "show_offline": True, "accent": "Kiwi",
                    "notify": True, "subnets": "", "ssh_user": "admin", "adm_account": "admin",
                    "devices": {}}


def load_settings():
    settings = json.loads(json.dumps(DEFAULT_SETTINGS))
    try:
        with open(SETTINGS_FILE, encoding="utf-8") as f:
            data = json.load(f)
        if isinstance(data, dict):
            settings.update({k: v for k, v in data.items()
                             if k in DEFAULT_SETTINGS and isinstance(v, type(DEFAULT_SETTINGS[k]))})
    except (OSError, ValueError):
        pass
    if settings["accent"] not in ACCENTS:
        settings["accent"] = DEFAULT_SETTINGS["accent"]
    return settings


def parse_subnets(text):
    """'10.1.2.0/24, 10.1.3.0/24' -> the valid CIDRs and the rejected words."""
    good, bad = [], []
    for word in re.split(r"[\s,;]+", text.strip()):
        if not word:
            continue
        try:
            net = ipaddress.ip_network(word, strict=False)
            if net.version != 4 or net.prefixlen < 22:
                raise ValueError
            good.append(str(net))
        except ValueError:
            bad.append(word)
    return good, bad


# -- ADM web API ------------------------------------------------------
# Restart and shut down go through ADM's own web API: a sign-in that
# returns a session id, then the power request with it. The paths are
# those of ADM's portal; ~/.config/asustor-acc/adm.json can replace any
# of them (the network tab of the browser shows what ADM calls when you
# click Power Off): {"restart": "settings/power.cgi?act=restart"}.
ADM_FILE = os.path.join(GLib.get_user_config_dir(), "asustor-acc", "adm.json")
ADM_ENDPOINTS = {
    "login": "login.cgi?act=login",
    "restart": "settings/power.cgi?act=restart",
    "shutdown": "settings/power.cgi?act=shutdown",
}
ADM_TIMEOUT = 15


class AdmError(Exception):
    pass


def adm_endpoints():
    endpoints = dict(ADM_ENDPOINTS)
    try:
        with open(ADM_FILE, encoding="utf-8") as f:
            data = json.load(f)
        if isinstance(data, dict):
            endpoints.update({k: v for k, v in data.items() if k in endpoints and isinstance(v, str)})
    except (OSError, ValueError):
        pass
    return endpoints


def adm_call(base_url, path, form=None, sid=None):
    """One request to ADM's API; the decoded JSON answer, or AdmError."""
    url = base_url.rstrip("/") + "/portal/apis/" + path
    if sid:
        url += ("&" if "?" in url else "?") + "sid=" + urllib.parse.quote(sid)
    data = urllib.parse.urlencode(form).encode() if form else None
    req = urllib.request.Request(url, data=data, headers={
        "Content-Type": "application/x-www-form-urlencoded; charset=UTF-8",
        "User-Agent": f"{APP_NAME} for Linux"})
    # The NAS uses a self-signed certificate on its HTTPS port.
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    try:
        with urllib.request.urlopen(req, timeout=ADM_TIMEOUT, context=ctx) as resp:
            body = resp.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as exc:
        raise AdmError(f"ADM answered {exc.code} for {path.split('?')[0]}") from exc
    except (urllib.error.URLError, OSError) as exc:
        raise AdmError(f"cannot reach ADM: {getattr(exc, 'reason', exc)}") from exc
    try:
        answer = json.loads(body)
    except ValueError:
        raise AdmError(f"ADM did not answer {path.split('?')[0]} with JSON") from None
    if not isinstance(answer, dict):
        raise AdmError("unexpected answer from ADM")
    if answer.get("success") is False:
        msg = answer.get("error_msg") or f"error {answer.get('error_code', '?')}"
        raise AdmError(f"ADM: {msg}")
    return answer


def adm_login(base_url, account, password):
    answer = adm_call(base_url, adm_endpoints()["login"],
                      {"account": account, "password": password})
    sid = answer.get("sid")
    if not sid:
        raise AdmError("ADM accepted the sign-in but gave no session id")
    return sid


def adm_power(base_url, sid, action):
    """action is 'restart' or 'shutdown'."""
    return adm_call(base_url, adm_endpoints()[action], sid=sid)


# -- the ADM password -------------------------------------------------
SECRET_SCHEMA = Secret.Schema.new(APP_ID, Secret.SchemaFlags.NONE,
                                  {"app": Secret.SchemaAttributeType.STRING}) if Secret else None


def keyring_lookup():
    if Secret is None:
        return None
    try:
        return Secret.password_lookup_sync(SECRET_SCHEMA, {"app": APP_ID}, None)
    except GLib.Error:
        return None


def keyring_store(account, password):
    if Secret is None:
        return False
    try:
        return Secret.password_store_sync(SECRET_SCHEMA, {"app": APP_ID}, Secret.COLLECTION_DEFAULT,
                                          f"{APP_NAME}: ADM password of {account}", password, None)
    except GLib.Error:
        return False


def keyring_clear():
    if Secret is None:
        return
    try:
        Secret.password_clear_sync(SECRET_SCHEMA, {"app": APP_ID}, None)
    except GLib.Error:
        pass


# -- terminals for the SSH shortcut ------------------------------------
TERMINALS = [
    ("gnome-terminal", ["--"]), ("kgx", ["--"]), ("ptyxis", ["--"]), ("konsole", ["-e"]),
    ("xfce4-terminal", ["-x"]), ("tilix", ["-e"]), ("alacritty", ["-e"]), ("kitty", []),
    ("foot", []), ("wezterm", ["start", "--"]), ("x-terminal-emulator", ["-e"]), ("xterm", ["-e"]),
]


def terminal_command():
    """The terminal emulator to run a command in: $TERMINAL, or the first
    known one that is installed. None when there is none."""
    term = os.environ.get("TERMINAL")
    if term and shutil.which(term):
        return [term, "-e"]
    for name, flag in TERMINALS:
        if shutil.which(name):
            return [name, *flag]
    return None


def adm_version_key(version):
    """'4.3.3.RC92' -> (4, 3, 3): what decides which ADM is newer."""
    return tuple(int(p) for p in re.findall(r"\d+", version)[:4])


def save_settings(settings):
    try:
        os.makedirs(os.path.dirname(SETTINGS_FILE), exist_ok=True)
        tmp = SETTINGS_FILE + ".tmp"
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(settings, f, indent=2)
        os.replace(tmp, SETTINGS_FILE)
    except OSError as exc:
        print(f"acc-gui: cannot save {SETTINGS_FILE}: {exc}", file=sys.stderr)

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
# when the window is narrow; Model, ADM and State are always shown whole
# (WHOLE), Name takes the rest. Tooltips carry the full text.
FIXED_WIDTHS = {"ip": 150, "serial": 150, "mac": 150, "port": 56}
WHOLE = {"model", "adm", "state"}

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

# The compatibility lists on asustor.com: the HDD list for AS models, the
# M.2 SSD list for the all-flash FS models. The NAS series is picked in
# the page's own filter. ~/.config/asustor-acc/links.json can replace
# either address ({model} and {media} are substituted) and override
# single models:
#   {"hdd": "https://...", "ssd": "https://...",
#    "models": {"AS6704T": {"compat": "https://..."}}}
LINK_TEMPLATES = {
    "hdd": "https://www.asustor.com/service/hd?id=hd",
    "ssd": "https://www.asustor.com/service/m2ssd?id=m2ssd",
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
    template = override.get("compat") or links.get("compat") or templates[media.lower()]
    return template.format(model=base, media=media), media

CSS = """
.state-dot, .port-dot { min-width: 10px; min-height: 10px; border-radius: 5px; background: #9a9996; }
.state-dot.uninitialized { background: #3584e4; }
.state-dot.notready, .port-dot.filtered { background: #e5a50a; }
.state-dot.offline, .port-dot.closed { background: #9a9996; }
columnview > listview > row > cell { padding: 7px 8px; }
/* The last column stays clear of the overlay scrollbar. */
columnview > listview > row > cell:last-child { padding-right: 24px; }
listview.cards > row { padding: 8px 12px; }
.card-name { font-weight: bold; }
.tabular { font-variant-numeric: tabular-nums; }
.empty-title { font-size: 1.4em; font-weight: bold; }
.statusbar { padding: 6px 12px; }
/* Tags after a name; an ADM version with a newer one on another NAS. */
.tag { background: alpha(currentColor, 0.12); border-radius: 999px; padding: 0 7px;
       font-size: 0.8em; }
label.outdated { text-decoration-line: underline; }
.health-value { font-variant-numeric: tabular-nums; }
.health-dot.warn { background: #e5a50a; }
.health-dot.bad { background: #e01b24; }
.section-title { font-weight: bold; margin-top: 10px; margin-bottom: 2px; }
/* Short fades instead of hard switches: hover and selection of a row,
   a device dimming when it goes offline, a dot changing colour. */
columnview > listview > row, listview.cards > row, list > row {
    transition: background-color 120ms ease-out; }
columnview > listview > row label, listview.cards > row label {
    transition: color 150ms ease-out, opacity 150ms ease-out; }
.state-dot, .port-dot { transition: background-color 200ms ease-out; }
"""

# The accent part of the stylesheet, regenerated when the accent changes:
# the stripe under the header bar, the "for Linux" pill, the main action
# button, the empty page's icon, the Ready and open-port dots.
ACCENT_CSS = """
.accent-stripe { min-height: 3px; background: FILL; }
.pill { background: FILL; color: #101418; border-radius: 999px; padding: 1px 8px;
        font-size: 0.72em; font-weight: bold; }
button.accent { background-color: FILL; background-image: none; color: #101418; }
button.accent:hover { background-color: shade(FILL, 1.08); }
button.accent:active, button.accent:checked { background-color: DEEP; }
button.accent:disabled { background-color: alpha(FILL, 0.35); color: alpha(#101418, 0.5); }
.accent-icon { color: DEEP; }
.state-dot.ready, .port-dot.open, .health-dot.ok { background: DEEP; }
label.outdated { text-decoration-color: DEEP; }
levelbar.accent block.filled { background-color: FILL; border-color: DEEP; }
levelbar.accent block.high { background-color: #e5a50a; border-color: #c48a00; }
levelbar.accent block.full { background-color: #e01b24; border-color: #a51d2d; }
progressbar.accent trough { min-height: 3px; border: none; border-radius: 0; background: alpha(FILL, 0.25); }
progressbar.accent progress { min-height: 3px; border: none; border-radius: 0; background: FILL; }
"""
_accent_provider = None


def _load_css(provider, css):
    if hasattr(provider, "load_from_string"):
        provider.load_from_string(css)
    else:
        provider.load_from_data(css.encode())


def apply_accent(name):
    """Switch the accent colour app-wide: the stylesheet and the LEDs of
    the model glyphs, which are drawn by hand."""
    global _accent_provider
    fill, deep = ACCENTS[name]
    ACCENT.update(name=name, fill=fill, deep=deep)
    if _accent_provider is None:
        _accent_provider = Gtk.CssProvider()
        Gtk.StyleContext.add_provider_for_display(
            Gdk.Display.get_default(), _accent_provider,
            Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION + 1)
    _load_css(_accent_provider, ACCENT_CSS.replace("FILL", fill).replace("DEEP", deep))
    for glyph in list(ModelGlyph.instances):
        glyph.queue_draw()


def accent_stripe():
    stripe = Gtk.Box()
    stripe.add_css_class("accent-stripe")
    return stripe


def fill_tags(box, dev):
    """Rebuild the pills of a tag box from the device's tags."""
    while (child := box.get_first_child()) is not None:
        box.remove(child)
    for tag in dev.tag_list():
        pill = Gtk.Label(label=tag)
        pill.add_css_class("tag")
        box.append(pill)


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
    LED = 3  # the power LED, in the accent colour while the NAS is online
    instances = weakref.WeakSet()  # every glyph alive, for accent changes

    def __init__(self):
        super().__init__(valign=Gtk.Align.CENTER)
        self.set_size_request(self.SIZE, self.SIZE)
        self.bays, self.kind = 0, "tower"
        self.lit = False
        ModelGlyph.instances.add(self)

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
            accent = Gdk.RGBA()
            accent.parse(ACCENT["deep"])
            snapshot.append_color(accent, Graphene.Rect().init(led_x, led_y, led, led))


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
    # Your own tags (comma-separated) and note, from the settings.
    tags = GObject.Property(type=str, default="")
    note = GObject.Property(type=str, default="")
    # Tooltips of the IP cell (the latency) and the ADM cell (a newer ADM
    # on another NAS), and whether the ADM is behind the newest seen.
    ip_tip = GObject.Property(type=str, default="")
    adm_tip = GObject.Property(type=str, default="")
    outdated = GObject.Property(type=bool, default=False)

    def __init__(self, data, meta=None):
        super().__init__()
        self.data = {}
        self.answered = False  # did the NAS answer the last scan
        self.known_state = ""  # its state when it last answered
        self.uptime = None  # seconds the NAS reported, as of last_seen
        self.last_seen = None
        self.seen_tip = ""
        self.rtt = ""  # "2.3 ms", the TCP round trip to ADM measured by the scan
        self.text = ""  # every field in one lower-case string, for the search
        if meta:
            self.tags = ",".join(meta.get("tags") or [])
            self.note = meta.get("note") or ""
        self.update(data)

    def tag_list(self):
        return [t for t in self.tags.split(",") if t]

    def set_meta(self, tags, note):
        """Tags (a list) and note from the settings; refreshes the search text."""
        self.tags, self.note = ",".join(tags), note
        self._set_online(self.online)

    def set_adm_state(self, outdated, tip):
        if outdated != self.outdated:
            self.outdated = outdated
        if tip != self.adm_tip:
            self.adm_tip = tip

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
        rtt = data.get("rtt_ms")
        self.rtt = f"{rtt:g} ms" if self.answered and isinstance(rtt, (int, float)) else ""
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
        # The latency is shown on request only (Ping, Details), not here.
        if self.ip != self.ip_tip:
            self.ip_tip = self.ip
        self.text = " ".join((self.name, self.ip, self.model, self.serial, self.mac, self.adm,
                              self.port, self.state, self.tags.replace(",", " "), self.note)).lower()
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
        self.settings = load_settings()
        self.auto_refresh = self.settings["auto_refresh"]
        self.show_offline = self.settings["show_offline"]
        apply_accent(self.settings["accent"])
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
        self._auto = False  # the running scan was started by the timer
        self.adm_password = None  # for this session, when not in the keyring
        self.prefs_win = None

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
            ("ports", self.check_ports),
            ("health", self.show_health),
            ("ssh", self.open_ssh),
            ("tags", self.edit_tags),
            ("export", self.export_list),
            ("restart", lambda: self.power("restart")),
            ("shutdown", lambda: self.power("shutdown")),
            ("adm-window", self.open_adm_window),
            ("prefs", self.show_prefs),
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
        accent = Gio.SimpleAction.new_stateful(
            "accent", GLib.VariantType("s"), GLib.Variant("s", self.settings["accent"]))
        accent.connect("change-state", self._set_accent)
        self.add_action(accent)

    def _toggle(self, action, value, attr):
        action.set_state(value)
        setattr(self, attr, value.get_boolean())
        self.settings[attr] = value.get_boolean()
        save_settings(self.settings)
        if attr == "show_offline":
            self.filter.changed(Gtk.FilterChange.DIFFERENT)
            self._refresh_summary()

    def _set_accent(self, action, value):
        name = value.get_string()
        if name not in ACCENTS:
            return
        action.set_state(value)
        self.settings["accent"] = name
        apply_accent(name)
        save_settings(self.settings)

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
        section.append("Details (SNMP)…", "win.health")
        section.append("Wake (WOL)", "win.wake")
        section.append("Ping", "win.ping")
        section.append("Check services (ports)…", "win.ports")
        menu.append_section(None, section)
        menu.append_section(None, self._link_menu())
        section = Gio.Menu()
        section.append(f"Refresh every {REFRESH_SECONDS} s", "win.auto-refresh")
        section.append("Show offline devices", "win.show-offline")
        menu.append_section(None, section)
        section = Gio.Menu()
        section.append("Export device list…", "win.export")
        section.append("Diagnose discovery…", "win.doctor")
        section.append("Forget offline devices", "win.forget-offline")
        menu.append_section(None, section)
        section = Gio.Menu()
        section.append("Preferences…", "win.prefs")
        section.append(f"About {APP_NAME}", "win.about")
        menu.append_section(None, section)
        menu_btn = Gtk.MenuButton(icon_name="open-menu-symbolic", menu_model=menu, primary=True)
        menu_btn.set_tooltip_text("Menu (F10)")
        header.pack_end(menu_btn)

        self.open_btn = Gtk.Button(label="Open ADM", action_name="win.open")
        self.open_btn.add_css_class("accent")
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
        section.append("Open ADM in a window", "win.adm-window")
        section.append("Open shared folders", "win.shares")
        section.append("Details (SNMP)…", "win.health")
        section.append("Wake (WOL)", "win.wake")
        section.append("Ping", "win.ping")
        section.append("Check services (ports)…", "win.ports")
        section.append("Open SSH in a terminal", "win.ssh")
        model.append_section(None, section)
        model.append_section(None, self._link_menu())
        section = Gio.Menu()
        section.append("Tags and note…", "win.tags")
        section.append("Copy IP address", "win.copy-ip")
        section.append("Copy MAC address", "win.copy-mac")
        model.append_section(None, section)
        section = Gio.Menu()
        section.append("Restart…", "win.restart")
        section.append("Shut down…", "win.shutdown")
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
        self.stack = Gtk.Stack(vexpand=True, hhomogeneous=False,
                               transition_type=Gtk.StackTransitionType.CROSSFADE,
                               transition_duration=150)
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
        box.append(accent_stripe())
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
        icon.add_css_class("accent-icon")
        self.empty_title = Gtk.Label(label="No ASUSTOR NAS found", wrap=True,
                                     justify=Gtk.Justification.CENTER)
        self.empty_title.add_css_class("empty-title")
        self.empty_text = Gtk.Label(wrap=True, max_width_chars=52, justify=Gtk.Justification.CENTER)
        self.empty_text.add_css_class("dim-label")
        buttons = Gtk.Box(spacing=12, halign=Gtk.Align.CENTER, margin_top=12)
        again = Gtk.Button(label="Scan again", action_name="win.scan")
        again.add_css_class("accent")
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
        # the tooltip carries the full text. Model, ADM and State are
        # always shown whole (the table gives way to the cards instead).
        whole = prop in WHOLE
        label = Gtk.Label(xalign=0, ellipsize=Pango.EllipsizeMode.NONE if whole
                          else Pango.EllipsizeMode.END)
        if prop in TABULAR:
            label.add_css_class("tabular")
        if prop == "name":
            # The name, then its tags as small pills.
            box = Gtk.Box(spacing=6)
            label.set_hexpand(True)
            box.append(label)
            box.tags = Gtk.Box(spacing=4, valign=Gtk.Align.CENTER)
            box.append(box.tags)
            box.label = label
            item.set_child(box)
            return
        if not whole or prop == "adm":
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
        tip = {"state": "state_tip", "ip": "ip_tip", "adm": "adm_tip", "name": "note"}.get(prop, prop)
        target = widget if prop == "state" else label
        item.bindings.append(dev.bind_property(tip, target, "tooltip-text", flags))
        item.handlers = [dev.connect("notify::online", lambda *_: self._style(widget, dev))]
        if prop == "model":
            widget.glyph.set_model(dev.model)
            item.handlers.append(dev.connect(
                "notify::model", lambda *_: widget.glyph.set_model(dev.model)))
        if prop == "state":
            item.handlers.append(dev.connect("notify::state", lambda *_: self._style(widget, dev)))
        if prop == "adm":
            item.handlers.append(dev.connect("notify::outdated", lambda *_: self._style(widget, dev)))
        if prop == "name":
            fill_tags(widget.tags, dev)
            item.handlers.append(dev.connect("notify::tags", lambda *_: fill_tags(widget.tags, dev)))
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
        row.tags = Gtk.Box(spacing=4, valign=Gtk.Align.CENTER)
        for w in (row.glyph, row.dot, row.model, row.name, row.tags):
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
                         dev.connect("notify::model", lambda *_: row.glyph.set_model(dev.model)),
                         dev.connect("notify::tags", lambda *_: fill_tags(row.tags, dev))]
        fill_tags(row.tags, dev)
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
        if isinstance(widget, Gtk.Label) and widget.get_label() == dev.adm:
            # the ADM cell: a dotted underline when a newer ADM runs elsewhere
            if dev.outdated:
                widget.add_css_class("outdated")
            else:
                widget.remove_css_class("outdated")
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

        args = [ACC_BIN, "scan", "--json", "--all", "--timeout", str(SCAN_TIMEOUT)]
        for cidr in parse_subnets(self.settings.get("subnets", ""))[0]:
            args += ["--subnet", cidr]

        def worker():
            try:
                out = subprocess.run(
                    args, capture_output=True, text=True, check=True, timeout=SCAN_TIMEOUT * 5,
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
        self._auto = False
        self._refresh_summary()
        return GLib.SOURCE_REMOVE

    def _apply(self, devices):
        """Update the views in place so selection and scroll position stay."""
        fresh = {Device.key_of(d): d for d in devices}
        meta = self.settings.get("devices", {})
        first = self.last_scan is None
        went_off, came_back, new = [], [], []
        i = 0
        while i < self.store.get_n_items():
            dev = self.store.get_item(i)
            data = fresh.pop(dev.key, None)
            if data is None:
                self.store.remove(i)
            else:
                was = dev.online
                dev.update(data)
                if was and not dev.online:
                    went_off.append(dev.name)
                elif dev.online and not was:
                    came_back.append(dev.name)
                i += 1
        # One splice instead of one append per device: every insert into
        # the sorted model shifts rows and makes the views rebind, which
        # with a few hundred devices takes seconds of main-loop time.
        if fresh:
            added = [Device(d, meta.get(Device.key_of(d))) for d in fresh.values()]
            new = [d.name for d in added if d.online]
            self.store.splice(self.store.get_n_items(), 0, added)
        self._mark_adm()
        self.filter.changed(Gtk.FilterChange.DIFFERENT)
        sorter = self.view.get_sorter()
        if sorter is not None:
            sorter.changed(Gtk.SorterChange.DIFFERENT)
        if not first and (self._auto or not self.is_active()):
            self._notify_presence(went_off, came_back, new)

    def _mark_adm(self):
        """Mark the ADM versions that are behind the newest one seen on
        any NAS: a dotted underline, the details in the tooltip."""
        devices = [d for d in self._devices() if d.adm]
        if not devices:
            return
        newest = max(devices, key=lambda d: adm_version_key(d.adm))
        newest_key = adm_version_key(newest.adm)
        for d in devices:
            behind = adm_version_key(d.adm) < newest_key
            tip = f"ADM {d.adm}"
            if behind:
                tip += f" · {newest.name} runs the newer ADM {newest.adm}"
            d.set_adm_state(behind, tip)

    def _notify_presence(self, went_off, came_back, new):
        """Desktop notifications for devices that changed presence."""
        if not self.settings.get("notify", True):
            return
        app = self.get_application()
        if app is None:
            return
        for ident, title, names in (("offline", "NAS offline", went_off),
                                    ("online", "NAS back online", came_back),
                                    ("new", "New NAS found", new)):
            if not names:
                continue
            body = ", ".join(names[:3]) + (f" and {len(names) - 3} more" if len(names) > 3 else "")
            note = Gio.Notification.new(title)
            note.set_body(body)
            note.set_icon(Gio.ThemedIcon.new(APP_ID))
            app.send_notification(f"presence-{ident}", note)

    def _auto_scan(self):
        if self.auto_refresh and not self.scanning:
            self._auto = True
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
        self.lookup_action("ports").set_enabled(dev is not None and bool(dev.ip))
        self.lookup_action("ssh").set_enabled(dev is not None and bool(dev.ip))
        self.lookup_action("health").set_enabled(dev is not None and dev.online and bool(dev.ip))
        self.lookup_action("tags").set_enabled(dev is not None)
        can_power = dev is not None and dev.online and dev.initialized and bool(dev.url)
        for name in ("restart", "shutdown", "adm-window"):
            self.lookup_action(name).set_enabled(can_power)
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

    def check_ports(self):
        dev = self.selected()
        if dev is None or not dev.ip:
            return
        PortsWindow(self, dev).present()

    def show_health(self):
        dev = self.selected()
        if dev is None or not dev.ip:
            return
        HealthWindow(self, dev).present()

    def open_adm_window(self):
        dev = self.selected()
        if dev is None or not dev.url:
            return
        if WebKit is None:
            self.set_status(f"Opening ADM in the browser ({WEBKIT_HINT})…")
            self._launch(dev.url)
            return
        WebWindow(self, dev.url, f"ADM · {dev.name}").present()

    def open_ssh(self):
        dev = self.selected()
        if dev is None or not dev.ip:
            return
        term = terminal_command()
        if term is None:
            self.set_status("No terminal emulator found; set $TERMINAL to yours.", sticky=True)
            return
        user = self.settings.get("ssh_user") or "admin"
        try:
            subprocess.Popen([*term, "ssh", f"{user}@{dev.ip}"],
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        except OSError as exc:
            self.set_status(f"Could not start {term[0]}: {exc}", sticky=True)
            return
        self.set_status(f"ssh {user}@{dev.ip} in {term[0]} (SSH must be enabled in ADM › Services)")

    def edit_tags(self):
        dev = self.selected()
        if dev is not None:
            TagsDialog(self, dev, self._save_tags).present()

    def _save_tags(self, dev, tags, note):
        devices = self.settings.setdefault("devices", {})
        if tags or note:
            devices[dev.key] = {"tags": tags, "note": note}
        else:
            devices.pop(dev.key, None)
        save_settings(self.settings)
        dev.set_meta(tags, note)
        self.filter.changed(Gtk.FilterChange.DIFFERENT)
        self._refresh_summary()

    def export_list(self):
        """The list as shown (filter and order) to a CSV file."""
        devices = [self.sorted.get_item(i) for i in range(self.sorted.get_n_items())]
        if not devices:
            self.set_status("Nothing to export.")
            return
        name = f"asustor-nas-{time.strftime('%Y-%m-%d')}.csv"

        def write(path):
            if not path:
                return
            try:
                with open(path, "w", newline="", encoding="utf-8") as f:
                    out = csv.writer(f)
                    out.writerow(["name", "model", "ip", "serial", "mac", "adm", "port", "state",
                                  "uptime", "tags", "note"])
                    for d in devices:
                        up = d.uptime_now()
                        out.writerow([d.name, d.model, d.ip, d.serial, d.mac, d.adm, d.port, d.state,
                                      format_uptime(up) if up is not None else "",
                                      ", ".join(d.tag_list()), d.note])
            except OSError as exc:
                self.set_status(f"Could not write {path}: {exc}", sticky=True)
                return
            self.set_status(f"Exported {len(devices)} device(s) to {path}")

        if hasattr(Gtk, "FileDialog"):
            dialog = Gtk.FileDialog(title="Export device list", initial_name=name)

            def done(d, res):
                try:
                    f = d.save_finish(res)
                except GLib.Error:
                    return
                write(f.get_path() if f else None)
            dialog.save(self, None, done)
        else:
            chooser = Gtk.FileChooserNative.new("Export device list", self,
                                                Gtk.FileChooserAction.SAVE, "Export", "Cancel")
            chooser.set_current_name(name)

            def response(c, r):
                if r == Gtk.ResponseType.ACCEPT:
                    write(c.get_file().get_path())
                c.destroy()
            chooser.connect("response", response)
            chooser.show()

    def show_prefs(self):
        if self.prefs_win is None:
            self.prefs_win = PreferencesWindow(self)
            self.prefs_win.connect("close-request", lambda *_: setattr(self, "prefs_win", None) or False)
        self.prefs_win.present()

    # -- restart and shut down through ADM ---------------------------------
    def power(self, action):
        dev = self.selected()
        if dev is None or not dev.online or not dev.url:
            return
        verb = "Restart" if action == "restart" else "Shut down"
        what = "restarts" if action == "restart" else "shuts down"
        self._confirm(f"{verb} {dev.name}?",
                      f"ADM at {dev.ip} signs you in with your ADM account and {what} the NAS. "
                      "Anyone using it loses the connection.",
                      verb, lambda: self._with_credentials(dev, lambda a, p: self._run_power(
                          dev, action, verb, a, p)))

    def _confirm(self, title, body, verb, then):
        if hasattr(Gtk, "AlertDialog"):
            dialog = Gtk.AlertDialog(message=title, detail=body, buttons=["Cancel", verb],
                                     cancel_button=0, default_button=0, modal=True)

            def chosen(d, res):
                try:
                    if d.choose_finish(res) == 1:
                        then()
                except GLib.Error:
                    pass
            dialog.choose(self, None, chosen)
            return
        dialog = Gtk.MessageDialog(transient_for=self, modal=True, text=title, secondary_text=body)
        dialog.add_button("Cancel", Gtk.ResponseType.CANCEL)
        dialog.add_button(verb, Gtk.ResponseType.OK)

        def response(d, r):
            d.destroy()
            if r == Gtk.ResponseType.OK:
                then()
        dialog.connect("response", response)
        dialog.present()

    def _with_credentials(self, dev, then):
        account = self.settings.get("adm_account") or "admin"
        password = self.adm_password or keyring_lookup()
        if password:
            then(account, password)
            return
        CredentialsDialog(self, dev, account,
                          lambda a, p, remember: self._got_credentials(a, p, remember, then)).present()

    def _got_credentials(self, account, password, remember, then):
        self.settings["adm_account"] = account
        save_settings(self.settings)
        self.adm_password = password
        if remember and keyring_store(account, password):
            self.set_status("The ADM password is in your keyring; Preferences forgets it.")
        then(account, password)

    def _run_power(self, dev, action, verb, account, password):
        self.set_status(f"Signing in to ADM on {dev.name} as {account}…", sticky=True)

        def worker():
            login_failed = False
            try:
                try:
                    sid = adm_login(dev.url, account, password)
                except AdmError:
                    login_failed = True
                    raise
                adm_power(dev.url, sid, action)
                msg, ok = f"{dev.name} is {'restarting' if action == 'restart' else 'shutting down'}.", True
            except AdmError as exc:
                msg, ok = f"Could not {verb.lower()} {dev.name}: {exc}", False
                if login_failed:
                    msg += " The password is forgotten; the next try asks again."
            GLib.idle_add(lambda: self._power_done(msg, ok, login_failed) and False)

        threading.Thread(target=worker, daemon=True).start()

    def _power_done(self, msg, ok, login_failed):
        if login_failed:
            self.adm_password = None
            keyring_clear()
        self.set_status(msg, sticky=not ok)

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
        version = GUI_VERSION if GUI_VERSION != "dev" else (self.acc_version or "dev")
        comments = ("Unofficial Linux version of the ASUSTOR Control Center: NAS discovery, "
                    "ADM launcher, shared folders, Wake-on-LAN, services check and SNMP details.")
        debug = "\n".join([
            f"acc-gui {GUI_VERSION}",
            f"acc {self.acc_version or '?'} at {ACC_PATH}",
            f"GTK {Gtk.get_major_version()}.{Gtk.get_minor_version()}.{Gtk.get_micro_version()}",
            f"libadwaita {Adw.MAJOR_VERSION}.{Adw.MINOR_VERSION}" if Adw else "libadwaita: not installed",
            f"WebKitGTK {WebKit.get_major_version()}.{WebKit.get_minor_version()}" if WebKit
            else "WebKitGTK: not installed (the compatibility list opens in the browser)",
            "libsecret: available" if Secret else "libsecret: not installed (the ADM password "
                                                  "is kept for the session only)",
        ])
        if Adw is not None and hasattr(Adw, "AboutWindow"):
            about = Adw.AboutWindow(
                transient_for=self, modal=True, application_name=APP_FULL_NAME,
                application_icon=APP_ID, developer_name="Frank Eckardt", version=version,
                comments=comments, website=WEBSITE, issue_url=f"{WEBSITE}/issues",
                license_type=Gtk.License.MIT_X11, debug_info=debug,
                debug_info_filename="asustor-acc-debug.txt")
            about.present()
            return
        about = Gtk.AboutDialog(
            transient_for=self, modal=True, program_name=APP_FULL_NAME, version=version,
            comments=comments, website=WEBSITE, license_type=Gtk.License.MIT_X11,
            system_information=debug, authors=["Frank Eckardt"])
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


def format_gb(gb):
    if gb is None:
        return "?"
    return f"{gb / 1000:.1f} TB" if gb >= 1000 else f"{gb:g} GB"


def verdict(*words):
    """'ok', 'warn' or 'bad' from the status words ADM reports: disks are
    Healthy, Normal, Risky, Dangerous or Bad; volumes healthy, clean,
    active, then the busy states (resyncing, recovering, ...), then
    degraded, failed and inactive."""
    text = " ".join(w for w in words if w).lower()
    if any(w in text for w in ("bad", "danger", "fail", "inactive", "degrad", "critical", "error")):
        return "bad"
    if any(w in text for w in ("risky", "warn", "resync", "recover", "reshap", "append", "migrat",
                               "expand", "initing", "abnormal", "unknown")):
        return "warn"
    return "ok"


def raid_name(level):
    """'raid1' -> 'RAID 1', 'single' -> 'Single', 'jbod' -> 'JBOD'."""
    low = (level or "").lower()
    if low.startswith("raid"):
        return "RAID " + low[4:]
    if low == "jbod":
        return "JBOD"
    return level[:1].upper() + level[1:] if level else ""


UPS_FLAGS = {"OL": "on line", "OB": "on battery", "LB": "low battery", "HB": "high battery",
             "RB": "replace battery", "CHRG": "charging", "DISCHRG": "discharging", "BYPASS": "bypass",
             "CAL": "calibrating", "OFF": "off", "OVER": "overloaded", "TRIM": "trimming",
             "BOOST": "boosting", "FSD": "forced shutdown"}


def ups_status_text(status):
    return ", ".join(UPS_FLAGS.get(f.upper(), f) for f in (status or "").split())


class ChildWindow(Gtk.Window):
    """A window of the app placed over the main window: header bar with a
    title and subtitle, accent stripe, a body and a status line."""

    def __init__(self, parent, title, subtitle, width, height):
        super().__init__(title=title, application=parent.get_application(),
                         transient_for=parent, destroy_with_parent=True,
                         default_width=width, default_height=height)
        self.set_size_request(MIN_WIDTH, 300)
        self.set_icon_name(APP_ID)
        self.parent_window = parent
        self.header = Gtk.HeaderBar()
        self.set_titlebar(self.header)
        titles = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, valign=Gtk.Align.CENTER)
        self.title_label = Gtk.Label(label=title, ellipsize=Pango.EllipsizeMode.END)
        self.title_label.add_css_class("title")
        self.subtitle_label = Gtk.Label(label=subtitle, ellipsize=Pango.EllipsizeMode.MIDDLE)
        self.subtitle_label.add_css_class("subtitle")
        titles.append(self.title_label)
        titles.append(self.subtitle_label)
        self.header.set_title_widget(titles)
        self.body = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=6,
                            margin_top=12, margin_bottom=12, margin_start=12, margin_end=12)
        self.summary = Gtk.Label(xalign=0, wrap=True, wrap_mode=Pango.WrapMode.WORD_CHAR)
        self.summary.add_css_class("dim-label")
        self.summary.add_css_class("statusbar")
        box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL)
        box.append(accent_stripe())
        box.append(Gtk.ScrolledWindow(child=self.body, vexpand=True,
                                      hscrollbar_policy=Gtk.PolicyType.NEVER))
        box.append(Gtk.Separator())
        box.append(self.summary)
        self.set_child(box)
        keys = Gtk.EventControllerKey(propagation_phase=Gtk.PropagationPhase.CAPTURE)
        keys.connect("key-pressed", self._key)
        self.add_controller(keys)

    def clear_body(self):
        while (child := self.body.get_first_child()) is not None:
            self.body.remove(child)

    def _key(self, _controller, keyval, _keycode, _state):
        if keyval == Gdk.KEY_Escape:
            self.close()
            return True
        if keyval == Gdk.KEY_F5 and hasattr(self, "check"):
            self.check()
            return True
        return False


class HealthWindow(ChildWindow):
    """What the NAS reports about itself over SNMP (`acc health`): system,
    disks and volumes, refreshed every REFRESH seconds while open."""

    REFRESH = 15

    def __init__(self, parent, dev):
        super().__init__(parent, f"{dev.name} · details", dev.ip, 560, 700)
        self.dev = dev
        self.checking = False
        again = Gtk.Button(icon_name="view-refresh-symbolic", tooltip_text="Refresh (F5)")
        again.connect("clicked", lambda *_: self.check())
        self.header.pack_start(again)
        self.spinner = Gtk.Spinner()
        self.header.pack_start(self.spinner)
        self.timer = GLib.timeout_add_seconds(self.REFRESH, self._tick)
        self.connect("close-request", self._closing)
        self.check()

    def _closing(self, *_):
        if self.timer:
            GLib.source_remove(self.timer)
            self.timer = None
        return False

    def _tick(self):
        self.check()
        return GLib.SOURCE_CONTINUE

    def check(self):
        if self.checking:
            return
        self.checking = True
        self.spinner.start()
        self.parent_window._run_acc(["health", self.dev.ip, "--json"], self._done)

    def _done(self, out, err):
        self.checking = False
        self.spinner.stop()
        try:
            report = json.loads(out) if out else None
        except ValueError:
            report = None
        self.clear_body()
        if not isinstance(report, dict):
            self._problem(f"No SNMP answer from {self.dev.ip}.",
                          "Enable SNMP in ADM › Services › SNMP (v1/v2c, community “public”); "
                          "the uptime, temperatures, fans, disks and volumes come from there.",
                          err)
            return
        self._render(report)
        self.summary.set_text(f"Read over SNMP at {time.strftime('%H:%M:%S')}; "
                              f"refreshes every {self.REFRESH} s while open.")

    def _problem(self, title, text, detail):
        page = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=8, margin_top=24,
                       halign=Gtk.Align.CENTER, valign=Gtk.Align.CENTER)
        icon = Gtk.Image.new_from_icon_name("network-server-symbolic")
        icon.set_pixel_size(64)
        icon.add_css_class("accent-icon")
        head = Gtk.Label(label=title, wrap=True, justify=Gtk.Justification.CENTER)
        head.add_css_class("empty-title")
        body = Gtk.Label(label=text, wrap=True, max_width_chars=48, justify=Gtk.Justification.CENTER)
        body.add_css_class("dim-label")
        button = Gtk.Button(label="Open ADM", halign=Gtk.Align.CENTER, margin_top=8)
        button.add_css_class("accent")
        button.connect("clicked", lambda *_: self.parent_window._launch(self.dev.url))
        for w in (icon, head, body, button):
            page.append(w)
        self.body.append(page)
        self.summary.set_text(detail.splitlines()[0] if detail else "")

    def _section(self, title):
        """A titled card holding a two-column grid: keys on the left, all
        of them the same width across the window, values on the right."""
        label = Gtk.Label(label=title, xalign=0)
        label.add_css_class("section-title")
        self.body.append(label)
        grid = Gtk.Grid(column_spacing=16, row_spacing=10, margin_top=12, margin_bottom=12,
                        margin_start=14, margin_end=14)
        card = Gtk.Frame(child=grid)
        card.add_css_class("card")
        self.body.append(card)
        grid.rows = 0
        return grid

    def _row(self, grid, key, value, detail=None, level=None, state=None):
        """One line of a card: the key, then the value with an optional
        status dot before it, a level bar under it and a dim detail line."""
        k = Gtk.Label(label=key, xalign=1, yalign=0, valign=Gtk.Align.START)
        k.add_css_class("dim-label")
        self.keys.add_widget(k)
        grid.attach(k, 0, grid.rows, 1, 1)
        cell = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=3, hexpand=True)
        line = Gtk.Box(spacing=8)
        if state is not None:
            dot = Gtk.Box(valign=Gtk.Align.CENTER)
            for cls in ("state-dot", "health-dot", state):
                dot.add_css_class(cls)
            line.append(dot)
        v = Gtk.Label(label=value, xalign=0, hexpand=True, wrap=True, wrap_mode=Pango.WrapMode.WORD_CHAR)
        v.add_css_class("health-value")
        line.append(v)
        cell.append(line)
        if level is not None:
            bar = Gtk.LevelBar(min_value=0, max_value=1, value=max(0.0, min(1.0, level)))
            bar.add_offset_value("low", 0.8)
            bar.add_offset_value("high", 0.95)
            bar.add_offset_value("full", 1.0)
            bar.add_css_class("accent")
            cell.append(bar)
        if detail:
            d = Gtk.Label(label=detail, xalign=0, wrap=True, wrap_mode=Pango.WrapMode.WORD_CHAR)
            d.add_css_class("dim-label")
            d.add_css_class("caption")
            cell.append(d)
        grid.attach(cell, 1, grid.rows, 1, 1)
        grid.rows += 1

    def _render(self, r):
        self.keys = Gtk.SizeGroup(mode=Gtk.SizeGroupMode.HORIZONTAL)
        system = self._section("System")
        self._row(system, "Model", r.get("model") or self.dev.model or "?", r.get("processor"))
        update = r.get("adm_update_available")
        self._row(system, "ADM", r.get("adm_version") or self.dev.adm or "?",
                  "An update is available for download" if update
                  else ("Up to date" if update is False else None),
                  state="warn" if update else None)
        if r.get("bios_version"):
            self._row(system, "BIOS", r["bios_version"])
        if r.get("serial_number"):
            self._row(system, "Serial", r["serial_number"])
        if r.get("uptime_seconds") is not None:
            self._row(system, "Uptime", format_uptime(r["uptime_seconds"]))
        elif r.get("uptime_text"):
            self._row(system, "Uptime", r["uptime_text"])
        if self.dev.rtt:
            self._row(system, "Latency", self.dev.rtt, "TCP round trip to ADM, measured by the last scan")
        cpu = r.get("cpu_usage") or []
        if cpu:
            avg = sum(cpu) / len(cpu)
            self._row(system, "CPU", f"{avg:.0f}% busy",
                      ("per core: " + " · ".join(f"{c}%" for c in cpu)) if len(cpu) > 1 else None,
                      level=avg / 100)
        total, free = r.get("total_mem_mb"), r.get("free_mem_mb")
        if total:
            used = total - (free or 0)
            self._row(system, "Memory", f"{used / 1024:.1f} of {total / 1024:.1f} GB used "
                      f"({used * 100 // total}%)", level=used / total)
        temps = []
        if r.get("sys_temp_c") is not None:
            temps.append(f"system {r['sys_temp_c']} °C")
        if r.get("cpu_temp_c") is not None:
            temps.append(f"CPU {r['cpu_temp_c']} °C")
        if temps:
            self._row(system, "Temperature", " · ".join(temps))
        fans = r.get("fan_rpm") or []
        if fans:
            self._row(system, "Fans", " · ".join(f"{f} rpm" for f in fans))
        if not r.get("model") and not r.get("disks") and not r.get("volumes"):
            self._row(system, "ASUSTOR MIB", "not available",
                      "The NAS answers SNMP without its own MIB; only the uptime is known.")
        disks = r.get("disks") or []
        if disks:
            grid = self._section("Disks")
            for d in disks:
                parts = [d.get("model"), d.get("type"),
                         format_gb(d["size_gb"]) if d.get("size_gb") else "",
                         f"{d['temp_c']} °C" if d.get("temp_c") is not None else ""]
                detail = [d.get("status") or "", f"SMART {d['smart']}" if d.get("smart") else ""]
                self._row(grid, d.get("name") or f"Disk {d.get('index')}",
                          " · ".join(p for p in parts if p) or "no details",
                          " · ".join(p for p in detail if p) or None,
                          state=verdict(d.get("status"), d.get("smart")))
        volumes = r.get("volumes") or []
        if volumes:
            grid = self._section("Volumes")
            for v in volumes:
                total, free = v.get("total_gb"), v.get("free_gb")
                used = (total - free) if total is not None and free is not None else None
                value = (f"{format_gb(used)} of {format_gb(total)} used ({used * 100 // total}%)"
                         if used is not None and total else "size unknown")
                detail = [raid_name(v.get("level")), v.get("filesystem") or "", v.get("status") or ""]
                self._row(grid, v.get("name") or f"Volume {v.get('index')}", value,
                          " · ".join(p for p in detail if p) or None,
                          level=(used / total) if used is not None and total else None,
                          state=verdict(v.get("status")))
        ups = r.get("ups")
        if isinstance(ups, dict) and (ups.get("model") or ups.get("status")):
            grid = self._section("UPS")
            model = " ".join(p for p in (ups.get("manufacturer"), ups.get("model")) if p)
            if model:
                self._row(grid, "Model", model)
            status = ups.get("status") or ""
            if status:
                flags = status.upper().split()
                self._row(grid, "Status", ups_status_text(status),
                          state="bad" if "LB" in flags or "RB" in flags
                          else ("warn" if "OB" in flags else "ok"))
            charge = ups.get("charge_percent")
            if charge is not None:
                left = ups.get("runtime_seconds")
                self._row(grid, "Battery", f"{charge}%"
                          + (f" · {format_uptime(left)} left" if left else ""), level=charge / 100)
            if ups.get("input_voltage") is not None:
                self._row(grid, "Input", f"{ups['input_voltage']} V")


class PreferencesWindow(Gtk.Window):
    """Accent, notifications, extra subnets, the SSH user and the ADM
    account. Everything applies at once and is remembered."""

    def __init__(self, parent):
        super().__init__(title="Preferences", application=parent.get_application(),
                         transient_for=parent, destroy_with_parent=True, default_width=560)
        self.set_size_request(MIN_WIDTH, -1)
        self.set_icon_name(APP_ID)
        self.parent_window = parent
        self.set_titlebar(Gtk.HeaderBar())
        settings = parent.settings
        rows = Gtk.ListBox(selection_mode=Gtk.SelectionMode.NONE, margin_top=12,
                           margin_bottom=12, margin_start=12, margin_end=12)
        rows.add_css_class("boxed-list" if Adw else "rich-list")

        names = list(ACCENTS)
        accent = Gtk.DropDown.new_from_strings(names)
        accent.set_selected(names.index(settings["accent"]) if settings["accent"] in names else 0)
        accent.connect("notify::selected", lambda d, _p: parent.activate_action(
            "accent", GLib.Variant("s", names[d.get_selected()])))
        self._row(rows, "Accent colour", "Stripe, main button, Ready dots, glyph LEDs.", accent)

        notify = Gtk.Switch(active=bool(settings.get("notify", True)), valign=Gtk.Align.CENTER)
        notify.connect("notify::active", lambda s, _p: self._set("notify", s.get_active()))
        self._row(rows, "Notify when a NAS goes offline or returns",
                  "For automatic scans, or while the window is in the background.", notify)

        self.subnets = Gtk.Entry(text=settings.get("subnets", ""), width_chars=18,
                                 placeholder_text="10.1.2.0/24, 10.1.3.0/24")
        self.subnets.connect("changed", self._subnets_changed)
        self._row(rows, "Also scan these subnets",
                  "Unicast queries to every host; reaches NAS behind a router. Up to a /22.",
                  self.subnets)

        ssh = Gtk.Entry(text=settings.get("ssh_user", "admin"), width_chars=18)
        ssh.connect("changed", lambda e: self._set("ssh_user", e.get_text().strip() or "admin"))
        self._row(rows, "SSH user", "For “Open SSH in a terminal”.", ssh)

        account = Gtk.Entry(text=settings.get("adm_account", "admin"), width_chars=18)
        account.connect("changed", lambda e: self._set("adm_account", e.get_text().strip() or "admin"))
        self._row(rows, "ADM account", "For Restart and Shut down.", account)

        self.forget = Gtk.Button(label="Forget", valign=Gtk.Align.CENTER)
        self.forget.set_sensitive(bool(parent.adm_password or keyring_lookup()))
        self.forget.connect("clicked", self._forget_password)
        where = ("Asked for once, then kept in your keyring if you tick it." if Secret
                 else "Asked for once per session; libsecret is not installed, so never stored.")
        self._row(rows, "ADM password", where, self.forget)

        self.set_child(Gtk.ScrolledWindow(child=rows, propagate_natural_height=True,
                                          hscrollbar_policy=Gtk.PolicyType.NEVER))
        keys = Gtk.EventControllerKey(propagation_phase=Gtk.PropagationPhase.CAPTURE)
        keys.connect("key-pressed", lambda _c, k, *_: (k == Gdk.KEY_Escape and (self.close() or True)))
        self.add_controller(keys)

    @staticmethod
    def _row(rows, title, subtitle, widget):
        row = Gtk.Box(spacing=12, margin_top=8, margin_bottom=8, margin_start=12, margin_end=12)
        text = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=2, hexpand=True,
                       valign=Gtk.Align.CENTER)
        text.append(Gtk.Label(label=title, xalign=0, wrap=True))
        sub = Gtk.Label(label=subtitle, xalign=0, wrap=True)
        sub.add_css_class("dim-label")
        sub.add_css_class("caption")
        text.append(sub)
        row.append(text)
        widget.set_valign(Gtk.Align.CENTER)
        row.append(widget)
        rows.append(row)

    def _set(self, key, value):
        self.parent_window.settings[key] = value
        save_settings(self.parent_window.settings)

    def _subnets_changed(self, entry):
        good, bad = parse_subnets(entry.get_text())
        if bad:
            entry.add_css_class("error")
            entry.set_tooltip_text("Not a usable IPv4 subnet: " + ", ".join(bad))
        else:
            entry.remove_css_class("error")
            entry.set_tooltip_text(None)
            self._set("subnets", ", ".join(good))

    def _forget_password(self, *_):
        self.parent_window.adm_password = None
        keyring_clear()
        self.forget.set_sensitive(False)
        self.parent_window.set_status("The ADM password is forgotten; the next restart asks again.")


class EntryDialog(Gtk.Window):
    """A small modal dialog over the main window with labelled entries and
    Cancel / action buttons; Enter confirms, Escape cancels."""

    def __init__(self, parent, title, subtitle, verb):
        super().__init__(title=title, application=parent.get_application(), transient_for=parent,
                         modal=True, destroy_with_parent=True, default_width=460, resizable=False)
        self.set_icon_name(APP_ID)
        header = Gtk.HeaderBar(show_title_buttons=False)
        self.set_titlebar(header)
        titles = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, valign=Gtk.Align.CENTER)
        head = Gtk.Label(label=title, ellipsize=Pango.EllipsizeMode.END)
        head.add_css_class("title")
        titles.append(head)
        if subtitle:
            sub = Gtk.Label(label=subtitle, ellipsize=Pango.EllipsizeMode.END)
            sub.add_css_class("subtitle")
            titles.append(sub)
        header.set_title_widget(titles)
        cancel = Gtk.Button(label="Cancel")
        cancel.connect("clicked", lambda *_: self.close())
        header.pack_start(cancel)
        self.ok = Gtk.Button(label=verb)
        self.ok.add_css_class("accent")
        self.ok.connect("clicked", lambda *_: self._confirm())
        header.pack_end(self.ok)
        self.grid = Gtk.Grid(row_spacing=10, column_spacing=12, margin_top=18,
                             margin_bottom=18, margin_start=18, margin_end=18)
        self.set_child(self.grid)
        self.entries = []
        keys = Gtk.EventControllerKey(propagation_phase=Gtk.PropagationPhase.CAPTURE)
        keys.connect("key-pressed", lambda _c, k, *_: (k == Gdk.KEY_Escape and (self.close() or True)))
        self.add_controller(keys)

    def add_entry(self, label, entry):
        caption = Gtk.Label(label=label, xalign=1)
        caption.add_css_class("dim-label")
        entry.set_hexpand(True)
        entry.connect("activate", lambda *_: self._confirm())
        self.grid.attach(caption, 0, len(self.entries), 1, 1)
        self.grid.attach(entry, 1, len(self.entries), 1, 1)
        self.entries.append(entry)
        return entry

    def add_widget(self, widget):
        self.grid.attach(widget, 1, len(self.entries), 1, 1)
        self.entries.append(widget)

    def present(self):
        super().present()
        if self.entries:
            self.entries[0].grab_focus()

    def _confirm(self):
        raise NotImplementedError


class TagsDialog(EntryDialog):
    def __init__(self, parent, dev, save):
        super().__init__(parent, "Tags and note", dev.name, "Save")
        self.dev, self.save = dev, save
        self.tags = self.add_entry("Tags", Gtk.Entry(text=", ".join(dev.tag_list()),
                                                     placeholder_text="office, rack 2, backup"))
        self.note = self.add_entry("Note", Gtk.Entry(text=dev.note,
                                                     placeholder_text="anything worth remembering"))

    def _confirm(self):
        tags = [t.strip() for t in self.tags.get_text().split(",") if t.strip()]
        self.save(self.dev, tags, self.note.get_text().strip())
        self.close()


class CredentialsDialog(EntryDialog):
    def __init__(self, parent, dev, account, done):
        super().__init__(parent, "Sign in to ADM", f"{dev.name} · {dev.ip}", "Sign in")
        self.done = done
        self.account = self.add_entry("Account", Gtk.Entry(text=account))
        self.password = self.add_entry("Password", Gtk.PasswordEntry(show_peek_icon=True))
        self.remember = Gtk.CheckButton(label="Remember the password in the keyring",
                                        active=Secret is not None, visible=Secret is not None)
        self.add_widget(self.remember)

    def present(self):
        super().present()
        self.password.grab_focus()

    def _confirm(self):
        account, password = self.account.get_text().strip(), self.password.get_text()
        if not account or not password:
            return
        remember = self.remember.get_active()
        self.close()
        self.done(account, password, remember)


class PortsWindow(Gtk.Window):
    """Which services of a NAS answer: the result of `acc ports`, one row
    per well-known port with a dot (accent: open, grey: closed, amber:
    filtered). A single list, so it works at any width."""

    def __init__(self, parent, dev):
        super().__init__(title=f"Services of {dev.name}", application=parent.get_application(),
                         transient_for=parent, destroy_with_parent=True,
                         default_width=520, default_height=640)
        self.set_size_request(MIN_WIDTH, 300)
        self.set_icon_name(APP_ID)
        self.parent_window, self.ip = parent, dev.ip
        self.checking = False

        header = Gtk.HeaderBar()
        self.set_titlebar(header)
        titles = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, valign=Gtk.Align.CENTER)
        title = Gtk.Label(label=f"Services of {dev.name}", ellipsize=Pango.EllipsizeMode.END)
        title.add_css_class("title")
        address = Gtk.Label(label=dev.ip)
        address.add_css_class("subtitle")
        titles.append(title)
        titles.append(address)
        header.set_title_widget(titles)
        again = Gtk.Button(icon_name="view-refresh-symbolic", tooltip_text="Check again (F5)")
        again.connect("clicked", lambda *_: self.check())
        header.pack_start(again)
        self.spinner = Gtk.Spinner()
        header.pack_start(self.spinner)

        self.rows = Gtk.ListBox(selection_mode=Gtk.SelectionMode.NONE,
                                margin_top=12, margin_bottom=12, margin_start=12, margin_end=12)
        self.rows.add_css_class("boxed-list" if Adw else "rich-list")
        self.summary = Gtk.Label(xalign=0, wrap=True, wrap_mode=Pango.WrapMode.WORD_CHAR)
        self.summary.add_css_class("dim-label")
        self.summary.add_css_class("statusbar")
        box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL)
        box.append(accent_stripe())
        box.append(Gtk.ScrolledWindow(child=self.rows, vexpand=True,
                                      hscrollbar_policy=Gtk.PolicyType.NEVER))
        box.append(Gtk.Separator())
        box.append(self.summary)
        self.set_child(box)

        keys = Gtk.EventControllerKey(propagation_phase=Gtk.PropagationPhase.CAPTURE)
        keys.connect("key-pressed", self._key)
        self.add_controller(keys)
        self.check()

    def check(self):
        if self.checking:
            return
        self.checking = True
        self.spinner.start()
        self.summary.set_text(f"Checking the well-known ports of {self.ip}… "
                              "(a connection attempt per port, nothing is sent)")
        self.parent_window._run_acc(["ports", self.ip, "--json"], self._done)

    def _done(self, out, err):
        self.checking = False
        self.spinner.stop()
        while (row := self.rows.get_row_at_index(0)) is not None:
            self.rows.remove(row)
        try:
            results = json.loads(out) if out else None
        except ValueError:
            results = None
        if not isinstance(results, list) or not results:
            self.summary.set_text(f"Could not check {self.ip}: {err or 'acc gave no output'}")
            return
        open_count = 0
        for r in results:
            state = str(r.get("state") or "")
            open_count += state == "open"
            row = Gtk.Box(spacing=10, margin_top=6, margin_bottom=6, margin_start=12, margin_end=12)
            dot = Gtk.Box(valign=Gtk.Align.CENTER)
            dot.add_css_class("port-dot")
            dot.add_css_class(state)
            port = Gtk.Label(label=str(r.get("port") or ""), xalign=1, width_chars=5)
            port.add_css_class("tabular")
            service = Gtk.Label(label=str(r.get("service") or ""), xalign=0, hexpand=True,
                                ellipsize=Pango.EllipsizeMode.END)
            label = Gtk.Label(label=state)
            label.add_css_class("dim-label")
            for w in (dot, port, service, label):
                row.append(w)
            self.rows.append(row)
        self.summary.set_text(f"{open_count} of {len(results)} services answer. Closed: nothing "
                              "listens on that port. Filtered: no answer, a firewall drops it.")

    def _key(self, _controller, keyval, _keycode, _state):
        if keyval == Gdk.KEY_Escape:
            self.close()
        elif keyval == Gdk.KEY_F5:
            self.check()
        else:
            return False
        return True


class WebWindow(Gtk.Window):
    """A page of asustor.com inside the app: back, forward and reload
    buttons, the address under the title, a button that hands the page to
    the browser, and a kiwi progress stripe. Works down to MIN_WIDTH: the
    buttons are icons and the title shortens."""

    def __init__(self, parent, url, title):
        super().__init__(title=title, application=parent.get_application(),
                         transient_for=parent, destroy_with_parent=True,
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
        self.progress.add_css_class("accent")
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
        self.set_accels_for_action("win.ports", ["<Control><Shift>p"])
        self.set_accels_for_action("win.health", ["<Control>d"])
        self.set_accels_for_action("win.ssh", ["<Control>t"])
        self.set_accels_for_action("win.tags", ["F2"])
        self.set_accels_for_action("win.export", ["<Control>e"])
        self.set_accels_for_action("win.prefs", ["<Control>comma"])

    def do_activate(self):
        win = self.get_active_window() or AccWindow(self)
        win.present()


if __name__ == "__main__":
    if "--version" in sys.argv[1:]:
        print(f"acc-gui {GUI_VERSION}")
        sys.exit(0)
    sys.exit(AccApp().run(sys.argv))
