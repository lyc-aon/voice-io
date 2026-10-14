// Display-list ops produced by internal/overlay (Go) and replayed natively.
// The layout must match overlay.Op exactly (checked at startup).
#ifndef VOICE_IO_DRAW_H
#define VOICE_IO_DRAW_H

#include <stdint.h>

enum {
  VIO_RRECT = 1,
  VIO_CIRCLE = 2,
  VIO_PATH = 3,
  VIO_ARC = 4,
  VIO_TEXT = 5,
  VIO_CLIP_PUSH = 6,
  VIO_CLIP_POP = 7,
};

enum {
  VIO_FILL = 1 << 0,
  VIO_STROKE = 1 << 1,
  VIO_MONO = 1 << 2,
  VIO_SHADOW = 1 << 3,
};

enum { VIO_PATH_MOVE = 0, VIO_PATH_LINE = 1, VIO_PATH_CUBIC = 2, VIO_PATH_CLOSE = 3 };

typedef struct vio_op {
  uint8_t kind;
  uint8_t flags;
  uint8_t align;     // 0 left, 1 center, 2 right
  uint8_t truncate;  // 0 tail, 1 head, 2 none
  uint8_t weight;    // 0 regular, 1 medium, 2 semibold, 3 bold
  uint8_t lines;
  uint16_t pad;
  float x, y;
  float w, h;
  float r;
  float line_w;
  float fill[4];
  float stroke[4];
  float shadow[4];
  float shadow_blur;
  float shadow_dy;
  float font_size;
  float a0, a1;
  uint32_t off, len;
} vio_op;

#endif
