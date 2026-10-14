// AppKit host: a non-activating overlay panel that never takes focus, the
// menu bar item, context menus and the WKWebView Settings window.
#import <AppKit/AppKit.h>
#import <WebKit/WebKit.h>

#include "_cgo_export.h"
#include "ui_darwin.h"

static const NSTimeInterval kFrame = 1.0 / 60.0;
static const NSTimeInterval kPointerPoll = 1.0 / 20.0;

@interface VIOPanel : NSPanel
@end
@implementation VIOPanel
- (BOOL)canBecomeKeyWindow {
  return NO;
}
- (BOOL)canBecomeMainWindow {
  return NO;
}
@end

@interface VIOOverlayView : NSView
@end

@interface VIODragStrip : NSView
@end
@implementation VIODragStrip
- (BOOL)mouseDownCanMoveWindow {
  return YES;
}
- (void)mouseDown:(NSEvent *)event {
  [self.window performWindowDragWithEvent:event];
}
@end

@interface VIOController
    : NSObject <NSApplicationDelegate, NSMenuDelegate, NSWindowDelegate, WKNavigationDelegate, WKUIDelegate>
@property(strong) VIOPanel *panel;
@property(strong) VIOOverlayView *view;
@property(strong) NSTimer *frameTimer;
@property(strong) NSTimer *pointerTimer;
@property(strong) NSStatusItem *statusItem;
@property(strong) NSMenu *statusMenu;
@property(strong) NSWindow *settingsWindow;
@property(strong) WKWebView *web;
@property(strong) NSScreen *activeScreen;
@property(copy) NSString *statusState;
@property NSSize canvas;
@property BOOL overlayEnabled;
@property BOOL showStatus;
@property BOOL wasActive;
@property NSPoint lastPointer;
@property BOOL pointerInside;
@property double screenHeight;
- (void)setup;
- (void)buildPanel;
- (void)afterPaint:(vio_frame_info)info;
- (void)requestFrame;
- (void)setStatus:(NSString *)state;
- (void)showMenu:(const char *)json at:(NSPoint)point;
- (void)openSettings:(NSString *)url token:(NSString *)token;
@end

static VIOController *controller;

@implementation VIOOverlayView
- (BOOL)isFlipped {
  return YES;
}
- (BOOL)isOpaque {
  return NO;
}
- (BOOL)acceptsFirstResponder {
  return NO;
}
- (BOOL)acceptsFirstMouse:(NSEvent *)event {
  return YES;
}
- (void)drawRect:(NSRect)dirty {
  CGContextRef ctx = NSGraphicsContext.currentContext.CGContext;
  CGContextClearRect(ctx, self.bounds);
  vio_frame_info info = {0};
  vioOverlayPaint(ctx, self.window.backingScaleFactor, &info);
  [controller afterPaint:info];
}
- (NSPoint)local:(NSEvent *)event {
  return [self convertPoint:event.locationInWindow fromView:nil];
}
// Screen y grows upward in AppKit; drags report it downward-positive.
- (void)mouseDown:(NSEvent *)event {
  NSPoint p = [self local:event];
  vioOverlayMouse(0, p.x, p.y, -NSEvent.mouseLocation.y, 0);
}
- (void)mouseDragged:(NSEvent *)event {
  NSPoint p = [self local:event];
  vioOverlayMouse(1, p.x, p.y, -NSEvent.mouseLocation.y, 0);
}
- (void)mouseUp:(NSEvent *)event {
  NSPoint p = [self local:event];
  vioOverlayMouse(2, p.x, p.y, -NSEvent.mouseLocation.y, 0);
}
- (void)rightMouseDown:(NSEvent *)event {
  NSPoint p = [self local:event];
  vioOverlayMouse(0, p.x, p.y, -NSEvent.mouseLocation.y, 1);
}
@end

static BOOL isDarkAppearance(void) {
  NSAppearanceName name = [NSApp.effectiveAppearance
      bestMatchFromAppearancesWithNames:@[ NSAppearanceNameAqua, NSAppearanceNameDarkAqua ]];
  return [name isEqualToString:NSAppearanceNameDarkAqua];
}

