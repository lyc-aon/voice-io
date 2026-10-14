// macOS desktop integration: Accessibility focus identity, pasteboard
// paste with restore, Carbon global hotkeys, sounds, permissions and the
// login item. Pasteboard, TIS and Carbon calls run on the main queue when
// the AppKit loop is running.
#import <AVFoundation/AVFoundation.h>
#import <AppKit/AppKit.h>
#import <ApplicationServices/ApplicationServices.h>
#import <Carbon/Carbon.h>
#import <ServiceManagement/ServiceManagement.h>
#include <string.h>
#include <unistd.h>
#include <wctype.h>

#include "_cgo_export.h"
#include "darwin.h"

static volatile int mainLoopRunning = 0;

void vio_set_main_loop_running(int running) { mainLoopRunning = running; }

static void onMain(dispatch_block_t block) {
  if ([NSThread isMainThread] || !mainLoopRunning) {
    block();
  } else {
    dispatch_sync(dispatch_get_main_queue(), block);
  }
}

// ---- focus ---------------------------------------------------------------

char *vio_focus_identity(int *reason) {
  *reason = 0;
  if (!AXIsProcessTrusted()) {
    *reason = 1;
    return NULL;
  }
  AXUIElementRef sys = AXUIElementCreateSystemWide();
  AXUIElementSetMessagingTimeout(sys, 0.5f);
  AXUIElementRef app = NULL;
  AXError err = AXUIElementCopyAttributeValue(sys, kAXFocusedApplicationAttribute, (CFTypeRef *)&app);
  CFRelease(sys);
  if (err != kAXErrorSuccess || app == NULL) {
    *reason = 2;
    return NULL;
  }
  AXUIElementSetMessagingTimeout(app, 0.5f);
  pid_t pid = 0;
  AXUIElementGetPid(app, &pid);
  CFTypeRef target = NULL;
  err = AXUIElementCopyAttributeValue(app, kAXFocusedWindowAttribute, &target);
  if (err != kAXErrorSuccess || target == NULL) {
    // Panels such as launchers expose a focused element but no window.
    err = AXUIElementCopyAttributeValue(app, kAXFocusedUIElementAttribute, &target);
  }
  CFRelease(app);
  if (err != kAXErrorSuccess || target == NULL) {
    *reason = 2;
    return NULL;
  }
  // Equal AX elements hash equally (CF contract), so the hash is a stable
  // identity for the window across probes.
  CFHashCode hash = CFHash(target);
  CFRelease(target);
  NSString *bundle =
      [NSRunningApplication runningApplicationWithProcessIdentifier:pid].bundleIdentifier ?: @"?";
  NSString *identity = [NSString stringWithFormat:@"%@|%d|%lx", bundle, pid, (unsigned long)hash];
  return strdup(identity.UTF8String);
}

static BOOL sameFocus(const char *expected) {
  if (expected == NULL) {
    return YES;
  }
  int reason = 0;
  char *now = vio_focus_identity(&reason);
  BOOL same = now != NULL && strcmp(now, expected) == 0;
  free(now);
  return same;
}

// ---- keys ----------------------------------------------------------------

// Finds the virtual key producing ch in the current layout (main thread).
static BOOL keycodeForChar(UniChar ch, CGKeyCode *out) {
  TISInputSourceRef source = TISCopyCurrentKeyboardLayoutInputSource();
  CFDataRef data = source ? TISGetInputSourceProperty(source, kTISPropertyUnicodeKeyLayoutData) : NULL;
  if (data == NULL) {
    if (source) CFRelease(source);
    source = TISCopyCurrentASCIICapableKeyboardLayoutInputSource();
    data = source ? TISGetInputSourceProperty(source, kTISPropertyUnicodeKeyLayoutData) : NULL;
  }
  BOOL found = NO;
  if (data != NULL) {
    const UCKeyboardLayout *layout = (const UCKeyboardLayout *)CFDataGetBytePtr(data);
    for (CGKeyCode code = 0; code < 128 && !found; code++) {
      UInt32 dead = 0;
      UniChar chars[4];
      UniCharCount len = 0;
      OSStatus st = UCKeyTranslate(layout, code, kUCKeyActionDisplay, 0, LMGetKbdType(),
                                   kUCKeyTranslateNoDeadKeysBit, &dead, 4, &len, chars);
      if (st == noErr && len == 1 && towlower(chars[0]) == ch) {
        *out = code;
        found = YES;
      }
    }
  }
  if (source) CFRelease(source);
  return found;
}

