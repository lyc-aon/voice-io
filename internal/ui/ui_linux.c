// GTK 3 overlay host. The overlay is an override-redirect X11 window (also
// under XWayland, where Wayland clients cannot place themselves) that never
// takes focus. Input is accepted only over the pill's interactive areas, so
// the rest of the canvas is click-through and hover events arrive without
// polling the pointer.
#include <math.h>
#include <stdlib.h>
#include <string.h>

#include <gio/gio.h>
#include <gtk/gtk.h>

#include "_cgo_export.h"
#include "ui_linux.h"

static GtkWidget *win;
static int canvasW, canvasH;
static gboolean overlayEnabled;
static gboolean running;
static guint frameTimer;
static gint framePending;  // atomic: a frame request is queued
static double placedOffset = NAN;
static int placedMonitorY = -1;
static GSettings *interfaceSettings;

static gboolean isDark(void) {
  if (interfaceSettings != NULL) {
    gchar *scheme = g_settings_get_string(interfaceSettings, "color-scheme");
    gboolean dark = scheme != NULL && strcmp(scheme, "prefer-dark") == 0;
    g_free(scheme);
    if (dark) return TRUE;
  }
  gboolean prefer = FALSE;
  g_object_get(gtk_settings_get_default(), "gtk-application-prefer-dark-theme", &prefer, NULL);
  if (prefer) return TRUE;
  gchar *theme = NULL;
  g_object_get(gtk_settings_get_default(), "gtk-theme-name", &theme, NULL);
  gboolean darkTheme = theme != NULL && g_str_has_suffix(theme, "-dark");
  g_free(theme);
  return darkTheme;
}

static void appearanceChanged(void) { vioAppearance(isDark()); }

static void onSettingChanged(GSettings *s, gchar *key, gpointer data) { appearanceChanged(); }

static void onThemeNotify(GObject *o, GParamSpec *p, gpointer data) { appearanceChanged(); }

// Font family of the desktop UI font ("Cantarell 11" -> "Cantarell").
static void adoptDesktopFont(void) {
  gchar *name = NULL;
  g_object_get(gtk_settings_get_default(), "gtk-font-name", &name, NULL);
  if (name != NULL) {
    PangoFontDescription *fd = pango_font_description_from_string(name);
    const char *family = pango_font_description_get_family(fd);
    if (family != NULL) vio_set_font_family(family);
    pango_font_description_free(fd);
    g_free(name);
  }
}

int vio_gtk_init(void) {
  // Absolute placement and override-redirect need X11; on a Wayland
  // session this uses XWayland, as the GNOME extension's peers do.
  gdk_set_allowed_backends("x11");
  if (!gtk_init_check(NULL, NULL)) return 0;
  adoptDesktopFont();
  return 1;
}

static GdkMonitor *targetMonitor(void) {
  GdkDisplay *d = gdk_display_get_default();
  GdkMonitor *m = gdk_display_get_primary_monitor(d);
  if (m == NULL && gdk_display_get_n_monitors(d) > 0) m = gdk_display_get_monitor(d, 0);
  return m;
}

static void place(double offsetY) {
  GdkMonitor *m = targetMonitor();
  if (m == NULL) return;
  GdkRectangle wa;
  gdk_monitor_get_workarea(m, &wa);
  if (offsetY == placedOffset && wa.y == placedMonitorY) return;
  placedOffset = offsetY;
  placedMonitorY = wa.y;
  vioScreenHeight(wa.height);
  int x = wa.x + wa.width - canvasW;
  int y = wa.y + wa.height / 2 + (int)lround(offsetY) - canvasH / 2;
  gtk_window_move(GTK_WINDOW(win), x, y);
}

void vio_ui_set_input_rects(const float *rects, int n) {
  if (win == NULL || gtk_widget_get_window(win) == NULL) return;
  cairo_region_t *region = cairo_region_create();
  for (int i = 0; i + 3 < n; i += 4) {
    cairo_rectangle_int_t r = {(int)floor(rects[i]), (int)floor(rects[i + 1]), (int)ceil(rects[i + 2]),
                               (int)ceil(rects[i + 3])};
    cairo_region_union_rectangle(region, &r);
  }
  gtk_widget_input_shape_combine_region(win, region);
  cairo_region_destroy(region);
}

static gboolean frame(gpointer data) {
  g_atomic_int_set(&framePending, 0);
  vio_frame_info info = {0};
  vioOverlayFrame(&info);
  gboolean show = overlayEnabled && info.visible;
  if (show) {
    place(info.offset_y);
    if (!gtk_widget_get_visible(win)) gtk_widget_show(win);
    gtk_widget_queue_draw(win);
  } else if (gtk_widget_get_visible(win)) {
    gtk_widget_hide(win);
  }
  if (info.animating && show) return G_SOURCE_CONTINUE;
  frameTimer = 0;
  return G_SOURCE_REMOVE;
}