static NSImage *statusImage(NSString *state) {
  NSImage *image = [NSImage imageWithSize:NSMakeSize(18, 18)
                                  flipped:YES
                           drawingHandler:^BOOL(NSRect rect) {
                             [[NSColor blackColor] set];
                             const CGFloat heights[5] = {4, 9, 13, 9, 4};
                             const CGFloat busy[5] = {7, 7, 7, 7, 7};
                             BOOL processing = [state isEqualToString:@"processing"];
                             for (int i = 0; i < 5; i++) {
                               CGFloat h = processing ? busy[i] : heights[i];
                               NSRect bar = NSMakeRect(1.5 + i * 3.5, 9 - h / 2, 2, h);
                               [[NSBezierPath bezierPathWithRoundedRect:bar xRadius:1 yRadius:1] fill];
                             }
                             if ([state isEqualToString:@"off"]) {
                               NSBezierPath *slash = [NSBezierPath bezierPath];
                               [slash moveToPoint:NSMakePoint(2, 2)];
                               [slash lineToPoint:NSMakePoint(16, 16)];
                               slash.lineWidth = 1.6;
                               slash.lineCapStyle = NSLineCapStyleRound;
                               [slash stroke];
                             }
                             if ([state isEqualToString:@"setup"] || [state isEqualToString:@"recording"]) {
                               [[NSBezierPath bezierPathWithOvalInRect:NSMakeRect(13, 1, 5, 5)] fill];
                             }
                             return YES;
                           }];
  image.template = YES;
  return image;
}

static NSString *jsonString(NSString *s) {
  NSData *data = [NSJSONSerialization dataWithJSONObject:@[ s ] options:0 error:nil];
  NSString *wrapped = [[NSString alloc] initWithData:data encoding:NSUTF8StringEncoding];
  return [wrapped substringWithRange:NSMakeRange(1, wrapped.length - 2)];
}

@implementation VIOController

- (void)setup {
  NSApp.delegate = self;
  [self buildMainMenu];
  if (self.overlayEnabled) {
    [self buildPanel];
  }
  if (self.showStatus) {
    self.statusItem = [NSStatusBar.systemStatusBar statusItemWithLength:NSSquareStatusItemLength];
    self.statusItem.button.image = statusImage(@"idle");
    self.statusItem.button.toolTip = @"Voice IO";
    self.statusMenu = [[NSMenu alloc] initWithTitle:@"Voice IO"];
    self.statusMenu.delegate = self;
    self.statusMenu.autoenablesItems = NO;
    self.statusItem.menu = self.statusMenu;
    self.statusState = @"idle";
  }
  [NSApp addObserver:self forKeyPath:@"effectiveAppearance" options:NSKeyValueObservingOptionNew context:nil];
  [[NSNotificationCenter defaultCenter] addObserver:self
                                           selector:@selector(screensChanged:)
                                               name:NSApplicationDidChangeScreenParametersNotification
                                             object:nil];
  vioAppearance(isDarkAppearance() ? 1 : 0);
}

- (void)buildMainMenu {
  NSMenu *main = [[NSMenu alloc] init];
  NSMenuItem *appItem = [main addItemWithTitle:@"Voice IO" action:nil keyEquivalent:@""];
  NSMenu *appMenu = [[NSMenu alloc] initWithTitle:@"Voice IO"];
  [appMenu addItemWithTitle:@"Close Window" action:@selector(performClose:) keyEquivalent:@"w"];
  appItem.submenu = appMenu;
  NSMenuItem *editItem = [main addItemWithTitle:@"Edit" action:nil keyEquivalent:@""];
  NSMenu *edit = [[NSMenu alloc] initWithTitle:@"Edit"];
  [edit addItemWithTitle:@"Undo" action:@selector(undo:) keyEquivalent:@"z"];
  NSMenuItem *redo = [edit addItemWithTitle:@"Redo" action:@selector(redo:) keyEquivalent:@"z"];
  redo.keyEquivalentModifierMask = NSEventModifierFlagCommand | NSEventModifierFlagShift;
  [edit addItem:[NSMenuItem separatorItem]];
  [edit addItemWithTitle:@"Cut" action:@selector(cut:) keyEquivalent:@"x"];
  [edit addItemWithTitle:@"Copy" action:@selector(copy:) keyEquivalent:@"c"];
  [edit addItemWithTitle:@"Paste" action:@selector(paste:) keyEquivalent:@"v"];
  [edit addItemWithTitle:@"Select All" action:@selector(selectAll:) keyEquivalent:@"a"];
  editItem.submenu = edit;
  NSApp.mainMenu = main;
}

