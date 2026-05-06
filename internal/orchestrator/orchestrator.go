// Package orchestrator wires board providers, the event dispatcher, and the
// task store together. It owns the main event loop that turns provider events
// into stage transitions.
//
// Phase 2 implements only the provider side: events are consumed and logged.
// Phase 3+ will add the stage engine that drives runtimes.
package orchestrator

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"code-agent/internal/config"
	"code-agent/pkg/actions"
	reviewact "code-agent/pkg/actions/review"
	"code-agent/pkg/lifecycle"
	"code-agent/pkg/llm"
	"code-agent/pkg/notifications"
	"code-agent/pkg/providers"
	_ "code-agent/pkg/providers/all" // register providers
	"code-agent/pkg/reactions"
	reactionactions "code-agent/pkg/reactions/actions"
	"code-agent/pkg/review"
	"code-agent/pkg/runtime"
	_ "code-agent/pkg/runtime/all" // register runtimes
	"code-agent/pkg/stages"
	"code-agent/pkg/tasks"
	"code-agent/pkg/transcript"
	"code-agent/pkg/vcs"
	_ "code-agent/pkg/vcs/all" // register vcs providers
)

type Orchestrator struct {
	cfg             *config.Config
	logger          zerolog.Logger
	tasks           tasks.Store
	tx              transcript.Store
	disp            *providers.Dispatcher
	engine          *stages.Engine
	reviewPoller    *review.Poller
	lifecyclePoller *lifecycle.PRPoller
	reactionsEngine *reactions.Engine
	probePoller     *lifecycle.ProbePoller

	runtimes    map[string]runtime.Runtime
	agentRunner *runtime.AgentRunner

	// boardByID lets webhook handlers route to the right provider.
	mu        sync.RWMutex
	boardByID map[string]providers.BoardProvider
}

