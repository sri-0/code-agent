// Package bootstrap performs shared initialization for the orchestrator's
// CLI entry points (cmd/server, cmd/cli). It loads config, sets up the logger,
// connects to storage, and returns a Result ready for wiring.
package bootstrap

import (
	"context"
	"fmt"

	"code-agent/internal/config"
	"code-agent/pkg/db/valkey"
	"code-agent/pkg/logging"
	"code-agent/pkg/tasks"
	tasksvalkey "code-agent/pkg/tasks/valkey"

	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
)

// Result holds everything produced by Init.
type Result struct {
	Cfg    *config.Config
	Logger zerolog.Logger
	Valkey *valkey.Valkey
	Tasks  tasks.Store
}

// Init loads env + YAML config, initialises logging, connects to valkey and
// constructs the task store. Missing optional YAML files are reported via a
// warning and left nil.
func Init(ctx context.Context) (*Result, error) {
	_ = godotenv.Load()

	cfg, err := config.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load env config: %w", err)
	}

	logger := logging.Setup(cfg.LogLevel, cfg.LogJSON)
	ctx = logger.WithContext(ctx)

	logger.Info().
		Str("config_dir", cfg.ConfigDir).
		Str("runtime_mode", cfg.RuntimeMode).
		Msg("starting code-agent")

	if err := config.LoadYAML(cfg); err != nil {
		return nil, fmt.Errorf("load yaml config: %w", err)
	}

	if cfg.Boards != nil {
		logger.Info().Int("boards", len(cfg.Boards.Boards)).Msg("boards config loaded")
	}
	if cfg.Stages != nil {
		logger.Info().Int("stage_sets", len(cfg.Stages.StageSets)).Msg("stages config loaded")
	}
	if cfg.Runtimes != nil {
		logger.Info().Int("runtimes", len(cfg.Runtimes.Runtimes)).Msg("runtimes config loaded")
	}

	var vk *valkey.Valkey
	var taskStore tasks.Store
	if cfg.Valkey != nil {
		v, err := valkey.New(ctx, *cfg.Valkey)
		if err != nil {
			logger.Warn().Err(err).Msg("valkey not reachable; task store disabled")
		} else {
			vk = v
			taskStore = tasksvalkey.New(vk)
		}
	}

	return &Result{
		Cfg:    cfg,
		Logger: logger,
		Valkey: vk,
		Tasks:  taskStore,
	}, nil
}
