// Package reactions converts lifecycle transitions into auto-actions.
// Mirrors agent-orchestrator's lifecycle-manager reaction system but with
// a Go-shaped interface and pluggable Action implementations.
//
// Flow:
//  1. Lifecycle poller emits prev→next snapshot via onTransition.
//  2. DeriveEvents(prev, next) yields zero or more Events (one per
//     reaction key that should fire — ci-failed, changes-requested,
//     approved-and-green, agent-stuck).
//  3. Engine.Process(evt) checks the per-task ReactionLedger for an
//     existing fire on the same evidence within the cooldown window;
//     either skips (already handled) or invokes the Action.
//  4. Each Action records its outcome (firing time + evidence hash) on
//     the task ledger so future ticks dedupe correctly.
//
// Decisions are pure config-driven — *no* LLM in the routing layer. The
// LLM is delegated to inside Action.Run (e.g. send-to-agent's prompt).
package reactions

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"code-agent/internal/config"
	"code-agent/pkg/notifications"
	"code-agent/pkg/tasks"
)

// EventKey is the reaction id; matches the boards.yaml reactions map keys.
type EventKey string

const (
	EventCIFailed         EventKey = "ci-failed"
	EventChangesRequested EventKey = "changes-requested"
	EventApprovedAndGreen EventKey = "approved-and-green"
	EventAgentStuck       EventKey = "agent-stuck"
	EventPRMerged         EventKey = "pr-merged"
)

// Event is what the engine processes. Carries the task + the lifecycle
// snapshot that triggered the fire so Actions have full context without
// re-reading from store.
type Event struct {
	Key   EventKey
	Task  *tasks.Task
	Board config.Board
	// Prev / Next are the lifecycle snapshots. Useful for actions that
	// want to know "what changed" (e.g. CI moved from passing→failing).
	Prev, Next tasks.Lifecycle
	// EvidenceHash is computed by DeriveEvents — short stable id for the
	// triggering signal so the ledger can dedupe re-fires on the same
	// evidence (e.g. same failing CI run).
	EvidenceHash string
}

// Action implements one auto-response. Run is invoked synchronously by
// the engine; long-running actions (e.g. send-to-agent) should manage
// their own goroutines internally.
type Action interface {
	Name() string // "send-to-agent" | "notify" | "auto-merge"
	Run(ctx context.Context, e Event) error
}

// Deps bundles everything actions need at construction time.
type Deps struct {
	Tasks         tasks.Store
	Notifications notifications.Router
	Logger        zerolog.Logger
}

// Engine wires reactions config + ledger + actions.
type Engine struct {
	logger        zerolog.Logger
	store         tasks.Store
	router        notifications.Router
	actions       map[string]Action
	ledgerCooldown time.Duration
}

// NewEngine creates an engine; pass actions in via Register.
func NewEngine(deps Deps) *Engine {
	return &Engine{
		logger:         deps.Logger.With().Str("component", "reactions").Logger(),
		store:          deps.Tasks,
		router:         deps.Notifications,
		actions:        map[string]Action{},
		ledgerCooldown: 5 * time.Minute,
	}
}

// Register an action under its name.
func (e *Engine) Register(a Action) { e.actions[a.Name()] = a }