func New(cfg *config.Config, logger zerolog.Logger, taskStore tasks.Store, tx transcript.Store) (*Orchestrator, error) {
	o := &Orchestrator{
		cfg:       cfg,
		logger:    logger.With().Str("component", "orchestrator").Logger(),
		tasks:     taskStore,
		tx:        tx,
		disp:      providers.NewDispatcher(256, 5*time.Minute, logger),
		engine:    stages.NewEngine(cfg, logger, taskStore),
		runtimes:  map[string]runtime.Runtime{},
		boardByID: map[string]providers.BoardProvider{},
	}

	// Build runtimes (each name in runtime.yaml -> instance).
	if cfg.Runtimes != nil {
		for name, rcfg := range cfg.Runtimes.Runtimes {
			var git config.GitIdentity
			if cfg.Runtimes != nil {
				git = cfg.Runtimes.Git
			}
			rt, err := runtime.Build(name, rcfg, runtime.Deps{
				Logger:           o.logger,
				Transcript:       tx,
				Tasks:            taskStore,
				Boards:           cfg.Boards,
				OpenCode:         cfg.OpenCode,
				Git:              git,
				TTLAfterMRClosed: cfg.PodTTLAfterMRClosed,
				TTLMax:           cfg.PodTTLMax,
				GCInterval:       cfg.PodGCInterval,
			})
			if err != nil {
				o.logger.Error().Err(err).Str("runtime", name).Msg("failed to build runtime")
				continue
			}
			o.runtimes[name] = rt
			o.logger.Info().Str("runtime", name).Str("mode", rcfg.Mode).Msg("runtime registered")
		}
	}

	// Wire the run_agent action runner.
	o.agentRunner = runtime.NewAgentRunner(o.runtimes, tx, taskStore, cfg.Skills)
	o.engine.Register("run_agent", o.agentRunner)

	// Wire the open_mr action runner (stateless / lazy vcs client cache).
	o.engine.Register("open_mr", actions.NewOpenMRRunner())

	// Wire the PR-review feedback loop. The poller watches open MRs for
	// new review comments and hands each one to the review action
	// handler, which classifies (explain vs change) and drives opencode.
	llmClient, llmErr := llm.New(llm.Options{})
	if llmErr != nil {
		o.logger.Warn().Err(llmErr).Msg("LLM client unavailable; PR review loop disabled")
	}
	reviewHandler := &reviewact.Handler{
		Runtimes: o.runtimes,
		Tasks:    taskStore,
		LLM:      llmClient,
		Logger:   o.logger,
	}
	if llmClient != nil {
		o.reviewPoller = review.New(taskStore, cfg.Boards, cfg.PodGCInterval, o.logger, reviewHandler.Handle)
	}

	// Reactions engine: declarative event→action rules driven by lifecycle
	// transitions. Decisions are pure config (auto/retries/escalateAfter);
	// the LLM layer lives inside send-to-agent's prompt, not the routing.
	notifRouter := buildNotificationRouter(cfg, o.logger, func(boardID string) (providers.BoardProvider, bool) {
		return o.Provider(boardID)
	}, cfg.Boards.ByID)
	o.reactionsEngine = reactions.NewEngine(reactions.Deps{
		Tasks:         taskStore,
		Notifications: notifRouter,
		Logger:        o.logger,
	})
	o.reactionsEngine.Register(&reactionactions.Notify{Router: notifRouter, Logger: o.logger})
	o.reactionsEngine.Register(&reactionactions.AutoMerge{
		MergeOptionsForKey: func(b config.Board, _ reactions.EventKey) vcs.MergeOptions {
			method := "squash"
			if rc, ok := b.Reactions[string(reactions.EventApprovedAndGreen)]; ok && rc.Method != "" {
				method = rc.Method
			}
			return vcs.MergeOptions{Method: method}
		},
		Logger: o.logger,
	})
	// send-to-agent uses the review handler's RunReactiveTurn method
	// as the underlying TurnRunner — same code path as the comment-driven
	// review loop, just driven by a structured reaction prompt.
	o.reactionsEngine.Register(&reactionactions.SendToAgent{
		Runner: &reviewTurnAdapter{handler: reviewHandler, boards: cfg.Boards},
		Logger: o.logger,
	})

	// Shared transition handler: derive reaction events and feed the
	// reactions engine. Used by both the PR poller (PR/CI signals) and
	// the probe poller (runtime liveness signals).
	transitionFn := func(ctx context.Context, t *tasks.Task, prev, next tasks.Lifecycle) {
		b, ok := cfg.Boards.ByID(t.BoardID)
		if !ok {
			return
		}
		for _, key := range reactions.DeriveEvents(prev, next) {
			evidence := string(next.PR.Reason) + "|" + next.PR.URL + "|" + string(next.Session.State)
			o.reactionsEngine.Process(ctx, reactions.Event{
				Key:          key,
				Task:         t,
				Board:        b,
				Prev:         prev,
				Next:         next,
				EvidenceHash: reactions.EvidenceHash(key, evidence),
			})
		}
	}

	// Lifecycle PR poller — refreshes Lifecycle.PR.Reason from CI +
	// review state on every tick, then feeds prev/next snapshots into
	// the reactions engine.
	o.lifecyclePoller = lifecycle.NewPRPoller(taskStore, cfg.Boards, cfg.PodGCInterval, o.logger, transitionFn)

	// Probe poller — checks runtime liveness (opencode reachable?) and
	// applies the detecting → stuck escalation budget.
	o.probePoller = lifecycle.NewProbePoller(taskStore, cfg.Boards, nil, nil, cfg.PodGCInterval, o.logger, transitionFn)

	if cfg.Boards == nil {
		o.logger.Warn().Msg("no boards configured; orchestrator idle")
		return o, nil
	}
	for _, b := range cfg.Boards.Boards {
		p, err := providers.Build(b, o.logger, o.disp)
		if err != nil {
			o.logger.Error().Err(err).Str("board", b.ID).Msg("failed to build provider")
			continue
		}
		o.disp.Register(p)
		o.boardByID[b.ID] = p
		o.logger.Info().Str("board", b.ID).Str("provider", b.Provider).Msg("provider registered")
	}
	return o, nil
}

// reviewTurnAdapter wraps reviewact.Handler so it satisfies the
// reactions actions.TurnRunner interface. It just looks up the board
// for the task and forwards to RunReactiveTurn.
type reviewTurnAdapter struct {
	handler *reviewact.Handler
	boards  *config.BoardsConfig
}

func (a *reviewTurnAdapter) RunTurn(ctx context.Context, in reactionactions.TurnInput) error {
	if a.handler == nil {
		return nil
	}
	b, ok := a.boards.ByID(in.BoardID)
	if !ok {
		return nil
	}
	return a.handler.RunReactiveTurn(ctx, reviewact.ReactiveTurnInput{
		Task:         in.Task,
		Board:        b,
		Mode:         in.Mode,
		SystemPrompt: in.SystemPrompt,
		UserMessage:  in.UserMessage,
		PushOnEdit:   in.PushOnEdit,
	})
}

