package app

import (
	"bytes"

	"github.com/BurntSushi/toml"

	"gitlab.com/integral-inference/voice-io/internal/config"
)

// configMap presents a configuration with its TOML key names, so Settings
// reads and writes the same names people see in config.toml.
func configMap(cfg *config.Config) map[string]any {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		return map[string]any{}
	}
	out := map[string]any{}
	if _, err := toml.Decode(buf.String(), &out); err != nil {
		return map[string]any{}
	}
	return out
}
