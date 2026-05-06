package lifecycle

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"code-agent/internal/config"
	"code-agent/pkg/tasks"
)

// ProbePoller wakes periodically and runs Probe against every task with
// an active worker. It writes the resulting Decision into Lifecycle.Runtime
// (and possibly Lifecycle.Session if escalation triggers stuck/terminated).
//
// Designed to coexist with PRPoller — that one watches PR state, this one
// watches *runtime* state (pod alive? opencode reachable?). Both feed
// the reactions engine via onTransition.
type ProbePoller struct {
	tasks    tasks.Store
	boards   *config.BoardsConfig
	pod      PodProber
	oc       OpenCodeProber
	interval time.Duration
	logger   zerolog.Logger

	onTransition func(ctx context.Context, t *tasks.Task, prev, next tasks.Lifecycle)
}

// NewProbePoller — pod may be nil for non-k8s setups. oc defaults to
// HTTPOpenCodeProber if nil. onTransition may be nil.
func NewProbePoller(
	store tasks.Store,
	boards *config.BoardsConfig,
	pod PodProber,
	oc OpenCodeProber,
	interval time.Duration,
	logger zerolog.Logger,
	onTransition func(ctx context.Context, t *tasks.Task, prev, next tasks.Lifecycle),
) *ProbePoller {
	if interval <= 0 {
		interval = 90 * time.Second
	}
	if oc == nil {
		oc = NewHTTPOpenCodeProber()
	}
	return &ProbePoller{
		tasks:        store,
		boards:       boards,
		pod:          pod,
		oc:           oc,
		interval:     interval,
		logger:       logger.With().Str("component", "probe_poller").Logger(),
		onTransition: onTransition,
	}
}

func (p *ProbePoller) Start(ctx context.Context) {
	p.logger.Info().Dur("interval", p.interval).Msg("probe poller started")
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.tick(ctx)
		}
	}
}

func (p *ProbePoller) tick(ctx context.Context) {
	if p.tasks == nil || p.boards == nil {
		return
	}
	for _, b := range p.boards.Boards {
		ts, err := p.tasks.ListByBoard(ctx, b.ID)
		if err != nil {
			p.logger.Warn().Err(err).Str("board", b.ID).Msg("list tasks failed")
			continue
		}
		for _, t := range ts {
			if !shouldProbe(t) {
				continue
			}
			p.processTask(ctx, t)
		}
	}
}

// shouldProbe filters out tasks that don't have an active runtime to
// probe. Done/terminated tasks are skipped. Tasks with no WorkerRef.URL
// are skipped (haven't been spawned yet).
func shouldProbe(t *tasks.Task) bool {
	if t.WorkerRef.URL == "" {
		return false
	}
	switch t.Lifecycle.Session.State {
	case tasks.SessionStateDone, tasks.SessionStateTerminated:
		return false
	}
	return true
}

func (p *ProbePoller) processTask(ctx context.Context, t *tasks.Task) {
	prev := t.Lifecycle
	tasks.EnsureLifecycle(t, time.Now())

	res := Probe(ctx, t, p.pod, p.oc)
	d := ResolveProbe(res, t.Lifecycle.Detect, time.Now())

	now := time.Now()
	t.Lifecycle.Runtime.State = d.NextRuntime
	t.Lifecycle.Runtime.Reason = d.NextRuntimeReason
	t.Lifecycle.Runtime.LastObservedAt = &now
	t.Lifecycle.Detect = d.NextDetect

	if d.NextSession != "" {
		t.Lifecycle.Session.State = d.NextSession
		t.Lifecycle.Session.Reason = d.NextSessionReason
		t.Lifecycle.Session.LastTransitionAt = &now
	}

	if !lifecycleChanged(prev, t.Lifecycle) {
		return
	}
	// Re-read + merge: another poller (PR) may have written a different
	// part of the lifecycle since our ListByBoard. We own only the
	// Runtime track, the Detect sub-state, and (when escalating) the
	// Session.State on stuck/terminated transitions. Everything else on
	// disk wins.
	fresh, err := p.tasks.Get(ctx, t.ID)
	if err != nil {
		p.logger.Warn().Err(err).Str("task", t.ID).Msg("re-read for merge failed")
		return
	}
	prevFresh := fresh.Lifecycle
	tasks.EnsureLifecycle(fresh, time.Now())
	fresh.Lifecycle.Runtime = t.Lifecycle.Runtime
	fresh.Lifecycle.Detect = t.Lifecycle.Detect
	if d.NextSession != "" {
		fresh.Lifecycle.Session.State = t.Lifecycle.Session.State
		fresh.Lifecycle.Session.Reason = t.Lifecycle.Session.Reason
		fresh.Lifecycle.Session.LastTransitionAt = t.Lifecycle.Session.LastTransitionAt
	}
	if err := p.tasks.Update(ctx, fresh); err != nil {
		p.logger.Warn().Err(err).Str("task", t.ID).Msg("persist probe lifecycle failed")
		return
	}
	if d.Stuck {
		p.logger.Warn().Str("task", t.ID).Str("evidence", d.Evidence).Msg("agent stuck")
	}
	if p.onTransition != nil {
		p.onTransition(ctx, fresh, prevFresh, fresh.Lifecycle)
	}
}
