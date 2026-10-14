// CoreGraphics/CoreText replay of overlay display lists, plus offscreen PNG
// rendering and text measurement. Coordinates are top-left origin points.
#import <AppKit/AppKit.h>
#import <CoreText/CoreText.h>
#import <ImageIO/ImageIO.h>
#import <UniformTypeIdentifiers/UniformTypeIdentifiers.h>

#include "ui_darwin.h"
#include "vio_draw.h"

static NSFontWeight fontWeight(uint8_t w) {
  switch (w) {
    case 1:
      return NSFontWeightMedium;
    case 2:
      return NSFontWeightSemibold;
    case 3:
      return NSFontWeightBold;
  }
  return NSFontWeightRegular;
}

static NSFont *fontFor(float size, uint8_t weight, BOOL mono) {
  return mono ? [NSFont monospacedDigitSystemFontOfSize:size weight:fontWeight(weight)]
              : [NSFont systemFontOfSize:size weight:fontWeight(weight)];
}

unsigned long vio_op_off_offset(void) { return (unsigned long)offsetof(vio_op, off); }

double vio_measure_text(const char *text, int len, float size, int weight, int mono) {
  @autoreleasepool {
    NSString *s = [[NSString alloc] initWithBytes:text length:(NSUInteger)len encoding:NSUTF8StringEncoding];
    if (s == nil) return 0;
    NSDictionary *attrs = @{NSFontAttributeName : fontFor(size, (uint8_t)weight, mono != 0)};
    NSAttributedString *as = [[NSAttributedString alloc] initWithString:s attributes:attrs];
    CTLineRef line = CTLineCreateWithAttributedString((__bridge CFAttributedStringRef)as);
    double w = CTLineGetTypographicBounds(line, NULL, NULL, NULL);
    CFRelease(line);
    return ceil(w);
  }
}

static CGColorRef color(const float c[4]) {
  return CGColorCreateSRGB(c[0], c[1], c[2], c[3]);
}

static CGPathRef rrectPath(CGRect r, CGFloat radius) {
  radius = MIN(radius, MIN(r.size.width, r.size.height) / 2);
  if (radius <= 0) return CGPathCreateWithRect(r, NULL);
  return CGPathCreateWithRoundedRect(r, radius, radius, NULL);
}

static void fillStroke(CGContextRef ctx, CGPathRef path, const vio_op *op, CGFloat scale) {
  CGContextSaveGState(ctx);
  if (op->flags & VIO_SHADOW) {
    CGColorRef sc = color(op->shadow);
    // Shadow geometry is in device space (unaffected by the flipped CTM).
    CGContextSetShadowWithColor(ctx, CGSizeMake(0, -op->shadow_dy * scale), op->shadow_blur * scale, sc);
    CGColorRelease(sc);
  }
  if (op->flags & VIO_FILL) {
    CGColorRef fc = color(op->fill);
    CGContextSetFillColorWithColor(ctx, fc);
    CGContextAddPath(ctx, path);
    CGContextFillPath(ctx);
    CGColorRelease(fc);
    CGContextSetShadowWithColor(ctx, CGSizeZero, 0, NULL);
  }
  if (op->flags & VIO_STROKE) {
    CGColorRef stc = color(op->stroke);
    CGContextSetStrokeColorWithColor(ctx, stc);
    CGContextSetLineWidth(ctx, op->line_w);
    CGContextSetLineCap(ctx, kCGLineCapRound);
    CGContextSetLineJoin(ctx, kCGLineJoinRound);
    CGContextAddPath(ctx, path);
    CGContextStrokePath(ctx);
    CGColorRelease(stc);
  }
  CGContextRestoreGState(ctx);
}

static CGPathRef buildPath(const float *p, uint32_t off, uint32_t len) {
  CGMutablePathRef path = CGPathCreateMutable();
  uint32_t i = off, end = off + len;
  while (i < end) {
    int cmd = (int)p[i++];
    switch (cmd) {
      case VIO_PATH_MOVE:
        CGPathMoveToPoint(path, NULL, p[i], p[i + 1]);
        i += 2;
        break;
      case VIO_PATH_LINE:
        CGPathAddLineToPoint(path, NULL, p[i], p[i + 1]);
        i += 2;
        break;
      case VIO_PATH_CUBIC:
        CGPathAddCurveToPoint(path, NULL, p[i], p[i + 1], p[i + 2], p[i + 3], p[i + 4], p[i + 5]);
        i += 6;
        break;
      case VIO_PATH_CLOSE:
        CGPathCloseSubpath(path);
        break;
      default:
        i = end;
    }
  }
  return path;
}

