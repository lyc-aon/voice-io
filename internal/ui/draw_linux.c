// cairo/pango replay of overlay display lists, offscreen PNG rendering and
// text measurement. Coordinates are top-left origin logical pixels; none of
// this needs a display connection.
#include <math.h>
#include <stddef.h>
#include <string.h>

#include <cairo.h>
#include <pango/pangocairo.h>

#include "ui_linux.h"

static char fontFamily[128] = "Sans";

unsigned long vio_op_off_offset(void) { return (unsigned long)offsetof(vio_op, off); }

void vio_set_font_family(const char *family) {
  if (family != NULL && family[0] != '\0') {
    strncpy(fontFamily, family, sizeof fontFamily - 1);
    fontFamily[sizeof fontFamily - 1] = '\0';
  }
}

static PangoWeight pangoWeight(int w) {
  switch (w) {
    case 1:
      return PANGO_WEIGHT_MEDIUM;
    case 2:
      return PANGO_WEIGHT_SEMIBOLD;
    case 3:
      return PANGO_WEIGHT_BOLD;
  }
  return PANGO_WEIGHT_NORMAL;
}

static void setupLayout(PangoLayout *layout, const char *text, int len, float size, int weight, int mono) {
  PangoFontDescription *fd = pango_font_description_new();
  pango_font_description_set_family(fd, fontFamily);
  pango_font_description_set_weight(fd, pangoWeight(weight));
  pango_font_description_set_absolute_size(fd, size * PANGO_SCALE);
  pango_layout_set_font_description(layout, fd);
  pango_font_description_free(fd);
  if (mono) {
    PangoAttrList *attrs = pango_attr_list_new();
    pango_attr_list_insert(attrs, pango_attr_font_features_new("tnum"));
    pango_layout_set_attributes(layout, attrs);
    pango_attr_list_unref(attrs);
  }
  pango_layout_set_text(layout, text, len);
}

double vio_measure_text(const char *text, int len, float size, int weight, int mono) {
  PangoFontMap *fm = pango_cairo_font_map_get_default();  // per thread
  PangoContext *ctx = pango_font_map_create_context(fm);
  PangoLayout *layout = pango_layout_new(ctx);
  setupLayout(layout, text, len, size, weight, mono);
  PangoRectangle logical;
  pango_layout_get_extents(layout, NULL, &logical);
  g_object_unref(layout);
  g_object_unref(ctx);
  return (double)logical.width / PANGO_SCALE;
}

static void rrect(cairo_t *cr, double x, double y, double w, double h, double r) {
  r = fmin(r, fmin(w, h) / 2);
  cairo_new_sub_path(cr);
  if (r <= 0) {
    cairo_rectangle(cr, x, y, w, h);
    return;
  }
  cairo_arc(cr, x + w - r, y + r, r, -M_PI / 2, 0);
  cairo_arc(cr, x + w - r, y + h - r, r, 0, M_PI / 2);
  cairo_arc(cr, x + r, y + h - r, r, M_PI / 2, M_PI);
  cairo_arc(cr, x + r, y + r, r, M_PI, 3 * M_PI / 2);
  cairo_close_path(cr);
}

static void setColor(cairo_t *cr, const float c[4]) { cairo_set_source_rgba(cr, c[0], c[1], c[2], c[3]); }

// Shape paths for the shadowable ops, grown outward by d.
static void shapePath(cairo_t *cr, const vio_op *op, double d) {
  if (op->kind == VIO_CIRCLE) {
    cairo_new_sub_path(cr);
    cairo_arc(cr, op->x, op->y, op->r + d, 0, 2 * M_PI);
    cairo_close_path(cr);
  } else {
    rrect(cr, op->x - d, op->y - d, op->w + 2 * d, op->h + 2 * d, op->r + d);
  }
}

// cairo has no blur: a soft drop shadow is stacked translucent layers that
// grow by the blur radius, approximating a gaussian falloff.
static void shadow(cairo_t *cr, const vio_op *op) {
  const int layers = 10;
  double blur = op->shadow_blur;
  if (blur <= 0 || op->shadow[3] <= 0) return;
  cairo_save(cr);
  cairo_translate(cr, 0, op->shadow_dy);
  for (int i = layers; i >= 1; i--) {
    double t = (double)i / layers;
    double alpha = op->shadow[3] * 2.2 / layers * (1 - t) * (1 - t);
    cairo_set_source_rgba(cr, op->shadow[0], op->shadow[1], op->shadow[2], alpha);
    shapePath(cr, op, blur * t - blur * 0.35);
    cairo_fill(cr);
  }
  cairo_restore(cr);
}

static void fillStroke(cairo_t *cr, const vio_op *op) {
  if (op->flags & VIO_FILL) {
    setColor(cr, op->fill);
    cairo_fill_preserve(cr);
  }
  if (op->flags & VIO_STROKE) {
    setColor(cr, op->stroke);
    cairo_set_line_width(cr, op->line_w);
    cairo_set_line_cap(cr, CAIRO_LINE_CAP_ROUND);
    cairo_set_line_join(cr, CAIRO_LINE_JOIN_ROUND);
    cairo_stroke_preserve(cr);
  }
  cairo_new_path(cr);
}

