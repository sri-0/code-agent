package reactions

import (
	"context"
	"strings"
	"time"

	"code-agent/internal/config"
	"code-agent/pkg/notifications"
	"code-agent/pkg/tasks"
)

// Process is the engine entry point. The lifecycle poller calls this
// from its onTransition callback for every prev→next change.
func (e *Engine) Process(ctx context.Context, evt Event) {
	cfg, ok := evt.Board.Reactions[string(evt.Key)]
	if !ok {
		cfg = defaultReactionConfig(evt.Key)
	}

	log := e.logger.With().
		Str("task", evt.Task.ID).
		Str("event", string(evt.Key)).
		Str("evidence", evt.EvidenceHash).
		Logger()

	// Ledger dedupe: have we already fired this event with this exact
	// evidence hash within the cooldown window?
	if alreadyFired(evt.Task, evt.Key, evt.EvidenceHash, e.ledgerCooldown) {
		log.Debug().Msg("dedupe — same evidence within cooldown")
		return
	}

	// Retry cap: if Retries > 0 and we've already fired the configured
	// number of times on this evidence, route to notify-only.
	if cfg.Retries > 0 && firedCount(evt.Task, evt.Key) >= cfg.Retries+1 {
		log.Info().Int("retries", cfg.Retries).Msg("retry cap reached; falling back to notify")
		e.notifyEscalation(ctx, evt, cfg, "retry-cap-reached")
		return
	}

	action := cfg.Action
	if action == "" {
		action = defaultActionFor(evt.Key)
	}

	// auto:false still fires the notify path so humans see the event.
	if !cfg.AutoEnabled() && action != "notify" {
		log.Info().Str("planned", action).Msg("auto disabled; routing to notify")
		e.notifyEscalation(ctx, evt, cfg, "auto-disabled")
		return
	}

	a, ok := e.actions[action]
	if !ok {
		log.Warn().Str("action", action).Msg("no action registered; skipping")
		return
	}

	if err := a.Run(ctx, evt); err != nil {
		log.Error().Err(err).Msg("action failed")
		// Failure on send-to-agent → notify so it doesn't silently swallow.
		e.notifyEscalation(ctx, evt, cfg, "action-failed: "+err.Error())
		return
	}

	e.recordFire(ctx, evt, evt.EvidenceHash)
}

// notifyEscalation routes a notify-priority message into the
// notifications router. Fires for auto:false events, retry-cap hits,
// and action failures.
func (e *Engine) notifyEscalation(ctx context.Context, evt Event, cfg config.ReactionConfig, reason string) {
	if e.router == nil {
		return
	}
	priority := notifications.Priority(cfg.Priority)
	if priority == "" {
		priority = defaultPriorityFor(evt.Key)
	}
	body := strings.Builder{}
	body.WriteString("Reaction `")
	body.WriteString(string(evt.Key))
	body.WriteString("` on task `")
	body.WriteString(evt.Task.ExternalID)
	body.WriteString("`\n\nReason: ")
	body.WriteString(reason)
	if evt.Next.PR.URL != "" {
		body.WriteString("\nPR: ")
		body.WriteString(evt.Next.PR.URL)
	}
	_ = e.router.Send(ctx, notifications.Notification{
		Priority: priority,
		Title:    titleFor(evt.Key, evt.Task.ExternalID),
		Body:     body.String(),
		Task:     evt.Task,
		EventKey: string(evt.Key),
	})
	e.recordFire(ctx, evt, evt.EvidenceHash)
}

func alreadyFired(t *tasks.Task, key EventKey, hash string, cooldown time.Duration) bool {
	if t.ReactionLedger == nil {
		return false
	}
	last, ok := t.ReactionLedger[ledgerKey(key, hash)]
	if !ok {
		return false
	}
	return time.Since(last) < cooldown
}

// firedCount returns the count of any historical fires of `key` against
// this task — used for retry caps. We don't distinguish by evidence
// here, intentionally: 3 attempts to fix CI is 3 attempts even if the
// failure flapped between two different runs.
func firedCount(t *tasks.Task, key EventKey) int {
	if t.ReactionLedger == nil {
		return 0
	}
	prefix := string(key) + "|"
	n := 0
	for k := range t.ReactionLedger {
		if strings.HasPrefix(k, prefix) {
			n++
		}
	}
	return n
}

func (e *Engine) recordFire(ctx context.Context, evt Event, hash string) {
	if evt.Task.ReactionLedger == nil {
		evt.Task.ReactionLedger = map[string]time.Time{}
	}
	evt.Task.ReactionLedger[ledgerKey(evt.Key, hash)] = time.Now()
	if e.store != nil {
		if err := e.store.Update(ctx, evt.Task); err != nil {
			e.logger.Warn().Err(err).Msg("persist reaction ledger failed")
		}
	}
}

func ledgerKey(key EventKey, hash string) string {
	return string(key) + "|" + hash
}

// defaults --------------------------------------------------------------

func defaultReactionConfig(key EventKey) config.ReactionConfig {
	switch key {
	case EventCIFailed:
		return config.ReactionConfig{Action: "send-to-agent", Retries: 2, Priority: "warning"}
	case EventChangesRequested:
		return config.ReactionConfig{Action: "send-to-agent", EscalateAfter: "30m", Priority: "warning"}
	case EventApprovedAndGreen:
		auto := false
		return config.ReactionConfig{Auto: &auto, Action: "notify", Priority: "action"}
	case EventAgentStuck:
		return config.ReactionConfig{Action: "notify", Threshold: "10m", Priority: "urgent"}
	case EventPRMerged:
		return config.ReactionConfig{Action: "notify", Priority: "info"}
	}
	return config.ReactionConfig{}
}

func defaultActionFor(key EventKey) string {
	switch key {
	case EventCIFailed, EventChangesRequested:
		return "send-to-agent"
	case EventApprovedAndGreen, EventAgentStuck, EventPRMerged:
		return "notify"
	}
	return "notify"
}

func defaultPriorityFor(key EventKey) notifications.Priority {
	switch key {
	case EventCIFailed, EventChangesRequested:
		return notifications.PriorityWarning
	case EventApprovedAndGreen:
		return notifications.PriorityAction
	case EventAgentStuck:
		return notifications.PriorityUrgent
	case EventPRMerged:
		return notifications.PriorityInfo
	}
	return notifications.PriorityInfo
}

func titleFor(key EventKey, externalID string) string {
	if externalID == "" {
		externalID = "<unknown>"
	}
	switch key {
	case EventCIFailed:
		return "CI failed: " + externalID
	case EventChangesRequested:
		return "Changes requested: " + externalID
	case EventApprovedAndGreen:
		return "Ready to merge: " + externalID
	case EventAgentStuck:
		return "Agent stuck: " + externalID
	case EventPRMerged:
		return "Merged: " + externalID
	}
	return string(key) + " on " + externalID
}