static void postKey(CGKeyCode code, CGEventFlags flags) {
  CGEventSourceRef src = CGEventSourceCreate(kCGEventSourceStateHIDSystemState);
  CGEventRef down = CGEventCreateKeyboardEvent(src, code, true);
  CGEventRef up = CGEventCreateKeyboardEvent(src, code, false);
  CGEventSetFlags(down, flags);
  CGEventSetFlags(up, flags);
  CGEventPost(kCGHIDEventTap, down);
  CGEventPost(kCGHIDEventTap, up);
  CFRelease(down);
  CFRelease(up);
  if (src) CFRelease(src);
}

int vio_key(int chord) {
  if (!AXIsProcessTrusted()) {
    return 3;
  }
  switch (chord) {
    case 0:
      postKey(kVK_Return, 0);
      break;
    case 1:
      postKey(kVK_Return, kCGEventFlagMaskControl);
      break;
    default: {
      __block CGKeyCode v = kVK_ANSI_V;
      onMain(^{
        keycodeForChar('v', &v);
      });
      postKey(v, kCGEventFlagMaskCommand);
    }
  }
  return 0;
}

// ---- clipboard -----------------------------------------------------------

int vio_copy(const char *text) {
  NSString *str = [NSString stringWithUTF8String:text];
  __block int rc = 0;
  onMain(^{
    NSPasteboard *board = [NSPasteboard generalPasteboard];
    [board clearContents];
    if (![board setString:str forType:NSPasteboardTypeString]) {
      rc = 2;
    }
  });
  return rc;
}

int vio_paste(const char *text, const char *expected) {
  if (!AXIsProcessTrusted()) {
    return 3;
  }
  NSString *str = [NSString stringWithUTF8String:text];
  __block NSArray<NSDictionary<NSPasteboardType, NSData *> *> *saved = nil;
  __block NSInteger ours = 0;
  __block int rc = 0;
  onMain(^{
    NSPasteboard *board = [NSPasteboard generalPasteboard];
    NSMutableArray *items = [NSMutableArray array];
    for (NSPasteboardItem *item in board.pasteboardItems) {
      NSMutableDictionary *types = [NSMutableDictionary dictionary];
      for (NSPasteboardType type in item.types) {
        NSData *data = [item dataForType:type];
        if (data == nil) {
          rc = 4;  // never silently lose clipboard content
          return;
        }
        types[type] = data;
      }
      [items addObject:types];
    }
    saved = items;
    [board clearContents];
    if (![board setString:str forType:NSPasteboardTypeString]) {
      rc = 2;
      return;
    }
    ours = board.changeCount;
  });
  if (rc != 0) {
    return rc;
  }
  // Re-probe immediately before the chord: the target must not have moved
  // while the transcript and clipboard were prepared.
  if (!sameFocus(expected)) {
    return 1;
  }
  usleep(60000);
  if (!sameFocus(expected)) {
    return 1;
  }
  vio_key(2);
  usleep(120000);
  onMain(^{
    NSPasteboard *board = [NSPasteboard generalPasteboard];
    // Never overwrite something the user copied meanwhile.
    if (board.changeCount != ours) {
      return;
    }
    [board clearContents];
    NSMutableArray *items = [NSMutableArray array];
    for (NSDictionary<NSPasteboardType, NSData *> *types in saved) {
      NSPasteboardItem *item = [[NSPasteboardItem alloc] init];
      for (NSPasteboardType type in types) {
        [item setData:types[type] forType:type];
      }
      [items addObject:item];
    }
    if (items.count > 0) {
      [board writeObjects:items];
    }
  });
  return 0;
}

// ---- hotkeys -------------------------------------------------------------

static EventHandlerRef hotkeyHandlerRef = NULL;
static EventHotKeyRef hotkeyRefs[16];

static OSStatus onHotkey(EventHandlerCallRef next, EventRef event, void *userData) {
  EventHotKeyID hk;
  if (GetEventParameter(event, kEventParamDirectObject, typeEventHotKeyID, NULL, sizeof(hk), NULL,
                        &hk) != noErr ||
      hk.signature != 'vIOh') {
    return eventNotHandledErr;
  }
  vioHotkeyEvent((int)hk.id, GetEventKind(event) == kEventHotKeyPressed ? 1 : 0);
  return noErr;
}