- (void)buildPanel {
  NSRect rect = NSMakeRect(0, 0, self.canvas.width, self.canvas.height);
  self.panel = [[VIOPanel alloc] initWithContentRect:rect
                                           styleMask:NSWindowStyleMaskBorderless | NSWindowStyleMaskNonactivatingPanel
                                             backing:NSBackingStoreBuffered
                                               defer:NO];
  self.panel.floatingPanel = YES;
  self.panel.hidesOnDeactivate = NO;
  self.panel.level = NSStatusWindowLevel;
  self.panel.collectionBehavior = NSWindowCollectionBehaviorCanJoinAllSpaces |
                                  NSWindowCollectionBehaviorFullScreenAuxiliary |
                                  NSWindowCollectionBehaviorStationary | NSWindowCollectionBehaviorIgnoresCycle;
  self.panel.opaque = NO;
  self.panel.backgroundColor = NSColor.clearColor;
  self.panel.hasShadow = NO;
  self.panel.ignoresMouseEvents = YES;
  self.panel.acceptsMouseMovedEvents = YES;
  self.view = [[VIOOverlayView alloc] initWithFrame:rect];
  self.panel.contentView = self.view;
  [self placeWithInfo:(vio_frame_info){0}];
  [self.panel orderFrontRegardless];
  self.pointerTimer = [NSTimer timerWithTimeInterval:kPointerPoll
                                              target:self
                                            selector:@selector(pollPointer)
                                            userInfo:nil
                                             repeats:YES];
  [[NSRunLoop mainRunLoop] addTimer:self.pointerTimer forMode:NSRunLoopCommonModes];
}

- (NSScreen *)screenWithMouse {
  NSPoint m = NSEvent.mouseLocation;
  for (NSScreen *s in NSScreen.screens) {
    if (NSPointInRect(m, s.frame)) return s;
  }
  return NSScreen.mainScreen;
}

- (void)placeWithInfo:(vio_frame_info)info {
  if (info.active && !self.wasActive) {
    self.activeScreen = [self screenWithMouse];
  }
  if (!info.active) {
    self.activeScreen = nil;
  }
  self.wasActive = info.active != 0;
  NSScreen *screen = self.activeScreen ?: NSScreen.screens.firstObject;
  if (screen == nil) return;
  NSRect vis = screen.visibleFrame;
  if (vis.size.height != self.screenHeight) {
    self.screenHeight = vis.size.height;
    vioScreenHeight(vis.size.height);
  }
  // Dock on the right edge, vertically centred plus the dragged offset.
  CGFloat x = NSMaxX(vis) - self.canvas.width;
  CGFloat y = NSMidY(vis) - self.canvas.height / 2 - info.offset_y;
  y = MAX(NSMinY(vis), MIN(y, NSMaxY(vis) - self.canvas.height));
  NSPoint origin = NSMakePoint(round(x), round(y));
  if (!NSEqualPoints(self.panel.frame.origin, origin)) {
    [self.panel setFrameOrigin:origin];
  }
}

- (void)afterPaint:(vio_frame_info)info {
  [self placeWithInfo:info];
  if (!info.animating && self.frameTimer != nil) {
    [self.frameTimer invalidate];
    self.frameTimer = nil;
  }
}

- (void)requestFrame {
  if (self.panel == nil) return;
  if (self.frameTimer == nil) {
    self.frameTimer = [NSTimer timerWithTimeInterval:kFrame
                                              target:self
                                            selector:@selector(tick)
                                            userInfo:nil
                                             repeats:YES];
    [[NSRunLoop mainRunLoop] addTimer:self.frameTimer forMode:NSRunLoopCommonModes];
  }
  self.view.needsDisplay = YES;
}

