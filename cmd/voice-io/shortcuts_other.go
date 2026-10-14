//go:build !linux

package main

import (
	"gitlab.com/integral-inference/voice-io/internal/config"
	"gitlab.com/integral-inference/voice-io/internal/platform"
)

func applyShortcuts(config.ParsedHotkeys, string) error { return platform.ErrUnsupported }

func removeShortcuts() error { return platform.ErrUnsupported }
