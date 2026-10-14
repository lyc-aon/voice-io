// GTK 3 host for the overlay (X11, including XWayland) and cairo/pango
// replay of overlay display lists.
#ifndef VOICE_IO_UI_LINUX_H
#define VOICE_IO_UI_LINUX_H

#include "vio_draw.h"

typedef struct vio_frame_info {
  int animating;
  int visible;
  int active;
  double offset_y;
} vio_frame_info;

unsigned long vio_op_off_offset(void);

// Host (ui_linux.c). vio_gtk_init must run on the thread that calls
// vio_ui_run; every other entry point is safe from any thread unless noted.
int vio_gtk_init(void);
void vio_ui_run(int canvas_w, int canvas_h, int overlay);
void vio_ui_quit(void);
void vio_ui_request_frame(void);
void vio_ui_set_overlay_enabled(int enabled);
void vio_ui_open_uri(const char *uri);
// Input shape from x, y, w, h quadruples (UI thread, during a frame).
void vio_ui_set_input_rects(const float *rects, int n);
// Menus are built and shown on the UI thread.
void *vio_menu_new(void);
void vio_menu_add_item(void *menu, const char *title, const char *subtitle, const char *action, int checked,
                       int disabled, int header);
void vio_menu_add_separator(void *menu);
void *vio_menu_add_submenu(void *menu, const char *title);
void vio_menu_popup(void *menu);

// Drawing (draw_linux.c); usable without a display.
void vio_set_font_family(const char *family);
void vio_replay(void *cr, const vio_op *ops, int n, const char *text, const float *path);
double vio_measure_text(const char *text, int len, float size, int weight, int mono);
int vio_render_png(const vio_op *ops, int n, const char *text, const float *path, int width, int height,
                   double scale, const float *backdrop, const char *out);

#endif
