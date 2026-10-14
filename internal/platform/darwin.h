// macOS desktop integration for voice-io (implemented in darwin.m).
#ifndef VOICE_IO_PLATFORM_DARWIN_H
#define VOICE_IO_PLATFORM_DARWIN_H

#include <stdint.h>

// Focus identity "bundle|pid|element-hash", malloc'd; NULL when unknown.
// *reason: 0 ok, 1 not trusted for Accessibility, 2 no focused app/window.
char *vio_focus_identity(int *reason);

// Clipboard paste with focus re-verification. expected may be NULL.
// Returns 0 ok, 1 focus changed (text left on clipboard), 2 clipboard
// rejected, 3 not trusted, 4 cannot snapshot clipboard.
int vio_paste(const char *text, const char *expected);
int vio_copy(const char *text);
// chord: 0 enter, 1 ctrl+enter, 2 cmd+v. Returns 0 ok, 3 not trusted.
int vio_key(int chord);

// Carbon global hotkeys; events arrive in vioHotkeyEvent(id, pressed).
// key is a portable key name ("space", "t", "f5"); mods use bit 1 ctrl,
// 2 alt, 4 shift, 8 cmd. Returns 0 ok, -1 unknown key, else OSStatus.
int vio_hotkey_register(int id, const char *key, int mods);
void vio_hotkeys_clear(void);

// Fn (🌐) key monitor; events arrive in vioFnEvent(kind): 1 pressed alone,
// 0 released, 2 another key pressed while held. Needs Accessibility.
void vio_fn_monitor_start(void);
void vio_fn_monitor_stop(void);
// System "Press 🌐 key to" setting: 0 do nothing, 1 input source, 2 emoji,
// 3 dictation, -1 unset.
int vio_fn_usage_type(void);

// Sounds: register a WAV under name, then play it.
void vio_sound_register(const char *name, const void *wav, int len);
void vio_sound_play(const char *name);

// Permissions: 0 not determined, 1 restricted, 2 denied, 3 granted.
int vio_mic_status(void);
void vio_mic_request(void);
int vio_ax_trusted(int prompt);
void vio_open_url(const char *url);
void vio_reveal(const char *path);
void vio_open_path(const char *path);

// Login item (SMAppService): -1 unsupported, 0 off, 1 on, 2 needs approval.
int vio_login_item_status(void);
// Returns NULL on success, else a malloc'd message.
char *vio_login_item_set(int enabled);

// Main-loop flag: when set, AppKit work hops to the main queue.
void vio_set_main_loop_running(int running);

#endif