static BOOL namedKeycode(const char *key, UInt32 *code) {
  static const struct {
    const char *name;
    UInt32 code;
  } names[] = {
      {"space", kVK_Space},       {"enter", kVK_Return},        {"tab", kVK_Tab},
      {"escape", kVK_Escape},     {"backspace", kVK_Delete},    {"delete", kVK_ForwardDelete},
      {"up", kVK_UpArrow},        {"down", kVK_DownArrow},      {"left", kVK_LeftArrow},
      {"right", kVK_RightArrow},  {"home", kVK_Home},           {"end", kVK_End},
      {"pageup", kVK_PageUp},     {"pagedown", kVK_PageDown},   {"insert", kVK_Help},
      {"f1", kVK_F1},             {"f2", kVK_F2},               {"f3", kVK_F3},
      {"f4", kVK_F4},             {"f5", kVK_F5},               {"f6", kVK_F6},
      {"f7", kVK_F7},             {"f8", kVK_F8},               {"f9", kVK_F9},
      {"f10", kVK_F10},           {"f11", kVK_F11},             {"f12", kVK_F12},
      {"f13", kVK_F13},           {"f14", kVK_F14},             {"f15", kVK_F15},
      {"f16", kVK_F16},           {"f17", kVK_F17},             {"f18", kVK_F18},
      {"f19", kVK_F19},           {"f20", kVK_F20},
  };
  for (size_t i = 0; i < sizeof(names) / sizeof(names[0]); i++) {
    if (strcmp(names[i].name, key) == 0) {
      *code = names[i].code;
      return YES;
    }
  }
  return NO;
}

int vio_hotkey_register(int id, const char *key, int mods) {
  if (id < 0 || id >= 16) {
    return -1;
  }
  __block int rc = 0;
  onMain(^{
    if (hotkeyHandlerRef == NULL) {
      EventTypeSpec specs[2] = {{kEventClassKeyboard, kEventHotKeyPressed},
                                {kEventClassKeyboard, kEventHotKeyReleased}};
      InstallApplicationEventHandler(&onHotkey, 2, specs, NULL, &hotkeyHandlerRef);
    }
    UInt32 code = 0;
    if (!namedKeycode(key, &code)) {
      CGKeyCode kc = 0;
      if (strlen(key) != 1 || !keycodeForChar((UniChar)key[0], &kc)) {
        rc = -1;
        return;
      }
      code = kc;
    }
    UInt32 carbonMods = 0;
    if (mods & 1) carbonMods |= controlKey;
    if (mods & 2) carbonMods |= optionKey;
    if (mods & 4) carbonMods |= shiftKey;
    if (mods & 8) carbonMods |= cmdKey;
    if (hotkeyRefs[id] != NULL) {
      UnregisterEventHotKey(hotkeyRefs[id]);
      hotkeyRefs[id] = NULL;
    }
    EventHotKeyID hid = {'vIOh', (UInt32)id};
    EventHotKeyRef ref = NULL;
    OSStatus st = RegisterEventHotKey(code, carbonMods, hid, GetApplicationEventTarget(), 0, &ref);
    if (st != noErr) {
      rc = (int)st;
      return;
    }
    hotkeyRefs[id] = ref;
  });
  return rc;
}

void vio_hotkeys_clear(void) {
  onMain(^{
    for (int i = 0; i < 16; i++) {
      if (hotkeyRefs[i] != NULL) {
        UnregisterEventHotKey(hotkeyRefs[i]);
        hotkeyRefs[i] = NULL;
      }
    }
  });
}

// ---- fn key --------------------------------------------------------------

static id fnGlobalMonitor;
static id fnLocalMonitor;
static BOOL fnHeld = NO;

static void onFnEvent(NSEvent *event) {
  if (event.type == NSEventTypeFlagsChanged && (event.keyCode == kVK_Function || event.keyCode == 179)) {
    NSEventModifierFlags flags = event.modifierFlags & NSEventModifierFlagDeviceIndependentFlagsMask;
    BOOL down = (flags & NSEventModifierFlagFunction) != 0;
    BOOL others = (flags & (NSEventModifierFlagCommand | NSEventModifierFlagControl | NSEventModifierFlagOption |
                            NSEventModifierFlagShift)) != 0;
    if (down && !fnHeld && !others) {
      fnHeld = YES;
      vioFnEvent(1);
    } else if (!down && fnHeld) {
      fnHeld = NO;
      vioFnEvent(0);
    }
  } else if (event.type == NSEventTypeKeyDown && fnHeld) {
    vioFnEvent(2);  // fn+key is a keyboard shortcut, not dictation
  }
}

void vio_fn_monitor_start(void) {
  onMain(^{
    if (fnGlobalMonitor != nil) {
      return;
    }
    NSEventMask mask = NSEventMaskFlagsChanged | NSEventMaskKeyDown;
    fnGlobalMonitor = [NSEvent addGlobalMonitorForEventsMatchingMask:mask
                                                             handler:^(NSEvent *event) {
                                                               onFnEvent(event);
                                                             }];
    fnLocalMonitor = [NSEvent addLocalMonitorForEventsMatchingMask:mask
                                                           handler:^NSEvent *(NSEvent *event) {
                                                             onFnEvent(event);
                                                             return event;
                                                           }];
  });
}