- (void)tick {
  self.view.needsDisplay = YES;
}

- (void)pollPointer {
  NSPoint m = NSEvent.mouseLocation;
  NSRect f = self.panel.frame;
  BOOL inside = NSPointInRect(m, f);
  if (NSEqualPoints(m, self.lastPointer) && inside == self.pointerInside) return;
  self.lastPointer = m;
  self.pointerInside = inside;
  double x = m.x - f.origin.x;
  double y = f.size.height - (m.y - f.origin.y);
  int hit = vioOverlayPointer(x, y, inside ? 1 : 0);
  BOOL ignore = hit == 0;
  if (self.panel.ignoresMouseEvents != ignore) {
    self.panel.ignoresMouseEvents = ignore;
  }
}

- (void)screensChanged:(NSNotification *)note {
  self.screenHeight = 0;
  [self requestFrame];
}

- (void)observeValueForKeyPath:(NSString *)keyPath
                      ofObject:(id)object
                        change:(NSDictionary *)change
                       context:(void *)context {
  vioAppearance(isDarkAppearance() ? 1 : 0);
  [self requestFrame];
}

// ---- status item -----------------------------------------------------------

- (void)setStatus:(NSString *)state {
  if (self.statusItem == nil || [state isEqualToString:self.statusState]) return;
  self.statusState = state;
  self.statusItem.button.image = statusImage(state);
  if ([state isEqualToString:@"recording"]) {
    self.statusItem.button.contentTintColor = NSColor.systemRedColor;
  } else if ([state isEqualToString:@"setup"]) {
    self.statusItem.button.contentTintColor = NSColor.systemOrangeColor;
  } else {
    self.statusItem.button.contentTintColor = nil;
  }
  self.statusItem.button.appearsDisabled = [state isEqualToString:@"off"];
}

- (void)menuNeedsUpdate:(NSMenu *)menu {
  if (menu != self.statusMenu) return;
  char *json = vioStatusMenuJSON();
  [self fill:menu json:json];
  free(json);
}

- (void)fill:(NSMenu *)menu json:(const char *)json {
  [menu removeAllItems];
  NSData *data = [NSData dataWithBytes:json length:strlen(json)];
  NSArray *items = [NSJSONSerialization JSONObjectWithData:data options:0 error:nil];
  if (![items isKindOfClass:NSArray.class]) return;
  [self add:items to:menu];
}

- (void)add:(NSArray *)items to:(NSMenu *)menu {
  menu.autoenablesItems = NO;
  for (NSDictionary *spec in items) {
    if ([spec[@"separator"] boolValue]) {
      [menu addItem:[NSMenuItem separatorItem]];
      continue;
    }
    NSString *title = spec[@"title"] ?: @"";
    if ([spec[@"header"] boolValue]) {
      NSMenuItem *header;
      if (@available(macOS 14.0, *)) {
        header = [NSMenuItem sectionHeaderWithTitle:title];
      } else {
        header = [[NSMenuItem alloc] initWithTitle:title action:nil keyEquivalent:@""];
        header.enabled = NO;
      }
      [menu addItem:header];
      continue;
    }
    NSString *key = spec[@"key"] ?: @"";
    NSMenuItem *item = [[NSMenuItem alloc] initWithTitle:title action:@selector(menuItemClicked:) keyEquivalent:key];
    item.target = self;
    item.representedObject = spec[@"action"];
    NSUInteger mods = [spec[@"mods"] unsignedIntegerValue];
    NSEventModifierFlags flags = 0;
    if (mods & 1) flags |= NSEventModifierFlagControl;
    if (mods & 2) flags |= NSEventModifierFlagOption;
    if (mods & 4) flags |= NSEventModifierFlagShift;
    if (mods & 8) flags |= NSEventModifierFlagCommand;
    item.keyEquivalentModifierMask = flags;
    item.state = [spec[@"checked"] boolValue] ? NSControlStateValueOn : NSControlStateValueOff;
    item.enabled = ![spec[@"disabled"] boolValue];
    if (spec[@"subtitle"] != nil) {
      if (@available(macOS 14.4, *)) {
        item.subtitle = spec[@"subtitle"];
      }
    }
    NSArray *children = spec[@"items"];
    if ([children isKindOfClass:NSArray.class] && children.count > 0) {
      NSMenu *sub = [[NSMenu alloc] initWithTitle:title];
      [self add:children to:sub];
      item.submenu = sub;
      item.action = nil;
    }
    [menu addItem:item];
  }
}

