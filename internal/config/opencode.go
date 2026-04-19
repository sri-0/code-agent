package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// OpenCodeConfig is the base template plus per-stage overrides the orchestrator
// merges when rendering the final opencode.json that gets mounted into a worker.
type OpenCodeConfig struct {
	Default   OpenCodeSpec            `yaml:"default"`
	Overrides map[string]OpenCodeSpec `yaml:"overrides"`
}

// OpenCodeSpec is a partial; unset fields inherit from Default during merge.
type OpenCodeSpec struct {
	Permissions map[string]string      `yaml:"permissions,omitempty"`
	Tools       *OpenCodeTools         `yaml:"tools,omitempty"`
	Folders     *OpenCodeFolders       `yaml:"folders,omitempty"`
	Providers   map[string]any         `yaml:"providers,omitempty"`
	MCP         map[string]OpenCodeMCP `yaml:"mcp,omitempty"`
}

type OpenCodeTools struct {
	Enabled  []string `yaml:"enabled,omitempty"`
	Disabled []string `yaml:"disabled,omitempty"`
}

type OpenCodeFolders struct {
	Allowed []string `yaml:"allowed,omitempty"`
	Denied  []string `yaml:"denied,omitempty"`
}

type OpenCodeMCP struct {
	Command []string          `yaml:"command,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"`
}

func LoadOpenCode(path string) (*OpenCodeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg OpenCodeConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	return &cfg, nil
}
