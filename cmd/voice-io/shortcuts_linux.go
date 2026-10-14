//go:build linux

package main

import (
	"gitlab.com/integral-inference/voice-io/internal/config"
	"gitlab.com/integral-inference/voice-io/internal/platform"
)

func applyShortcuts(keys config.ParsedHotkeys, exe string) error {
	return platform.ApplyGNOMEShortcuts(keys, exe)
}

func removeShortcuts() error { return platform.RemoveGNOMEShortcuts() }
