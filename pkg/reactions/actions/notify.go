// Package actions implements the built-in reactions Actions.
package actions

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	"code-agent/pkg/notifications"
	"code-agent/pkg/reactions"
)

// Notify is the simplest reaction action — fans the event out via the
// notifications router. Used directly for approved-and-green and
// agent-stuck (no agent run, just tell humans).
type Notify struct {
	Router notifications.Router
	Logger zerolog.Logger
}

func (n *Notify) Name() string { return "notify" }

func (n *Notify) Run(ctx context.Context, e reactions.Event) error {
	if n.Router == nil {
		return fmt.Errorf("notify: no router configured")
	}
	priority := mapPriority(e.Key)
	return n.Router.Send(ctx, notifications.Notification{
		Priority: priority,
		Title:    titleFor(e),
		Body:     bodyFor(e),
		Task:     e.Task,
		EventKey: string(e.Key),
	})
}

func mapPriority(key reactions.EventKey) notifications.Priority {
	switch key {
	case reactions.EventCIFailed, reactions.EventChangesRequested:
		return notifications.PriorityWarning
	case reactions.EventApprovedAndGreen:
		return notifications.PriorityAction
	case reactions.EventAgentStuck:
		return notifications.PriorityUrgent
	case reactions.EventPRMerged:
		return notifications.PriorityInfo
	}
	return notifications.PriorityInfo
}

func titleFor(e reactions.Event) string {
	switch e.Key {
	case reactions.EventCIFailed:
		return "CI failed: " + e.Task.ExternalID
	case reactions.EventChangesRequested:
		return "Changes requested: " + e.Task.ExternalID
	case reactions.EventApprovedAndGreen:
		return "Ready to merge: " + e.Task.ExternalID
	case reactions.EventAgentStuck:
		return "Agent stuck: " + e.Task.ExternalID
	case reactions.EventPRMerged:
		return "Merged: " + e.Task.ExternalID
	}
	return string(e.Key) + " on " + e.Task.ExternalID
}

func bodyFor(e reactions.Event) string {
	url := e.Next.PR.URL
	switch e.Key {
	case reactions.EventCIFailed:
		return fmt.Sprintf("CI failing on %s. PR: %s", e.Task.Title, url)
	case reactions.EventChangesRequested:
		return fmt.Sprintf("Reviewer requested changes on %s. PR: %s", e.Task.Title, url)
	case reactions.EventApprovedAndGreen:
		return fmt.Sprintf("Approved + green CI — ready to merge. PR: %s", url)
	case reactions.EventAgentStuck:
		return fmt.Sprintf("Agent for %s is stuck and needs attention.", e.Task.ExternalID)
	case reactions.EventPRMerged:
		return fmt.Sprintf("PR merged for %s — %s", e.Task.ExternalID, url)
	}
	return ""
}
