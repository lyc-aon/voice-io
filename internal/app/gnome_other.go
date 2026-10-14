//go:build !linux

package app

import (
	"gitlab.com/integral-inference/voice-io/internal/config"
	"gitlab.com/integral-inference/voice-io/internal/platform"
)

func applyGNOME(config.ParsedHotkeys, string) error { return platform.ErrUnsupported }