static void drawText(const vio_op *op, const char *text) {
  NSString *s = [[NSString alloc] initWithBytes:text + op->off length:op->len encoding:NSUTF8StringEncoding];
  if (s.length == 0) return;
  NSMutableParagraphStyle *para = [[NSMutableParagraphStyle alloc] init];
  para.alignment = op->align == 1 ? NSTextAlignmentCenter
                   : op->align == 2 ? NSTextAlignmentRight
                                    : NSTextAlignmentLeft;
  BOOL multi = op->lines > 1;
  para.lineBreakMode = multi ? NSLineBreakByWordWrapping
                       : op->truncate == 1 ? NSLineBreakByTruncatingHead
                       : op->truncate == 2 ? NSLineBreakByClipping
                                           : NSLineBreakByTruncatingTail;
  NSColor *c = [NSColor colorWithSRGBRed:op->fill[0] green:op->fill[1] blue:op->fill[2] alpha:op->fill[3]];
  NSDictionary *attrs = @{
    NSFontAttributeName : fontFor(op->font_size, op->weight, (op->flags & VIO_MONO) != 0),
    NSForegroundColorAttributeName : c,
    NSParagraphStyleAttributeName : para,
  };
  NSStringDrawingOptions opts = NSStringDrawingUsesLineFragmentOrigin | NSStringDrawingTruncatesLastVisibleLine;
  NSRect box = NSMakeRect(op->x, op->y, op->w, op->h);
  NSRect need = [s boundingRectWithSize:NSMakeSize(op->w, op->h) options:opts attributes:attrs];
  CGFloat textH = MIN(ceil(need.size.height), op->h);
  NSRect r = NSMakeRect(box.origin.x, box.origin.y + (box.size.height - textH) / 2, box.size.width, textH);
  [s drawWithRect:r options:opts attributes:attrs context:nil];
}

void vio_replay(void *context, const vio_op *ops, int n, const char *text, const float *path,
                double scale) {
  CGContextRef ctx = (CGContextRef)context;
  int clips = 0;
  for (int k = 0; k < n; k++) {
    const vio_op *op = &ops[k];
    switch (op->kind) {
      case VIO_RRECT: {
        CGPathRef p = rrectPath(CGRectMake(op->x, op->y, op->w, op->h), op->r);
        fillStroke(ctx, p, op, scale);
        CGPathRelease(p);
        break;
      }
      case VIO_CIRCLE: {
        CGPathRef p = CGPathCreateWithEllipseInRect(
            CGRectMake(op->x - op->r, op->y - op->r, 2 * op->r, 2 * op->r), NULL);
        fillStroke(ctx, p, op, scale);
        CGPathRelease(p);
        break;
      }
      case VIO_PATH: {
        CGPathRef p = buildPath(path, op->off, op->len);
        fillStroke(ctx, p, op, scale);
        CGPathRelease(p);
        break;
      }
      case VIO_ARC: {
        CGMutablePathRef p = CGPathCreateMutable();
        CGPathAddArc(p, NULL, op->x, op->y, op->r, op->a0, op->a1, false);
        fillStroke(ctx, p, op, scale);
        CGPathRelease(p);
        break;
      }
      case VIO_TEXT:
        drawText(op, text);
        break;
      case VIO_CLIP_PUSH: {
        CGContextSaveGState(ctx);
        CGPathRef p = rrectPath(CGRectMake(op->x, op->y, op->w, op->h), op->r);
        CGContextAddPath(ctx, p);
        CGContextClip(ctx);
        CGPathRelease(p);
        clips++;
        break;
      }
      case VIO_CLIP_POP:
        if (clips > 0) {
          CGContextRestoreGState(ctx);
          clips--;
        }
        break;
    }
  }
  while (clips-- > 0) CGContextRestoreGState(ctx);
}

int vio_render_png(const vio_op *ops, int n, const char *text, const float *path, int width, int height,
                   double scale, const float *backdrop, const char *out) {
  @autoreleasepool {
    size_t pw = (size_t)ceil(width * scale), ph = (size_t)ceil(height * scale);
    CGColorSpaceRef cs = CGColorSpaceCreateWithName(kCGColorSpaceSRGB);
    CGContextRef ctx = CGBitmapContextCreate(NULL, pw, ph, 8, 0, cs, (CGBitmapInfo)kCGImageAlphaPremultipliedLast);
    CGColorSpaceRelease(cs);
    if (ctx == NULL) return 1;
    // Top-left origin in points, matching a flipped NSView.
    CGContextTranslateCTM(ctx, 0, (CGFloat)ph);
    CGContextScaleCTM(ctx, scale, -scale);
    if (backdrop != NULL && backdrop[3] > 0) {
      CGColorRef bg = color(backdrop);
      CGContextSetFillColorWithColor(ctx, bg);
      CGContextFillRect(ctx, CGRectMake(0, 0, width, height));
      CGColorRelease(bg);
    }
    NSGraphicsContext *gc = [NSGraphicsContext graphicsContextWithCGContext:ctx flipped:YES];
    [NSGraphicsContext saveGraphicsState];
    [NSGraphicsContext setCurrentContext:gc];
    vio_replay(ctx, ops, n, text, path, scale);
    [NSGraphicsContext restoreGraphicsState];
    CGImageRef img = CGBitmapContextCreateImage(ctx);
    CGContextRelease(ctx);
    NSURL *url = [NSURL fileURLWithPath:[NSString stringWithUTF8String:out]];
    CGImageDestinationRef dest = CGImageDestinationCreateWithURL(
        (__bridge CFURLRef)url, (__bridge CFStringRef)UTTypePNG.identifier, 1, NULL);
    if (dest == NULL) {
      CGImageRelease(img);
      return 2;
    }
    CGImageDestinationAddImage(dest, img, NULL);
    BOOL ok = CGImageDestinationFinalize(dest);
    CFRelease(dest);
    CGImageRelease(img);
    return ok ? 0 : 3;
  }
}
