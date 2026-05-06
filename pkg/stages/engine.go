// Package stages is the configurable state-machine engine that drives a
// task through provider-statuses (e.g. todo -> in_progress -> ready_for_qa).
//
// The engine is intentionally decoupled from runtimes and providers. It owns:
//   - choosing the next stage from a Stage's success/failure_next or Next
//   - asking the provider to transition the ticket
//   - delegating action execution to a registered ActionRunner
//
// Phase 3 ships engine + ActionRunner registration; concrete runners (run_agent,
// open_mr, ...) come in later phases as their dependencies materialise.
package stages

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"code-agent/internal/config"
	"code-agent/pkg/metrics"
	"code-agent/pkg/providers"
	"code-agent/pkg/tasks"
)

// ActionRunner executes a single stage action. Returning ErrAsync signals
// the engine that the action runs asynchronously and the engine should not
// auto-advance the task — the runner will call Engine.Advance later.
type ActionRunner interface {
	Run(ctx context.Context, in ActionInput) (ActionResult, error)
}

type ActionInput struct {
	Task     *tasks.Task
	Stage    config.Stage
	Board    config.Board
	Provider providers.BoardProvider
	Logger   zerolog.Logger
}

type ActionResult struct {
	// Outcome is "success" | "failure" | "async". Drives next-stage selection.
	Outcome string
	// Comment, if non-empty, is posted to the ticket.
	Comment string
	// Mutate, if non-nil, is called with a writable copy of the task before
	// it is persisted (used to record SessionID, RepoState updates, MRs).
	Mutate func(t *tasks.Task)
}

// ErrAsync sentinel — runners can return it instead of building ActionResult{Outcome: "async"}.
var ErrAsync = fmt.Errorf("async")

type Engine struct {
	cfg    *config.Config
	logger zerolog.Logger
	store  tasks.Store

	mu      sync.RWMutex
	runners map[string]ActionRunner

	// inFlight tracks tasks that have a running action so repeated poll
	// events don't start a parallel run. In-memory: cleared on restart,
	// which is the right behaviour (a crashed orchestrator forgets its
	// locks and re-tries the task from scratch).
	flightMu sync.Mutex
	inFlight map[string]struct{}
}

func NewEngine(cfg *config.Config, logger zerolog.Logger, store tasks.Store) *Engine {
	return &Engine{
		cfg:      cfg,
		logger:   logger.With().Str("component", "stages").Logger(),
		store:    store,
		runners:  map[string]ActionRunner{},
		inFlight: map[string]struct{}{},
	}
}

// tryAcquire marks a (task, stage) pair as running; returns false if the
// same pair is already in flight. Keyed on task+stage (not just task) so
// legitimate cascades — implement succeeding and calling open_mr within
// the same call stack — don't self-deadlock.
func (e *Engine) tryAcquire(taskID, stage string) bool {
	key := taskID + "/" + stage
	e.flightMu.Lock()
	defer e.flightMu.Unlock()
	if _, busy := e.inFlight[key]; busy {
		return false
	}
	e.inFlight[key] = struct{}{}
	return true
}

func (e *Engine) release(taskID, stage string) {
	key := taskID + "/" + stage
	e.flightMu.Lock()
	defer e.flightMu.Unlock()
	delete(e.inFlight, key)
}

// Register an action runner under a name (matches Stage.Action).
func (e *Engine) Register(action string, r ActionRunner) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.runners[action] = r
}

// stageSetForBoard resolves the Stage list a board uses.
func (e *Engine) stageSetForBoard(b config.Board) ([]config.Stage, error) {
	if e.cfg.Stages == nil {
		return nil, fmt.Errorf("no stages config loaded")
	}
	set, ok := e.cfg.Stages.Set(b.StagesRef)
	if !ok {
		return nil, fmt.Errorf("board %s: stage set %q not found", b.ID, b.StagesRef)
	}
	return set, nil
}

// stageByName looks up by name within a set.
func stageByName(set []config.Stage, name string) (config.Stage, bool) {
	for _, s := range set {
		if s.Name == name {
			return s, true
		}
	}
	return config.Stage{}, false
}

// stageByProviderStatus does the same but matches provider_status.
func stageByProviderStatus(set []config.Stage, status string) (config.Stage, bool) {
	for _, s := range set {
		if s.ProviderStatus == status {
			return s, true
		}
	}
	return config.Stage{}, false
}

