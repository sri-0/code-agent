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
	"code-agent/pkg/providers"
	_ "code-agent/pkg/providers/all" // register providers
	"code-agent/pkg/runtime"
	_ "code-agent/pkg/runtime/all" // register runtimes
	"code-agent/pkg/stages"
	"code-agent/pkg/tasks"
	"code-agent/pkg/transcript"
	_ "code-agent/pkg/vcs/all" // register vcs providers
)

type Orchestrator struct {
	cfg    *config.Config
	logger zerolog.Logger
	tasks  tasks.Store
	tx     transcript.Store
	disp   *providers.Dispatcher
	engine *stages.Engine

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
				Logger:     o.logger,
				Transcript: tx,
				OpenCode:   cfg.OpenCode,
				Git:        git,
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
	o.agentRunner = runtime.NewAgentRunner(o.runtimes, tx)
	o.engine.Register("run_agent", o.agentRunner)

	// Wire the open_mr action runner (stateless / lazy vcs client cache).
	o.engine.Register("open_mr", actions.NewOpenMRRunner())

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