static gboolean startFrames(gpointer data) {
  if (running && win != NULL && frameTimer == 0 && frame(NULL) == G_SOURCE_CONTINUE) {
    frameTimer = g_timeout_add(16, frame, NULL);
  }
  return G_SOURCE_REMOVE;
}

void vio_ui_request_frame(void) {
  if (g_atomic_int_compare_and_exchange(&framePending, 0, 1)) g_idle_add(startFrames, NULL);
}

static gboolean onDraw(GtkWidget *w, cairo_t *cr, gpointer data) {
  cairo_save(cr);
  cairo_set_operator(cr, CAIRO_OPERATOR_SOURCE);
  cairo_set_source_rgba(cr, 0, 0, 0, 0);
  cairo_paint(cr);
  cairo_restore(cr);
  vioOverlayReplay(cr);
  return TRUE;
}

static gboolean onButtonPress(GtkWidget *w, GdkEventButton *e, gpointer data) {
  if (e->type != GDK_BUTTON_PRESS) return TRUE;  // ignore double-click synthesis
  vioOverlayMouse(0, e->x, e->y, e->y_root, e->button == 1 ? 0 : 1);
  return TRUE;
}

static gboolean onMotion(GtkWidget *w, GdkEventMotion *e, gpointer data) {
  if (e->state & GDK_BUTTON1_MASK) {
    vioOverlayMouse(1, e->x, e->y, e->y_root, 0);
  } else {
    vioOverlayPointer(e->x, e->y, 1);
  }
  return TRUE;
}

static gboolean onRelease(GtkWidget *w, GdkEventButton *e, gpointer data) {
  if (e->button == 1) vioOverlayMouse(2, e->x, e->y, e->y_root, 0);
  return TRUE;
}

static gboolean onLeave(GtkWidget *w, GdkEventCrossing *e, gpointer data) {
  if (e->mode == GDK_CROSSING_NORMAL) vioOverlayPointer(e->x, e->y, 0);
  return TRUE;
}

static void buildOverlay(void) {
  win = gtk_window_new(GTK_WINDOW_POPUP);
  gtk_window_set_title(GTK_WINDOW(win), "Voice IO");
  gtk_window_set_decorated(GTK_WINDOW(win), FALSE);
  gtk_window_set_accept_focus(GTK_WINDOW(win), FALSE);
  gtk_window_set_focus_on_map(GTK_WINDOW(win), FALSE);
  gtk_window_set_keep_above(GTK_WINDOW(win), TRUE);
  gtk_window_set_skip_taskbar_hint(GTK_WINDOW(win), TRUE);
  gtk_window_set_skip_pager_hint(GTK_WINDOW(win), TRUE);
  gtk_window_set_default_size(GTK_WINDOW(win), canvasW, canvasH);
  gtk_widget_set_size_request(win, canvasW, canvasH);
  gtk_widget_set_app_paintable(win, TRUE);
  GdkScreen *screen = gtk_widget_get_screen(win);
  GdkVisual *rgba = gdk_screen_get_rgba_visual(screen);
  if (rgba != NULL) gtk_widget_set_visual(win, rgba);
  gtk_widget_add_events(win, GDK_POINTER_MOTION_MASK | GDK_BUTTON_PRESS_MASK | GDK_BUTTON_RELEASE_MASK |
                                 GDK_BUTTON1_MOTION_MASK | GDK_LEAVE_NOTIFY_MASK);
  g_signal_connect(win, "draw", G_CALLBACK(onDraw), NULL);
  g_signal_connect(win, "button-press-event", G_CALLBACK(onButtonPress), NULL);
  g_signal_connect(win, "button-release-event", G_CALLBACK(onRelease), NULL);
  g_signal_connect(win, "motion-notify-event", G_CALLBACK(onMotion), NULL);
  g_signal_connect(win, "leave-notify-event", G_CALLBACK(onLeave), NULL);
  gtk_widget_realize(win);
  vio_ui_set_input_rects(NULL, 0);
}

static void watchAppearance(void) {
  GSettingsSchemaSource *src = g_settings_schema_source_get_default();
  GSettingsSchema *schema =
      src != NULL ? g_settings_schema_source_lookup(src, "org.gnome.desktop.interface", TRUE) : NULL;
  if (schema != NULL) {
    if (g_settings_schema_has_key(schema, "color-scheme")) {
      interfaceSettings = g_settings_new("org.gnome.desktop.interface");
      g_signal_connect(interfaceSettings, "changed::color-scheme", G_CALLBACK(onSettingChanged), NULL);
    }
    g_settings_schema_unref(schema);
  }
  g_signal_connect(gtk_settings_get_default(), "notify::gtk-theme-name", G_CALLBACK(onThemeNotify), NULL);
  g_signal_connect(gtk_settings_get_default(), "notify::gtk-application-prefer-dark-theme",
                   G_CALLBACK(onThemeNotify), NULL);
  appearanceChanged();
}