// HandleEvent is the entry point from the orchestrator: a provider event
// arrives, we look up the matching stage, run its action, then transition
// the ticket and persist.
func (e *Engine) HandleEvent(ctx context.Context, evt providers.Event, board config.Board, prov providers.BoardProvider) error {
	set, err := e.stageSetForBoard(board)
	if err != nil {
		return err
	}
	stg, ok := stageByProviderStatus(set, evt.Ticket.Status)
	if !ok {
		e.logger.Debug().Str("status", evt.Ticket.Status).Str("board", board.ID).Msg("no stage matches status; ignoring")
		return nil
	}

	t, err := e.loadOrCreateTask(ctx, board, evt, stg)
	if err != nil {
		return err
	}

	// Ensure the canonical lifecycle is populated for tasks that pre-date
	// the multi-track refactor; from here on all writers can rely on it.
	tasks.EnsureLifecycle(t, time.Now())

	// If the task is already in this stage and was recently processed,
	// dedupe at the task level (covers webhook+poll overlap that escaped
	// the dispatcher cache).
	if t.Stage == stg.Name && time.Since(t.LastSync) < 5*time.Second {
		return nil
	}
	// Skip if this stage was already run to completion. Covers every kind
	// of re-entry: terminal-stage self-trigger (description mutation
	// bumps updated_at), non-terminal stage re-firing because a new
	// event arrived at the same provider_status after we cascaded past.
	// Each stage runs at most once per task.
	for _, done := range t.CompletedStages {
		if done == stg.Name {
			e.logger.Debug().Str("task", t.ID).Str("stage", stg.Name).Msg("stage already completed; skipping re-entry")
			return nil
		}
	}

	e.logger.Info().Str("task", t.ID).Str("ext_id", t.ExternalID).Str("stage", stg.Name).Str("action", stg.Action).Msg("entering stage")
	t.Stage = stg.Name
	t.StageStarted = time.Now()
	t.LastEventKey = evt.Key
	t.LastSync = time.Now()
	t.UpdatedAt = time.Now()
	// Mirror onto the canonical lifecycle: entering any non-terminal stage
	// means the agent is working again. Reason is the stage name itself so
	// the UI can render a useful subtitle ("working: implement").
	now := t.UpdatedAt
	t.Lifecycle.Session.State = tasks.SessionStateWorking
	t.Lifecycle.Session.Reason = tasks.SessionReason(stg.Name)
	t.Lifecycle.Session.LastTransitionAt = &now
	if t.Lifecycle.Session.StartedAt == nil {
		t.Lifecycle.Session.StartedAt = &now
	}
	if err := e.store.Update(ctx, t); err != nil {
		return fmt.Errorf("save before action: %w", err)
	}

	return e.runAction(ctx, t, board, stg, set, prov)
}

func (e *Engine) loadOrCreateTask(ctx context.Context, b config.Board, evt providers.Event, stg config.Stage) (*tasks.Task, error) {
	t, err := e.store.GetByExternal(ctx, b.ID, evt.Ticket.ExternalID)
	if err == nil && t != nil {
		t.Title = evt.Ticket.Title
		t.Description = evt.Ticket.Description
		t.URL = evt.Ticket.URL
		return t, nil
	}
	if err != nil && !errors.Is(err, tasks.ErrNotFound) {
		return nil, err
	}
	now := time.Now()
	t = &tasks.Task{
		ID:           generateID(),
		BoardID:      b.ID,
		ExternalID:   evt.Ticket.ExternalID,
		Title:        evt.Ticket.Title,
		Description:  evt.Ticket.Description,
		URL:          evt.Ticket.URL,
		Stage:        stg.Name,
		StageStarted: now,
		RuntimeMode:  b.RuntimeRef, // resolved to a real mode at action time
		CreatedAt:    now,
		UpdatedAt:    now,
		Lifecycle:    tasks.SynthesizeLifecycle(&tasks.Task{Stage: stg.Name, StageStarted: now}, now),
	}
	for _, r := range b.Repos {
		t.Repos = append(t.Repos, tasks.RepoState{
			Name:       r.Name,
			URL:        r.URL,
			BaseBranch: r.BaseBranch,
			Branch:     b.BranchPrefix + evt.Ticket.ExternalID,
		})
	}
	if err := e.store.Create(ctx, t); err != nil {
		return nil, err
	}
	return t, nil
}

func (e *Engine) runAction(ctx context.Context, t *tasks.Task, b config.Board, stg config.Stage, set []config.Stage, prov providers.BoardProvider) error {
	if stg.Action == "" || stg.Action == "noop" {
		return e.advance(ctx, t, b, stg, set, prov, "success")
	}

	e.mu.RLock()
	r, ok := e.runners[stg.Action]
	e.mu.RUnlock()
	if !ok {
		e.logger.Warn().Str("action", stg.Action).Msg("no runner registered; treating as noop")
		return e.advance(ctx, t, b, stg, set, prov, "success")
	}

	// Prevent concurrent runs of the same task: a re-entered poll event
	// while an action is already running would start a parallel (and
	// destructive — sync wipes files) session.
	if !e.tryAcquire(t.ID, stg.Name) {
		e.logger.Debug().Str("task", t.ID).Str("stage", stg.Name).Msg("action already in flight; skipping re-entry")
		return nil
	}
	defer e.release(t.ID, stg.Name)

	timeout := stg.Timeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	actCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	actStart := time.Now()
	res, err := r.Run(actCtx, ActionInput{
		Task:     t,
		Stage:    stg,
		Board:    b,
		Provider: prov,
		Logger:   e.logger.With().Str("task", t.ID).Str("stage", stg.Name).Logger(),
	})
	metrics.ActionDurationSeconds.WithLabelValues(stg.Action).Observe(time.Since(actStart).Seconds())
	outcomeLabel := res.Outcome
	if outcomeLabel == "" {
		outcomeLabel = "unknown"
	}
	metrics.ActionRunsTotal.WithLabelValues(stg.Action, outcomeLabel).Inc()
	if err == ErrAsync || res.Outcome == "async" {
		return nil // runner will call Advance later
	}
	if err != nil {
		e.logger.Error().Err(err).Str("action", stg.Action).Msg("action failed")
		if res.Outcome == "" {
			res.Outcome = "failure"
		}
	}
	if res.Mutate != nil {
		res.Mutate(t)
	}
	if res.Comment != "" {
		_ = prov.Comment(ctx, t.ExternalID, providers.Comment{Body: res.Comment})
	}
	// Record the completion — terminal or not. Every stage runs at most
	// once per task.
	t.CompletedStages = append(t.CompletedStages, stg.Name)
	t.UpdatedAt = time.Now()
	if err := e.store.Update(ctx, t); err != nil {
		return fmt.Errorf("save after action: %w", err)
	}
	return e.advance(ctx, t, b, stg, set, prov, res.Outcome)
}