// buildNotificationRouter assembles a FanoutRouter from the configured
// backends (Mattermost, Teams, generic webhook, ticket). Backends are
// only added when their env vars are present, so a partially-configured
// deployment fan-outs only to what's wired.
func buildNotificationRouter(
	cfg *config.Config,
	logger zerolog.Logger,
	providerForBoard func(string) (providers.BoardProvider, bool),
	boardForID func(string) (config.Board, bool),
) notifications.Router {
	var backends []notifications.Notifier
	if cfg.MattermostBaseURL != "" && cfg.MattermostToken != "" && cfg.MattermostDefaultChannelID != "" {
		backends = append(backends, notifications.NewMattermost(
			cfg.MattermostBaseURL, cfg.MattermostToken, cfg.MattermostDefaultChannelID,
		))
	}
	if cfg.TeamsWebhookURL != "" {
		backends = append(backends, notifications.NewTeams(cfg.TeamsWebhookURL))
	}
	if cfg.GenericWebhookURL != "" {
		backends = append(backends, notifications.NewWebhook(cfg.GenericWebhookURL))
	}
	// Always include the ticket backend — preserves the existing
	// failure-comment behaviour even with no chat backends configured.
	backends = append(backends, &notifications.Ticket{
		ProviderForBoard: providerForBoard,
		BoardForID:       boardForID,
		Logger:           logger,
	})
	return notifications.NewFanoutRouter(logger, notifications.DefaultRouting, backends...)
}

// Provider returns the provider registered for the board id, if any.
func (o *Orchestrator) Provider(boardID string) (providers.BoardProvider, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	p, ok := o.boardByID[boardID]
	return p, ok
}

// Dispatcher exposes the event dispatcher (used by webhook handlers).
func (o *Orchestrator) Dispatcher() *providers.Dispatcher { return o.disp }

// Engine exposes the stage engine (used by runtime adapters that complete
// async actions).
func (o *Orchestrator) Engine() *stages.Engine { return o.engine }

// BoardConfig returns the static board config for the id, if any.
func (o *Orchestrator) BoardConfig(boardID string) (config.Board, bool) {
	if o.cfg.Boards == nil {
		return config.Board{}, false
	}
	return o.cfg.Boards.ByID(boardID)
}

// Start launches pollers (per board interval) and the consumer goroutine.
// It blocks until ctx is cancelled.
func (o *Orchestrator) Start(ctx context.Context) {
	// pick the smallest configured poll interval as the dispatcher tick.
	interval := 60 * time.Second
	if o.cfg.Boards != nil {
		for _, b := range o.cfg.Boards.Boards {
			if b.Triggers.Poll.Enabled && b.Triggers.Poll.Interval > 0 && b.Triggers.Poll.Interval < interval {
				interval = b.Triggers.Poll.Interval
			}
		}
	}
	// Runtimes that ship their own background GC (currently only
	// `persistent` for pod TTL) opt in via the GCCapable interface.
	for _, rt := range o.runtimes {
		if gc, ok := rt.(runtime.GCCapable); ok {
			gc.StartGC(ctx)
		}
	}
	if o.reviewPoller != nil {
		go o.reviewPoller.Start(ctx)
	}
	if o.lifecyclePoller != nil {
		go o.lifecyclePoller.Start(ctx)
	}
	if o.probePoller != nil {
		go o.probePoller.Start(ctx)
	}
	o.disp.StartPollers(ctx, interval)
	go o.consume(ctx)
	o.logger.Info().Dur("poll_interval", interval).Msg("orchestrator started")
	<-ctx.Done()
	o.logger.Info().Msg("orchestrator stopped")
}

// consume reads events and dispatches them through the stage engine.
// We process serially per orchestrator instance for now; sharding by board
// id is a Phase 6+ concern.
func (o *Orchestrator) consume(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-o.disp.Events():
			o.logger.Debug().
				Str("board", e.BoardID).
				Str("provider", e.Provider).
				Str("source", e.Source).
				Str("type", e.Type).
				Str("ext_id", e.Ticket.ExternalID).
				Str("status", e.Ticket.Status).
				Msg("event")

			b, ok := o.BoardConfig(e.BoardID)
			if !ok {
				continue
			}
			prov, ok := o.Provider(e.BoardID)
			if !ok {
				continue
			}
			if o.tasks == nil {
				o.logger.Warn().Msg("no task store; cannot drive stages")
				continue
			}
			if err := o.engine.HandleEvent(ctx, e, b, prov); err != nil {
				o.logger.Error().Err(err).Str("ext_id", e.Ticket.ExternalID).Msg("stage handle failed")
			}
		}
	}
}