static gboolean ready(gpointer data) {
  vioUIReady();
  return G_SOURCE_REMOVE;
}

void vio_ui_run(int cw, int ch, int overlay) {
  canvasW = cw;
  canvasH = ch;
  overlayEnabled = overlay != 0;
  buildOverlay();
  watchAppearance();
  running = TRUE;
  g_idle_add(ready, NULL);
  vio_ui_request_frame();
  gtk_main();
  running = FALSE;
}

static gboolean quitIdle(gpointer data) {
  if (running) gtk_main_quit();
  return G_SOURCE_REMOVE;
}

void vio_ui_quit(void) { g_idle_add(quitIdle, NULL); }

static gboolean setOverlayIdle(gpointer data) {
  overlayEnabled = GPOINTER_TO_INT(data);
  if (!overlayEnabled && win != NULL) gtk_widget_hide(win);
  startFrames(NULL);
  return G_SOURCE_REMOVE;
}

void vio_ui_set_overlay_enabled(int enabled) { g_idle_add(setOverlayIdle, GINT_TO_POINTER(enabled != 0)); }

static gboolean openURIIdle(gpointer data) {
  GError *err = NULL;
  if (!gtk_show_uri_on_window(NULL, (const char *)data, GDK_CURRENT_TIME, &err)) {
    g_warning("cannot open %s: %s", (const char *)data, err != NULL ? err->message : "unknown error");
    g_clear_error(&err);
  }
  g_free(data);
  return G_SOURCE_REMOVE;
}

void vio_ui_open_uri(const char *uri) { g_idle_add(openURIIdle, g_strdup(uri)); }

// ---- menus -------------------------------------------------------------------

static void onMenuActivate(GtkMenuItem *item, gpointer data) { vioMenuAction((char *)data); }

void *vio_menu_new(void) {
  GtkWidget *menu = gtk_menu_new();
  g_object_ref_sink(menu);
  return menu;
}

void vio_menu_add_item(void *menu, const char *title, const char *subtitle, const char *action, int checked,
                       int disabled, int header) {
  GtkWidget *item = checked ? gtk_check_menu_item_new() : gtk_menu_item_new();
  if (checked) gtk_check_menu_item_set_active(GTK_CHECK_MENU_ITEM(item), TRUE);
  GtkWidget *label = gtk_label_new(NULL);
  gchar *t = g_markup_escape_text(title, -1);
  gchar *markup = NULL;
  if (subtitle != NULL && subtitle[0] != '\0') {
    gchar *s = g_markup_escape_text(subtitle, -1);
    markup = g_strdup_printf(header ? "<b>%s</b>\n<small>%s</small>" : "%s\n<small>%s</small>", t, s);
    g_free(s);
  } else {
    markup = g_strdup_printf(header ? "<b>%s</b>" : "%s", t);
  }
  gtk_label_set_markup(GTK_LABEL(label), markup);
  gtk_label_set_xalign(GTK_LABEL(label), 0);
  g_free(t);
  g_free(markup);
  gtk_container_add(GTK_CONTAINER(item), label);
  if (disabled || header || action == NULL || action[0] == '\0') {
    gtk_widget_set_sensitive(item, FALSE);
  } else {
    g_signal_connect_data(item, "activate", G_CALLBACK(onMenuActivate), g_strdup(action),
                          (GClosureNotify)g_free, 0);
  }
  gtk_widget_show_all(item);
  gtk_menu_shell_append(GTK_MENU_SHELL(menu), item);
}

void vio_menu_add_separator(void *menu) {
  GtkWidget *sep = gtk_separator_menu_item_new();
  gtk_widget_show(sep);
  gtk_menu_shell_append(GTK_MENU_SHELL(menu), sep);
}

void *vio_menu_add_submenu(void *menu, const char *title) {
  GtkWidget *item = gtk_menu_item_new_with_label(title);
  GtkWidget *sub = gtk_menu_new();
  gtk_menu_item_set_submenu(GTK_MENU_ITEM(item), sub);
  gtk_widget_show(item);
  gtk_menu_shell_append(GTK_MENU_SHELL(menu), item);
  return sub;
}

// The menu is released after its item's "activate" has run.
static gboolean unrefMenu(gpointer menu) {
  g_object_unref(menu);
  return G_SOURCE_REMOVE;
}

static void onMenuHidden(GtkWidget *menu, gpointer data) { g_idle_add(unrefMenu, menu); }

void vio_menu_popup(void *menu) {
  GtkWidget *m = GTK_WIDGET(menu);
  g_signal_connect(m, "deactivate", G_CALLBACK(onMenuHidden), NULL);
  GdkEvent *ev = gtk_get_current_event();
  gtk_menu_popup_at_pointer(GTK_MENU(m), ev);
  if (ev != NULL) gdk_event_free(ev);
}