// Advance is the public entry point an async runner calls when it has finished.
func (e *Engine) Advance(ctx context.Context, taskID, outcome string) error {
	t, err := e.store.Get(ctx, taskID)
	if err != nil || t == nil {
		return fmt.Errorf("advance: get task %s: %w", taskID, err)
	}
	b, ok := e.cfg.Boards.ByID(t.BoardID)
	if !ok {
		return fmt.Errorf("board %s not found", t.BoardID)
	}
	set, err := e.stageSetForBoard(b)
	if err != nil {
		return err
	}
	stg, ok := stageByName(set, t.Stage)
	if !ok {
		return fmt.Errorf("stage %s not found in set %s", t.Stage, b.StagesRef)
	}
	return e.advance(ctx, t, b, stg, set, nil, outcome)
}

func (e *Engine) advance(ctx context.Context, t *tasks.Task, b config.Board, stg config.Stage, set []config.Stage, prov providers.BoardProvider, outcome string) error {
	if stg.Terminal {
		return nil
	}
	// Human-review gate: on success, park the task. Don't auto-transition
	// the ticket; a human must move it forward manually. Failures still
	// route to failure_next so errors aren't silenced.
	if outcome == "success" && stg.HumanReview {
		e.logger.Info().Str("task", t.ID).Str("stage", stg.Name).Msg("stage complete; human review required — parking")
		return nil
	}
	var nextName string
	switch outcome {
	case "failure":
		nextName = stg.FailureNext
	default:
		nextName = stg.SuccessNext
		if nextName == "" {
			nextName = stg.Next
		}
	}
	if nextName == "" {
		return nil
	}
	next, ok := stageByName(set, nextName)
	if !ok {
		return fmt.Errorf("next stage %q not in set", nextName)
	}
	if next.ProviderStatus != "" && prov != nil {
		if err := prov.Transition(ctx, t.ExternalID, next.ProviderStatus); err != nil {
			e.logger.Error().Err(err).Str("status", next.ProviderStatus).Msg("transition failed")
			// keep going; we still flip our local stage so we don't loop
		}
	}
	metrics.StageTransitionsTotal.WithLabelValues(b.ID, stg.Name, next.Name, outcome).Inc()
	t.Stage = next.Name
	t.StageStarted = time.Now()
	t.UpdatedAt = time.Now()
	// Update the lifecycle session track on every transition. Terminal
	// stages handled above; here we always have a forward edge.
	now := t.UpdatedAt
	if next.Terminal {
		t.Lifecycle.Session.State = tasks.SessionStateDone
		t.Lifecycle.Session.Reason = tasks.SessionReasonResearchComplete
		t.Lifecycle.Session.CompletedAt = &now
	} else {
		t.Lifecycle.Session.State = tasks.SessionStateWorking
		t.Lifecycle.Session.Reason = tasks.SessionReason(next.Name)
	}
	t.Lifecycle.Session.LastTransitionAt = &now
	// Clear the cached opencode session id so the next stage starts with a
	// fresh session. Each stage has its own system prompt; we don't want
	// the plan-stage conversation leaking into the implement-stage turn.
	t.SessionID = ""
	// Don't clear CompletedStages — we keep the full history so any
	// re-entry of a stage already run is skipped, even after we've
	// advanced past it.
	if err := e.store.Update(ctx, t); err != nil {
		return err
	}
	// If the next stage has its own action and provider is available, run it.
	if prov != nil && next.Action != "" && next.Action != "noop" {
		return e.runAction(ctx, t, b, next, set, prov)
	}
	return nil
}

// generateID is a tiny uuid-ish helper. Using crypto/rand keeps deps small.
func generateID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	const hex = "0123456789abcdef"
	out := make([]byte, 32)
	for i, v := range b {
		out[i*2] = hex[v>>4]
		out[i*2+1] = hex[v&0x0f]
	}
	return string(out)
}
