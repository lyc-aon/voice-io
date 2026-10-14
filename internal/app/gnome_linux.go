//go:build linux

package app

import (
	"gitlab.com/integral-inference/voice-io/internal/config"
	"gitlab.com/integral-inference/voice-io/internal/platform"
)

func applyGNOME(keys config.ParsedHotkeys, executable string) error {
	return platform.ApplyGNOMEShortcuts(keys, executable)
}
