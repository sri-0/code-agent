// Package runtime defines the abstraction over the four runtime modes
// (local, ephemeral, persistent, shared) for spawning and talking to
// OpenCode workers.
//
// Each concrete runtime lives in a subpackage (local/, ephemeral/, ...) and
// registers itself via Register in init(). The orchestrator builds one
// instance per runtime config name from runtime.yaml.
package runtime

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	"code-agent/internal/config"
	"code-agent/pkg/opencode"
	"code-agent/pkg/tasks"
	"code-agent/pkg/transcript"
)

// Runtime is the per-mode worker manager.
type Runtime interface {
	// Mode returns "local" | "ephemeral" | "persistent" | "shared".
	Mode() string

	// EnsureWorker returns an opencode client connected to a worker capable
	// of executing actions for the given task. The runtime is responsible
	// for spawning, reusing, or routing — the caller doesn't care which.
	EnsureWorker(ctx context.Context, t *tasks.Task, b config.Board) (*opencode.Client, *tasks.WorkerRef, error)

	// EnsureSession creates or reuses an opencode session and returns its id.
	EnsureSession(ctx context.Context, client *opencode.Client, t *tasks.Task) (string, error)

	// Cleanup releases any resources tied to a task (e.g. tear down a
	// per-task Deployment in persistent mode). Local mode is a no-op.
	Cleanup(ctx context.Context, t *tasks.Task) error
}

// Deps is what every runtime needs.
type Deps struct {
	Logger     zerolog.Logger
	Transcript transcript.Store
	// OpenCode is the opencode.json content to inject into worker pods
	// (ephemeral/persistent/shared). Local runtime ignores it — the host
	// opencode has its own config file. Nil means cmd/runner will fall
	// back to its minimal default. API keys live inline inside this
	// config (e.g. provider.openrouter.options.apiKey), so no separate
	// env forwarding is needed.
	OpenCode config.OpenCodeConfig
	// Git is the identity used by cmd/runner when it makes commits on
	// behalf of the agent. Applied via `git config --global` at pod
	// startup. Empty fields fall back to runner-side defaults.
	Git config.GitIdentity
}

// Factory builds a Runtime instance from a named runtime config entry.
type Factory func(name string, cfg config.Runtime, deps Deps) (Runtime, error)

var registry = map[string]Factory{}

// Register a runtime factory under its mode name (e.g. "local").
func Register(mode string, f Factory) {
	registry[mode] = f
}

// Build constructs a runtime instance from a runtime config entry.
func Build(name string, cfg config.Runtime, deps Deps) (Runtime, error) {
	f, ok := registry[cfg.Mode]
	if !ok {
		return nil, fmt.Errorf("unknown runtime mode %q", cfg.Mode)
	}
	return f(name, cfg, deps)
}
