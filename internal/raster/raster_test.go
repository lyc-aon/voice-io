package raster

import (
	"image"
	"testing"

	"golang.org/x/image/font/gofont/goregular"

	"gitlab.com/integral-inference/voice-io/internal/overlay"
)

var red = overlay.Color{1, 0, 0, 1}

func render(t *testing.T, build func(l *overlay.List)) *image.RGBA {
	t.Helper()
	fonts, err := NewFonts(goregular.TTF, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var l overlay.List
	build(&l)
	dst := image.NewRGBA(image.Rect(0, 0, 100, 60))
	(&Renderer{Fonts: fonts}).Draw(dst, &l, 1)
	return dst
}

func alphaAt(img *image.RGBA, x, y int) uint8 { return img.Pix[img.PixOffset(x, y)+3] }

func TestRoundedRectFillsItsInteriorButNotItsCorners(t *testing.T) {
	img := render(t, func(l *overlay.List) { l.RRect(10, 10, 60, 30, 12, red, overlay.Color{}, 0, nil) })
	if a := alphaAt(img, 40, 25); a != 255 {
		t.Errorf("centre alpha %d, want 255", a)
	}
	if a := alphaAt(img, 11, 11); a != 0 {
		t.Errorf("rounded-off corner alpha %d, want 0", a)
	}
	if a := alphaAt(img, 80, 25); a != 0 {
		t.Errorf("outside alpha %d, want 0", a)
	}
}

func TestStrokedCircleLeavesItsCentreEmpty(t *testing.T) {
	img := render(t, func(l *overlay.List) { l.Circle(30, 30, 20, overlay.Color{}, red, 4) })
	if a := alphaAt(img, 30, 30); a != 0 {
		t.Errorf("centre alpha %d, want 0", a)
	}
	if a := alphaAt(img, 50, 30); a != 255 {
		t.Errorf("ring alpha %d, want 255", a)
	}
}

// A path that crosses itself must paint its crossing once: overlapping
// stroke pieces neither cancel out nor composite twice.
func TestSelfCrossingStrokePaintsTheCrossingOnce(t *testing.T) {
	half := overlay.Color{1, 0, 0, 0.5}
	img := render(t, func(l *overlay.List) {
		l.BeginPath(0, 0, 1).Move(10, 10).Line(50, 50).Move(50, 10).Line(10, 50).Stroke(half, 6)
	})
	if a := alphaAt(img, 30, 30); a < 125 || a > 130 {
		t.Errorf("crossing alpha %d, want about 128", a)
	}
}

func TestClipLimitsLaterOpsUntilPopped(t *testing.T) {
	img := render(t, func(l *overlay.List) {
		l.ClipRRect(0, 0, 50, 60, 0)
		l.RRect(0, 0, 100, 30, 0, red, overlay.Color{}, 0, nil)
		l.PopClip()
		l.RRect(0, 30, 100, 30, 0, red, overlay.Color{}, 0, nil)
	})
	if a := alphaAt(img, 25, 15); a != 255 {
		t.Errorf("inside clip alpha %d, want 255", a)
	}
	if a := alphaAt(img, 75, 15); a != 0 {
		t.Errorf("outside clip alpha %d, want 0", a)
	}
	if a := alphaAt(img, 75, 45); a != 255 {
		t.Errorf("after pop alpha %d, want 255", a)
	}
}

func TestTruncatedTextStaysInsideItsBox(t *testing.T) {
	img := render(t, func(l *overlay.List) {
		l.Text("the release notes for Friday and the rest of the week", 10, 20, 50, 20,
			overlay.TextStyle{Size: 13, Color: red, Truncate: overlay.TruncTail})
	})
	inked := false
	for y := range 60 {
		for x := range 100 {
			if alphaAt(img, x, y) == 0 {
				continue
			}
			inked = true
			if x >= 62 {
				t.Fatalf("ink at x=%d, beyond the box's right edge (60)", x)
			}
		}
	}
	if !inked {
		t.Fatal("no text drawn")
	}
}