void vio_fn_monitor_stop(void) {
  onMain(^{
    if (fnGlobalMonitor != nil) {
      [NSEvent removeMonitor:fnGlobalMonitor];
      fnGlobalMonitor = nil;
    }
    if (fnLocalMonitor != nil) {
      [NSEvent removeMonitor:fnLocalMonitor];
      fnLocalMonitor = nil;
    }
    fnHeld = NO;
  });
}

int vio_fn_usage_type(void) {
  CFPropertyListRef value = CFPreferencesCopyAppValue(CFSTR("AppleFnUsageType"), CFSTR("com.apple.HIToolbox"));
  if (value == NULL) {
    return -1;
  }
  int result = -1;
  if (CFGetTypeID(value) == CFNumberGetTypeID()) {
    CFNumberGetValue((CFNumberRef)value, kCFNumberIntType, &result);
  }
  CFRelease(value);
  return result;
}

// ---- sounds --------------------------------------------------------------

static NSMutableDictionary<NSString *, NSSound *> *sounds;

void vio_sound_register(const char *name, const void *wav, int len) {
  NSString *key = [NSString stringWithUTF8String:name];
  NSData *data = [NSData dataWithBytes:wav length:(NSUInteger)len];
  onMain(^{
    if (sounds == nil) {
      sounds = [NSMutableDictionary dictionary];
    }
    NSSound *sound = [[NSSound alloc] initWithData:data];
    if (sound != nil) {
      sounds[key] = sound;
    }
  });
}

void vio_sound_play(const char *name) {
  NSString *key = [NSString stringWithUTF8String:name];
  dispatch_block_t play = ^{
    NSSound *sound = sounds[key];
    if (sound != nil) {
      [sound stop];
      [sound play];
    }
  };
  if (mainLoopRunning && ![NSThread isMainThread]) {
    dispatch_async(dispatch_get_main_queue(), play);
  } else {
    play();
  }
}

// ---- permissions ---------------------------------------------------------

int vio_mic_status(void) {
  switch ([AVCaptureDevice authorizationStatusForMediaType:AVMediaTypeAudio]) {
    case AVAuthorizationStatusNotDetermined:
      return 0;
    case AVAuthorizationStatusRestricted:
      return 1;
    case AVAuthorizationStatusDenied:
      return 2;
    case AVAuthorizationStatusAuthorized:
      return 3;
  }
  return 0;
}

void vio_mic_request(void) {
  [AVCaptureDevice requestAccessForMediaType:AVMediaTypeAudio
                           completionHandler:^(BOOL granted){
                           }];
}

int vio_ax_trusted(int prompt) {
  if (prompt) {
    NSDictionary *opts = @{(__bridge NSString *)kAXTrustedCheckOptionPrompt : @YES};
    return AXIsProcessTrustedWithOptions((__bridge CFDictionaryRef)opts) ? 1 : 0;
  }
  return AXIsProcessTrusted() ? 1 : 0;
}

void vio_open_url(const char *url) {
  NSURL *u = [NSURL URLWithString:[NSString stringWithUTF8String:url]];
  if (u != nil) {
    [[NSWorkspace sharedWorkspace] openURL:u];
  }
}

void vio_reveal(const char *path) {
  NSURL *u = [NSURL fileURLWithPath:[NSString stringWithUTF8String:path]];
  [[NSWorkspace sharedWorkspace] activateFileViewerSelectingURLs:@[ u ]];
}

void vio_open_path(const char *path) {
  [[NSWorkspace sharedWorkspace] openURL:[NSURL fileURLWithPath:[NSString stringWithUTF8String:path]]];
}

// ---- login item ----------------------------------------------------------

static BOOL inAppBundle(void) {
  return [[NSBundle mainBundle].bundlePath hasSuffix:@".app"] && [NSBundle mainBundle].bundleIdentifier != nil;
}

int vio_login_item_status(void) {
  if (@available(macOS 13.0, *)) {
    if (!inAppBundle()) {
      return -1;
    }
    switch ([SMAppService mainAppService].status) {
      case SMAppServiceStatusEnabled:
        return 1;
      case SMAppServiceStatusRequiresApproval:
        return 2;
      default:
        return 0;
    }
  }
  return -1;
}

char *vio_login_item_set(int enabled) {
  if (@available(macOS 13.0, *)) {
    if (!inAppBundle()) {
      return strdup("Start at login needs the installed Voice IO app");
    }
    NSError *error = nil;
    BOOL ok = enabled ? [[SMAppService mainAppService] registerAndReturnError:&error]
                      : [[SMAppService mainAppService] unregisterAndReturnError:&error];
    if (!ok) {
      return strdup((error.localizedDescription ?: @"login item change failed").UTF8String);
    }
    return NULL;
  }
  return strdup("Start at login needs macOS 13 or later");
}
