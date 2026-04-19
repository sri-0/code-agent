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
	"code-agent/pkg/providers"
	_ "code-agent/pkg/providers/all" // register providers
	"code-agent/pkg/tasks"
)

type Orchestrator struct {
	cfg    *config.Config
	logger zerolog.Logger
	tasks  tasks.Store
	disp   *providers.Dispatcher

	// boardByID lets webhook handlers route to the right provider.
	mu        sync.RWMutex
	boardByID map[string]providers.BoardProvider
}

func New(cfg *config.Config, logger zerolog.Logger, taskStore tasks.Store) (*Orchestrator, error) {
	o := &Orchestrator{
		cfg:       cfg,
		logger:    logger.With().Str("component", "orchestrator").Logger(),
		tasks:     taskStore,
		disp:      providers.NewDispatcher(256, 5*time.Minute, logger),
		boardByID: map[string]providers.BoardProvider{},
	}

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

// consume reads events and (for now) just logs them. Stage engine wiring
// lands in Phase 3.
func (o *Orchestrator) consume(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-o.disp.Events():
			o.logger.Info().
				Str("board", e.BoardID).
				Str("provider", e.Provider).
				Str("source", e.Source).
				Str("type", e.Type).
				Str("ext_id", e.Ticket.ExternalID).
				Str("status", e.Ticket.Status).
				Msg("event")
		}
	}
}