- (void)menuItemClicked:(NSMenuItem *)item {
  NSString *action = item.representedObject;
  if (![action isKindOfClass:NSString.class]) return;
  vioMenuAction((char *)action.UTF8String);
}

- (void)showMenu:(const char *)json at:(NSPoint)point {
  NSMenu *menu = [[NSMenu alloc] initWithTitle:@"Voice IO"];
  [self fill:menu json:json];
  [menu popUpMenuPositioningItem:nil atLocation:point inView:self.view];
}

// ---- settings window ---------------------------------------------------------

- (void)openSettings:(NSString *)url token:(NSString *)token {
  if (self.settingsWindow == nil) {
    WKWebViewConfiguration *cfg = [[WKWebViewConfiguration alloc] init];
    NSString *js = [NSString
        stringWithFormat:@"window.__VOICEIO__ = {token: %@, platform: 'darwin', native: true};", jsonString(token)];
    WKUserScript *script = [[WKUserScript alloc] initWithSource:js
                                                  injectionTime:WKUserScriptInjectionTimeAtDocumentStart
                                               forMainFrameOnly:YES];
    [cfg.userContentController addUserScript:script];
    NSRect rect = NSMakeRect(0, 0, 960, 680);
    self.web = [[WKWebView alloc] initWithFrame:rect configuration:cfg];
    self.web.navigationDelegate = self;
    self.web.UIDelegate = self;
    self.web.autoresizingMask = NSViewWidthSizable | NSViewHeightSizable;
    self.settingsWindow = [[NSWindow alloc]
        initWithContentRect:rect
                  styleMask:NSWindowStyleMaskTitled | NSWindowStyleMaskClosable | NSWindowStyleMaskMiniaturizable |
                            NSWindowStyleMaskResizable | NSWindowStyleMaskFullSizeContentView
                    backing:NSBackingStoreBuffered
                      defer:NO];
    self.settingsWindow.title = @"Voice IO Settings";
    self.settingsWindow.titlebarAppearsTransparent = YES;
    self.settingsWindow.titleVisibility = NSWindowTitleHidden;
    self.settingsWindow.minSize = NSMakeSize(780, 540);
    self.settingsWindow.releasedWhenClosed = NO;
    self.settingsWindow.delegate = self;
    NSView *container = [[NSView alloc] initWithFrame:rect];
    container.autoresizingMask = NSViewWidthSizable | NSViewHeightSizable;
    [container addSubview:self.web];
    // A transparent strip keeps the hidden title bar draggable over the page.
    VIODragStrip *strip = [[VIODragStrip alloc] initWithFrame:NSMakeRect(80, rect.size.height - 28, rect.size.width - 80, 28)];
    strip.autoresizingMask = NSViewWidthSizable | NSViewMinYMargin;
    [container addSubview:strip];
    self.settingsWindow.contentView = container;
    [self.settingsWindow center];
    [self.settingsWindow setFrameAutosaveName:@"VoiceIOSettings"];
    [self.web loadRequest:[NSURLRequest requestWithURL:[NSURL URLWithString:url]]];
  }
  if (@available(macOS 14.0, *)) {
    [NSApp activate];
  } else {
    [NSApp activateIgnoringOtherApps:YES];
  }
  [self.settingsWindow makeKeyAndOrderFront:nil];
}

- (void)windowWillClose:(NSNotification *)note {
  if (note.object == self.settingsWindow) {
    self.settingsWindow = nil;
    self.web = nil;
    vioSettingsClosed();
  }
}

