import Gio from 'gi://Gio';
import GLib from 'gi://GLib';
import * as Main from 'resource:///org/gnome/shell/ui/main.js';
import {Extension} from 'resource:///org/gnome/shell/extensions/extension.js';

const BUS_NAME = 'org.voiceio.Focus1';
const OBJECT_PATH = '/org/voiceio/Focus1';
const INTERFACE = `<node>
  <interface name="org.voiceio.Focus1">
    <method name="GetFocusedWindow">
      <arg type="s" direction="out" name="app"/>
      <arg type="s" direction="out" name="title"/>
      <arg type="s" direction="out" name="identity"/>
    </method>
  </interface>
</node>`;

export default class FocusExtension extends Extension {
    enable() {
        // A reload invalidates all previous recording-time identities.
        this._instance = GLib.uuid_string_random();
        this._dbus = Gio.DBusExportedObject.wrapJSObject(INTERFACE, this);
        this._owner = Gio.bus_own_name(
            Gio.BusType.SESSION, BUS_NAME, Gio.BusNameOwnerFlags.NONE,
            null, connection => this._dbus?.export(connection, OBJECT_PATH), null);
    }

    disable() {
        this._dbus?.unexport();
        this._dbus = null;
        if (this._owner) {
            Gio.bus_unown_name(this._owner);
            this._owner = 0;
        }
        this._instance = null;
    }

    GetFocusedWindow() {
        // GNOME may retain focus_window while an overview or modal Shell UI
        // intercepts keys. Never report the old client in that case.
        if (Main.sessionMode.isLocked || Main.overview.visible || Main.modalCount > 0)
            return ['', '', ''];
        const window = global.display.focus_window;
        if (!window || window.is_override_redirect())
            return ['', '', ''];
        const windowApp = window.get_wm_class() || window.get_gtk_application_id();
        // Tern's Wayland app ID matches its desktop file; preserve the
        // terminal paste chord selected by the daemon's canonical app name.
        const app = windowApp === 'so.stencil.tern' ? 'tern' : windowApp;
        const sequence = window.get_stable_sequence();
        if (!app || !sequence || !this._instance)
            return ['', '', ''];
        return [app, window.get_title() || '', `${this._instance}:${sequence}`];
    }
}
