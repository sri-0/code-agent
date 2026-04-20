// Package config loads environment + YAML configuration for the orchestrator.
package config

import (
	"context"
	"fmt"
	"path/filepath"

	"code-agent/pkg/db/valkey"

	"github.com/sethvargo/go-envconfig"
)

// Config holds all runtime-level configuration. Everything that may vary per
// environment lives here. The *Config pointer fields below are loaded from
// YAML files under ConfigDir and attached after envconfig.Process runs.
type Config struct {
	Port      int    `env:"PORT,default=8080"`
	Host      string `env:"HOST,default=0.0.0.0"`
	AppName   string `env:"APP_NAME,default=code-agent"`
	ConfigDir string `env:"CONFIG_DIR,default=config/default"`
	LogLevel  string `env:"LOG_LEVEL,default=info"`
	LogJSON   bool   `env:"LOG_JSON,default=false"`

	// RUNTIME_MODE forces a specific runtime mode globally. When empty, the
	// per-board/per-stage runtime_ref in boards.yaml/stages.yaml is used.
	// Useful for `make dev-local` which sets RUNTIME_MODE=local.
	RuntimeMode string `env:"RUNTIME_MODE"`

	// Valkey / Redis connection for task store, transcript store, dedupe.
	Valkey *valkey.Config `env:",noinit"`

	// OpenCode connection for `local` runtime mode.
	OpenCodeURL      string `env:"OPENCODE_URL,default=http://localhost:4096"`
	OpenCodePassword string `env:"OPENCODE_SERVER_PASSWORD"`

	// SkillsDir points at the host directory holding Anthropic-style skill
	// bundles (each a subdir with SKILL.md + supporting files). Bundle
	// names referenced from boards.yaml / stages.yaml resolve here. Empty
	// disables skills.
	SkillsDir string `env:"CODE_AGENT_SKILLS_DIR"`

	// Attached after YAML load. No env tag — envconfig leaves them alone.
	Boards   *BoardsConfig
	Stages   *StagesConfig
	Runtimes *RuntimesConfig
	OpenCode OpenCodeConfig
	Skills   *SkillsIndex
}

// Load reads env vars; call LoadYAML afterwards to populate the YAML fields.
func Load(ctx context.Context) (*Config, error) {
	var cfg Config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return nil, fmt.Errorf("envconfig: %w", err)
	}
	return &cfg, nil
}

// LoadYAML populates the YAML-derived fields from the given ConfigDir.
// Missing optional files are ignored but logged by the caller.
func LoadYAML(cfg *Config) error {
	dir := cfg.ConfigDir

	if b, err := LoadBoards(filepath.Join(dir, "boards.yaml")); err != nil {
		return fmt.Errorf("boards.yaml: %w", err)
	} else {
		cfg.Boards = b
	}

	if s, err := LoadStages(filepath.Join(dir, "stages.yaml")); err != nil {
		return fmt.Errorf("stages.yaml: %w", err)
	} else {
		cfg.Stages = s
	}

	if r, err := LoadRuntimes(filepath.Join(dir, "runtime.yaml")); err != nil {
		return fmt.Errorf("runtime.yaml: %w", err)
	} else {
		cfg.Runtimes = r
	}

	if o, err := LoadOpenCode(filepath.Join(dir, "opencode.yaml")); err != nil {
		return fmt.Errorf("opencode.yaml: %w", err)
	} else {
		cfg.OpenCode = o
	}

	if sk, err := LoadSkills(cfg.SkillsDir); err != nil {
		return fmt.Errorf("skills: %w", err)
	} else {
		cfg.Skills = sk
	}

	return nil
}
