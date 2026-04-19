package config

import (
	"encoding/json"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// OpenCodeConfig is a free-form map that mirrors opencode's own config
// schema 1:1 (https://opencode.ai/config.json). We ship it to worker pods
// via the CODE_AGENT_OPENCODE_CONFIG env var; cmd/runner writes it to
// /workspace/opencode.json before launching opencode.
//
// Keeping this untyped avoids the constant drift between our Go types
// and opencode's rapidly-changing schema — the YAML is the opencode
// config, nothing more.
type OpenCodeConfig map[string]any

// LoadOpenCode reads a YAML file and returns it as a free-form map. If
// the file is missing, returns nil, nil (opencode config is optional for
// local runtime; required for pod-based runtimes).
func LoadOpenCode(path string) (OpenCodeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cfg OpenCodeConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	return cfg, nil
}

// JSON returns the config as a compact JSON byte slice suitable for
// injection via env var.
func (c OpenCodeConfig) JSON() ([]byte, error) {
	if c == nil {
		return nil, nil
	}
	return json.Marshal(c)
}
