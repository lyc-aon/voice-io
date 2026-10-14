package overlay

import (
	"strconv"
	"strings"
)

// Theme is the resolved palette for one appearance.
type Theme struct {
	Dark      bool
	Surface   Color // pill fill
	Border    Color
	Shadow    Color
	Text      Color
	Secondary Color
	Button    Color
	ButtonHot Color
	Ink       Color    // primary glyphs (the accent when one is set)
	OnInk     Color    // glyphs drawn on an Ink fill
	Wave      [3]Color // waveform colors, top to bottom
	Glow      Color    // waveform glow, spinner and ready pulse
	Success   Color
	Warning   Color
	Error     Color
	Record    Color
	Preview   Color
	Handle    Color
	HandleRim Color
}

// AccentGradient is the default accent: a blue-violet-rose waveform.
const AccentGradient = "gradient"

// ParseHex parses #RRGGBB.
func ParseHex(s string) (Color, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) != 6 {
		return Color{}, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return Color{}, false
	}
	return RGB(uint8(v>>16), uint8(v>>8), uint8(v)), true
}

func luminance(c Color) float32 { return 0.2126*c[0] + 0.7152*c[1] + 0.0722*c[2] }

// NewTheme builds a palette. accent is AccentGradient (a colored waveform),
// "" (monochrome: white on a black pill, black on a white one) or #RRGGBB;
// preview "" uses the default caption color.
func NewTheme(dark bool, accent, preview string) Theme {
	var t Theme
	t.Dark = dark
	if dark {
		t.Surface = Color{0.03, 0.03, 0.035, 0.93}
		t.Border = Color{1, 1, 1, 0.14}
		t.Shadow = Color{0, 0, 0, 0.45}
		t.Text = Color{0.97, 0.97, 0.98, 1}
		t.Secondary = Color{1, 1, 1, 0.62}
		t.Button = Color{1, 1, 1, 0.10}
		t.ButtonHot = Color{1, 1, 1, 0.18}
		t.Success = RGB(0x30, 0xd1, 0x58)
		t.Warning = RGB(0xff, 0xb0, 0x3a)
		t.Error = RGB(0xff, 0x55, 0x4a)
		t.Record = RGB(0xff, 0x45, 0x3a)
		t.Preview = Color{1, 1, 1, 0.92}
		t.Handle = Color{0.05, 0.05, 0.06, 0.78}
		t.HandleRim = Color{1, 1, 1, 0.38}
	} else {
		t.Surface = Color{0.99, 0.99, 0.995, 0.96}
		t.Border = Color{0, 0, 0, 0.12}
		t.Shadow = Color{0, 0, 0, 0.22}
		t.Text = Color{0.10, 0.10, 0.11, 1}
		t.Secondary = Color{0, 0, 0, 0.55}
		t.Button = Color{0, 0, 0, 0.06}
		t.ButtonHot = Color{0, 0, 0, 0.12}
		t.Success = RGB(0x1f, 0xa8, 0x4a)
		t.Warning = RGB(0xd9, 0x7a, 0x00)
		t.Error = RGB(0xe0, 0x30, 0x2a)
		t.Record = RGB(0xe8, 0x33, 0x2b)
		t.Preview = Color{0.10, 0.10, 0.11, 0.92}
		t.Handle = Color{0.99, 0.99, 0.995, 0.85}
		t.HandleRim = Color{0, 0, 0, 0.25}
	}
	t.Ink = t.Text
	if dark {
		t.Wave = [3]Color{RGB(0x5e, 0xb8, 0xff), RGB(0xb0, 0x6b, 0xff), RGB(0xff, 0x6b, 0x95)}
	} else {
		t.Wave = [3]Color{RGB(0x1f, 0x6f, 0xf2), RGB(0x84, 0x3c, 0xe8), RGB(0xe8, 0x35, 0x6e)}
	}
	switch acc, ok := ParseHex(accent); {
	case ok:
		// Keep a chosen accent legible against the pill.
		switch {
		case dark && luminance(acc) < 0.18:
			acc = Mix(acc, RGB(255, 255, 255), 0.35)
		case !dark && luminance(acc) > 0.8:
			acc = Mix(acc, RGB(0, 0, 0), 0.35)
		}
		t.Ink = acc
		t.Wave = [3]Color{Mix(acc, t.Text, 0.3), acc, Mix(acc, t.Surface, 0.18)}
	case accent != AccentGradient:
		t.Wave = [3]Color{t.Ink, t.Ink, t.Ink}
	}
	t.Glow = t.Wave[1]
	t.OnInk = RGB(255, 255, 255)
	if luminance(t.Ink) > 0.55 {
		t.OnInk = RGB(14, 14, 16)
	}
	if p, ok := ParseHex(preview); ok {
		t.Preview = p.WithAlpha(0.95)
	}
	return t
}

// waveColor returns the waveform color at pos (0 top, 1 bottom).
func (t Theme) waveColor(pos float32) Color {
	if pos < 0.5 {
		return Mix(t.Wave[0], t.Wave[1], pos*2)
	}
	return Mix(t.Wave[1], t.Wave[2], (pos-0.5)*2)
}

// Tone returns the color for a status style.
func (t Theme) Tone(style string) Color {
	switch style {
	case "ok":
		return t.Success
	case "warn":
		return t.Warning
	case "error":
		return t.Error
	}
	return t.Ink
}
