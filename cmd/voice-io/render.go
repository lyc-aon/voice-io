package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"gitlab.com/integral-inference/voice-io/internal/app"
	"gitlab.com/integral-inference/voice-io/internal/config"
	"gitlab.com/integral-inference/voice-io/internal/overlay"
	"gitlab.com/integral-inference/voice-io/internal/ui"
)

// cmdRenderOverlay renders every overlay state to PNG files without opening
// a window, for design review and diagnostics.
func cmdRenderOverlay(args []string) error {
	fs := flag.NewFlagSet("render-overlay", flag.ContinueOnError)
	out := fs.String("out", "overlay-preview", "output directory")
	theme := fs.String("theme", "both", "dark, light or both")
	scale := fs.Float64("scale", 2, "pixel scale")
	only := fs.String("state", "", "render only this state")
	if err := fs.Parse(args); err != nil {
		return exitCode(2)
	}
	cfg, err := config.Load("")
	if err != nil {
		cfg = config.Default()
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	themes := []string{"dark", "light"}
	if *theme != "both" {
		themes = []string{*theme}
	}
	oc := app.OverlayConfig(cfg)
	var list overlay.List
	for _, th := range themes {
		dark := th == "dark"
		oc.Theme = th
		backdrop := overlay.RGB(0xd4, 0xd8, 0xe0)
		if dark {
			backdrop = overlay.RGB(0x2a, 0x2f, 0x3a)
		}
		for _, s := range overlay.Scenarios() {
			if *only != "" && s.Name != *only {
				continue
			}
			overlay.RenderScenario(s, oc, dark, ui.Measurer{}, &list)
			path := filepath.Join(*out, fmt.Sprintf("%s-%s.png", s.Name, th))
			if err := ui.RenderPNG(&list, overlay.CanvasW, overlay.CanvasH, *scale, backdrop, path); err != nil {
				return err
			}
			fmt.Println(path)
		}
	}
	return nil
}
