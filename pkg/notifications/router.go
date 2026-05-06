package notifications

import (
	"context"

	"github.com/rs/zerolog"
)

// Routing maps a Priority to the list of notifier names that should
// receive notifications of that priority. Notifier names match
// Notifier.Name() ("mattermost" | "teams" | "webhook" | "ticket").
type Routing map[Priority][]string

// DefaultRouting is the out-of-the-box mapping. Operators override
// per-deployment via config (boards.yaml notifications.routing block).
//
// urgent  → all channels (stuck, errored — humans must see)
// action  → mattermost + teams (PR ready to merge — non-urgent but actionable)
// warning → mattermost + ticket (auto-fix failed — ops only)
// info    → mattermost (summary noise)
var DefaultRouting = Routing{
	PriorityUrgent:  {"mattermost", "teams", "ticket"},
	PriorityAction:  {"mattermost", "teams"},
	PriorityWarning: {"mattermost", "ticket"},
	PriorityInfo:    {"mattermost"},
}

// FanoutRouter fans a Notification across the backends configured for
// its Priority. Backends that don't error are tried independently;
// errors are logged but don't abort other backends.
type FanoutRouter struct {
	notifiers map[string]Notifier
	routing   Routing
	logger    zerolog.Logger
}

func NewFanoutRouter(logger zerolog.Logger, routing Routing, notifiers ...Notifier) *FanoutRouter {
	if len(routing) == 0 {
		routing = DefaultRouting
	}
	m := make(map[string]Notifier, len(notifiers))
	for _, n := range notifiers {
		if n == nil {
			continue
		}
		m[n.Name()] = n
	}
	return &FanoutRouter{
		notifiers: m,
		routing:   routing,
		logger:    logger.With().Str("component", "notify_router").Logger(),
	}
}

// Send fans out to every configured backend for the notification's
// priority. Best-effort: errors per backend are logged but don't fail
// the call. Returns nil unless every configured backend errored AND
// at least one was configured.
func (r *FanoutRouter) Send(ctx context.Context, n Notification) error {
	names := r.routing[n.Priority]
	if len(names) == 0 {
		r.logger.Debug().Str("priority", string(n.Priority)).Msg("no backends for priority; dropping")
		return nil
	}

	tried, ok := 0, 0
	for _, name := range names {
		nf, exists := r.notifiers[name]
		if !exists {
			continue
		}
		tried++
		if err := nf.Send(ctx, n); err != nil {
			r.logger.Warn().Err(err).Str("backend", name).Str("event", n.EventKey).Msg("notify failed")
			continue
		}
		ok++
	}
	if tried > 0 && ok == 0 {
		// Every configured backend errored — log louder.
		r.logger.Error().Str("event", n.EventKey).Msg("all notify backends failed")
	}
	return nil
}
