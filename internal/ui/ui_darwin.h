// AppKit host for the overlay, menu bar item, menus and Settings window.
#ifndef VOICE_IO_UI_DARWIN_H
#define VOICE_IO_UI_DARWIN_H

#include "vio_draw.h"

typedef struct vio_frame_info {
  int animating;
  int visible;
  int active;  // a session capsule (not the resting handle) is showing
  double offset_y;  // from the screen's vertical centre, down is positive
} vio_frame_info;

// Runs the AppKit loop until vio_ui_quit; calls vioUIReady once running.
void vio_ui_run(int overlay_w, int overlay_h, int show_overlay, int show_status);
void vio_ui_quit(void);
void vio_ui_request_frame(void);
void vio_ui_set_overlay_enabled(int enabled);
void vio_ui_set_status(const char *state);
void vio_ui_show_menu(const char *json, double x, double y);
void vio_ui_open_settings(const char *url, const char *token);
void vio_ui_reply_terminate(void);

// Drawing (draw_darwin.m).
void vio_replay(void *ctx, const vio_op *ops, int n, const char *text, const float *path, double scale);
int vio_render_png(const vio_op *ops, int n, const char *text, const float *path, int width, int height,
                   double scale, const float *backdrop, const char *out);
double vio_measure_text(const char *text, int len, float size, int weight, int mono);
// offsetof(vio_op, off), for the Go layout check.
unsigned long vio_op_off_offset(void);

#endif