static void buildPath(cairo_t *cr, const float *p, uint32_t off, uint32_t len) {
  uint32_t i = off, end = off + len;
  cairo_new_path(cr);
  while (i < end) {
    int cmd = (int)p[i++];
    switch (cmd) {
      case VIO_PATH_MOVE:
        cairo_move_to(cr, p[i], p[i + 1]);
        i += 2;
        break;
      case VIO_PATH_LINE:
        cairo_line_to(cr, p[i], p[i + 1]);
        i += 2;
        break;
      case VIO_PATH_CUBIC:
        cairo_curve_to(cr, p[i], p[i + 1], p[i + 2], p[i + 3], p[i + 4], p[i + 5]);
        i += 6;
        break;
      case VIO_PATH_CLOSE:
        cairo_close_path(cr);
        break;
      default:
        i = end;
    }
  }
}

static void drawText(cairo_t *cr, const vio_op *op, const char *text) {
  if (op->len == 0 || op->w <= 0) return;
  PangoLayout *layout = pango_cairo_create_layout(cr);
  setupLayout(layout, text + op->off, (int)op->len, op->font_size, op->weight, (op->flags & VIO_MONO) != 0);
  pango_layout_set_width(layout, (int)(op->w * PANGO_SCALE));
  pango_layout_set_alignment(layout, op->align == 1   ? PANGO_ALIGN_CENTER
                                     : op->align == 2 ? PANGO_ALIGN_RIGHT
                                                      : PANGO_ALIGN_LEFT);
  if (op->lines > 1) {
    pango_layout_set_wrap(layout, PANGO_WRAP_WORD);
    pango_layout_set_height(layout, -(int)op->lines);
    pango_layout_set_ellipsize(layout, PANGO_ELLIPSIZE_END);
  } else {
    pango_layout_set_single_paragraph_mode(layout, TRUE);
    pango_layout_set_ellipsize(layout, op->truncate == 1   ? PANGO_ELLIPSIZE_START
                                       : op->truncate == 2 ? PANGO_ELLIPSIZE_NONE
                                                           : PANGO_ELLIPSIZE_END);
  }
  PangoRectangle logical;
  pango_layout_get_extents(layout, NULL, &logical);
  double th = fmin((double)logical.height / PANGO_SCALE, op->h);
  cairo_save(cr);
  if (op->truncate == 2 && op->lines <= 1) {
    cairo_rectangle(cr, op->x, op->y, op->w, op->h);
    cairo_clip(cr);
  }
  setColor(cr, op->fill);
  cairo_move_to(cr, op->x, op->y + (op->h - th) / 2);
  pango_cairo_show_layout(cr, layout);
  cairo_restore(cr);
  g_object_unref(layout);
}

void vio_replay(void *context, const vio_op *ops, int n, const char *text, const float *path) {
  cairo_t *cr = (cairo_t *)context;
  int clips = 0;
  for (int k = 0; k < n; k++) {
    const vio_op *op = &ops[k];
    switch (op->kind) {
      case VIO_RRECT:
      case VIO_CIRCLE:
        if (op->flags & VIO_SHADOW) shadow(cr, op);
        shapePath(cr, op, 0);
        fillStroke(cr, op);
        break;
      case VIO_PATH:
        buildPath(cr, path, op->off, op->len);
        fillStroke(cr, op);
        break;
      case VIO_ARC:
        cairo_new_path(cr);
        cairo_arc(cr, op->x, op->y, op->r, op->a0, op->a1);
        fillStroke(cr, op);
        break;
      case VIO_TEXT:
        drawText(cr, op, text);
        break;
      case VIO_CLIP_PUSH:
        cairo_save(cr);
        rrect(cr, op->x, op->y, op->w, op->h, op->r);
        cairo_clip(cr);
        clips++;
        break;
      case VIO_CLIP_POP:
        if (clips > 0) {
          cairo_restore(cr);
          clips--;
        }
        break;
    }
  }
  while (clips-- > 0) cairo_restore(cr);
}

int vio_render_png(const vio_op *ops, int n, const char *text, const float *path, int width, int height,
                   double scale, const float *backdrop, const char *out) {
  int pw = (int)ceil(width * scale), ph = (int)ceil(height * scale);
  cairo_surface_t *surface = cairo_image_surface_create(CAIRO_FORMAT_ARGB32, pw, ph);
  if (cairo_surface_status(surface) != CAIRO_STATUS_SUCCESS) return 1;
  cairo_t *cr = cairo_create(surface);
  cairo_scale(cr, scale, scale);
  if (backdrop != NULL && backdrop[3] > 0) {
    setColor(cr, backdrop);
    cairo_paint(cr);
  }
  vio_replay(cr, ops, n, text, path);
  cairo_destroy(cr);
  cairo_status_t st = cairo_surface_write_to_png(surface, out);
  cairo_surface_destroy(surface);
  return st == CAIRO_STATUS_SUCCESS ? 0 : 2;
}