- (void)webView:(WKWebView *)webView
    decidePolicyForNavigationAction:(WKNavigationAction *)action
                    decisionHandler:(void (^)(WKNavigationActionPolicy))decisionHandler {
  NSURL *url = action.request.URL;
  NSString *host = url.host;
  BOOL local = [host isEqualToString:@"127.0.0.1"] || [host isEqualToString:@"localhost"];
  if (!local && action.navigationType == WKNavigationTypeLinkActivated) {
    [[NSWorkspace sharedWorkspace] openURL:url];
    decisionHandler(WKNavigationActionPolicyCancel);
    return;
  }
  decisionHandler(local || [url.scheme isEqualToString:@"about"] ? WKNavigationActionPolicyAllow
                                                                  : WKNavigationActionPolicyCancel);
}

- (WKWebView *)webView:(WKWebView *)webView
    createWebViewWithConfiguration:(WKWebViewConfiguration *)configuration
               forNavigationAction:(WKNavigationAction *)action
                    windowFeatures:(WKWindowFeatures *)features {
  if (action.request.URL != nil) {
    [[NSWorkspace sharedWorkspace] openURL:action.request.URL];
  }
  return nil;
}

// ---- lifecycle -------------------------------------------------------------

- (NSApplicationTerminateReply)applicationShouldTerminate:(NSApplication *)sender {
  vioTerminateRequested();
  return NSTerminateLater;
}

@end

void vio_ui_run(int overlay_w, int overlay_h, int show_overlay, int show_status) {
  @autoreleasepool {
    [NSApplication sharedApplication];
    [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
    controller = [[VIOController alloc] init];
    controller.canvas = NSMakeSize(overlay_w, overlay_h);
    controller.overlayEnabled = show_overlay != 0;
    controller.showStatus = show_status != 0;
    [controller setup];
    dispatch_async(dispatch_get_main_queue(), ^{
      vioUIReady();
    });
    [NSApp run];
  }
}

static void onMainAsync(dispatch_block_t block) {
  if ([NSThread isMainThread]) {
    block();
  } else {
    dispatch_async(dispatch_get_main_queue(), block);
  }
}

void vio_ui_quit(void) {
  onMainAsync(^{
    [NSApp stop:nil];
    NSEvent *wake = [NSEvent otherEventWithType:NSEventTypeApplicationDefined
                                       location:NSZeroPoint
                                  modifierFlags:0
                                      timestamp:0
                                   windowNumber:0
                                        context:nil
                                        subtype:0
                                          data1:0
                                          data2:0];
    [NSApp postEvent:wake atStart:YES];
  });
}

void vio_ui_reply_terminate(void) {
  onMainAsync(^{
    [NSApp replyToApplicationShouldTerminate:YES];
  });
}

void vio_ui_request_frame(void) {
  onMainAsync(^{
    [controller requestFrame];
  });
}

void vio_ui_set_overlay_enabled(int enabled) {
  onMainAsync(^{
    if (enabled && controller.panel == nil) {
      controller.overlayEnabled = YES;
      [controller buildPanel];
    } else if (!enabled && controller.panel != nil) {
      [controller.pointerTimer invalidate];
      [controller.frameTimer invalidate];
      controller.frameTimer = nil;
      [controller.panel orderOut:nil];
      controller.panel = nil;
      controller.view = nil;
    }
  });
}

void vio_ui_set_status(const char *state) {
  NSString *s = [NSString stringWithUTF8String:state];
  onMainAsync(^{
    [controller setStatus:s];
  });
}

void vio_ui_show_menu(const char *json, double x, double y) {
  NSString *copy = [NSString stringWithUTF8String:json];
  dispatch_async(dispatch_get_main_queue(), ^{
    if (controller.view == nil) return;
    [controller showMenu:copy.UTF8String at:NSMakePoint(x, y)];
  });
}

void vio_ui_open_settings(const char *url, const char *token) {
  NSString *u = [NSString stringWithUTF8String:url];
  NSString *t = [NSString stringWithUTF8String:token];
  onMainAsync(^{
    [controller openSettings:u token:t];
  });
}
