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
	"time"

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
	// Tasks is the orchestrator's task store. The persistent runtime's
	// GC goroutine uses it to iterate live tasks when deciding which pods
	// to age off. Nil is tolerated — runtimes that don't GC (local,
	// shared, ephemeral) ignore it.
	Tasks tasks.Store
	// Boards is the static board config. Persistent-runtime GC needs it
	// to build a VCS client for refreshing MR state on each scan. Nil is
	// tolerated but disables MR-state-based TTLs.
	Boards *config.BoardsConfig
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
	// TTLAfterMRClosed / TTLMax / GCInterval are only meaningful for
	// the persistent runtime (others ignore). Zero values fall back to
	// the runtime's own sane defaults.
	TTLAfterMRClosed time.Duration
	TTLMax           time.Duration
	GCInterval       time.Duration
}

// GCCapable is implemented by runtimes that need a background
// garbage-collection goroutine (currently only persistent). The
// orchestrator calls StartGC once on startup with the server's lifetime
// context; the goroutine exits when ctx is cancelled.
type GCCapable interface {
	StartGC(ctx context.Context)
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
