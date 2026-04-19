package providers

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// Dispatcher fans events from many BoardProviders into a single channel,
// applying a TTL cache so the same Key is not emitted twice within the
// dedupe window (covers webhook+poll overlap).
type Dispatcher struct {
	out       chan Event
	providers []BoardProvider
	logger    zerolog.Logger

	dedupeTTL time.Duration
	mu        sync.Mutex
	seen      map[string]time.Time
}

func NewDispatcher(buffer int, dedupeTTL time.Duration, logger zerolog.Logger) *Dispatcher {
	if dedupeTTL == 0 {
		dedupeTTL = 5 * time.Minute
	}
	return &Dispatcher{
		out:       make(chan Event, buffer),
		dedupeTTL: dedupeTTL,
		seen:      make(map[string]time.Time),
		logger:    logger,
	}
}

func (d *Dispatcher) Register(p BoardProvider) {
	d.providers = append(d.providers, p)
}

// Events returns the read side of the channel.
func (d *Dispatcher) Events() <-chan Event { return d.out }

// EmitWebhook is the entry point for HTTP webhook handlers. It applies dedupe
// before pushing to the channel.
func (d *Dispatcher) EmitWebhook(e Event) {
	if e.ReceivedAt.IsZero() {
		e.ReceivedAt = time.Now()
	}
	if e.Source == "" {
		e.Source = "webhook"
	}
	d.emit(e)
}

// StartPollers launches a goroutine per provider that calls Poll on the given
// interval. ctx cancels them all.
func (d *Dispatcher) StartPollers(ctx context.Context, interval time.Duration) {
	if interval == 0 {
		interval = 60 * time.Second
	}
	for _, p := range d.providers {
		go d.pollLoop(ctx, p, interval)
	}
	go d.gcLoop(ctx)
}

func (d *Dispatcher) pollLoop(ctx context.Context, p BoardProvider, interval time.Duration) {
	pollCh := make(chan Event, 64)
	go func() {
		for e := range pollCh {
			if e.Source == "" {
				e.Source = "poll"
			}
			if e.ReceivedAt.IsZero() {
				e.ReceivedAt = time.Now()
			}
			d.emit(e)
		}
	}()

	t := time.NewTicker(interval)
	defer t.Stop()
	// kick once at start
	if err := p.Poll(ctx, pollCh); err != nil {
		d.logger.Error().Err(err).Str("board", p.ID()).Msg("initial poll failed")
	}
	for {
		select {
		case <-ctx.Done():
			close(pollCh)
			return
		case <-t.C:
			if err := p.Poll(ctx, pollCh); err != nil {
				d.logger.Error().Err(err).Str("board", p.ID()).Msg("poll failed")
			}
		}
	}
}

func (d *Dispatcher) emit(e Event) {
	if e.Key != "" {
		d.mu.Lock()
		if last, ok := d.seen[e.Key]; ok && time.Since(last) < d.dedupeTTL {
			d.mu.Unlock()
			d.logger.Debug().Str("key", e.Key).Msg("event deduped")
			return
		}
		d.seen[e.Key] = time.Now()
		d.mu.Unlock()
	}
	select {
	case d.out <- e:
	default:
		d.logger.Warn().Str("board", e.BoardID).Str("ext", e.Ticket.ExternalID).Msg("event channel full; dropping")
	}
}

func (d *Dispatcher) gcLoop(ctx context.Context) {
	t := time.NewTicker(d.dedupeTTL)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			d.mu.Lock()
			for k, ts := range d.seen {
				if now.Sub(ts) > d.dedupeTTL {
					delete(d.seen, k)
				}
			}
			d.mu.Unlock()
		}
	}
}
