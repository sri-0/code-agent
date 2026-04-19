// Package local implements runtime.Runtime against an opencode server the
// developer is already running on the host (typically `opencode serve` on
// port 4096). It does not spawn anything — it just talks to the existing
// process.
//
// Use for `make dev-local`: no docker, no k8s, fast iteration.
package local

import (
	"context"
	"fmt"
	"sync"

	"github.com/rs/zerolog"

	"code-agent/internal/config"
	"code-agent/pkg/opencode"
	"code-agent/pkg/runtime"
	"code-agent/pkg/tasks"
	"code-agent/pkg/transcript"
)

type Runtime struct {
	name   string
	cfg    config.Runtime
	logger zerolog.Logger
	tx     transcript.Store

	mu     sync.Mutex
	client *opencode.Client
	// sessions maps task id -> opencode session id.
	sessions map[string]string
}

func (r *Runtime) Mode() string { return "local" }

// EnsureWorker returns the shared client (we only ever talk to one host
// opencode in local mode).
func (r *Runtime) EnsureWorker(ctx context.Context, t *tasks.Task, _ config.Board) (*opencode.Client, *tasks.WorkerRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client == nil {
		if r.cfg.OpenCodeURL == "" {
			return nil, nil, fmt.Errorf("local runtime %q: opencode_url not set", r.name)
		}
		r.client = opencode.New(opencode.Options{
			BaseURL:  r.cfg.OpenCodeURL,
			Password: r.cfg.OpenCodePassword.Value(),
		}, r.logger)
		if err := r.client.Health(ctx); err != nil {
			r.client = nil
			return nil, nil, fmt.Errorf("local runtime: opencode not reachable at %s: %w", r.cfg.OpenCodeURL, err)
		}
		r.logger.Info().Str("url", r.cfg.OpenCodeURL).Msg("local runtime: connected to opencode")

		// Best-effort: ensure opencode's current worktree is a git repo, so
		// its project picker assigns a real hash id. If the result is still
		// "global" the user needs to restart opencode for the new project to
		// be registered (this is an opencode startup quirk, not fixable via
		// API). Non-fatal — we warn and continue.
		if p, err := r.client.EnsureGitProject(ctx); err != nil {
			r.logger.Warn().Err(err).Msg("local runtime: could not init project; sessions may be orphaned")
		} else {
			if p.ID == "global" {
				r.logger.Warn().
					Str("worktree", p.Worktree).
					Msg("local runtime: opencode project is still 'global' — sessions won't show in the UI's project picker. Make sure opencode was started inside an existing git repo (with at least one commit).")
			} else {
				r.logger.Info().
					Str("project_id", p.ID).
					Str("worktree", p.Worktree).
					Msg("local runtime: opencode project ready")
			}
		}
	}
	ref := &tasks.WorkerRef{
		Mode:     "local",
		URL:      r.cfg.OpenCodeURL,
		UIURL:    r.cfg.OpenCodeURL,
		Password: r.cfg.OpenCodePassword.Value(),
	}
	return r.client, ref, nil
}

// EnsureSession creates a new opencode session if the task doesn't have one
// yet, otherwise returns the cached session id.
func (r *Runtime) EnsureSession(ctx context.Context, client *opencode.Client, t *tasks.Task) (string, error) {
	r.mu.Lock()
	if sid, ok := r.sessions[t.ID]; ok {
		r.mu.Unlock()
		return sid, nil
	}
	r.mu.Unlock()

	if t.SessionID != "" {
		// task remembers a session from a previous run; trust it
		r.mu.Lock()
		r.sessions[t.ID] = t.SessionID
		r.mu.Unlock()
		return t.SessionID, nil
	}

	title := t.ExternalID
	if t.Title != "" {
		title = t.ExternalID + " " + t.Title
	}
	s, err := client.CreateSession(ctx, title)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	r.sessions[t.ID] = s.ID
	r.mu.Unlock()

	if r.tx != nil {
		_ = r.tx.Init(ctx, transcript.Meta{
			SessionID: s.ID,
			TaskID:    t.ID,
			BoardID:   t.BoardID,
		})
	}
	return s.ID, nil
}

func (r *Runtime) Cleanup(_ context.Context, _ *tasks.Task) error { return nil }

func init() {
	runtime.Register("local", func(name string, cfg config.Runtime, deps runtime.Deps) (runtime.Runtime, error) {
		return &Runtime{
			name:     name,
			cfg:      cfg,
			logger:   deps.Logger.With().Str("runtime", "local").Str("name", name).Logger(),
			tx:       deps.Transcript,
			sessions: map[string]string{},
		}, nil
	})
}
