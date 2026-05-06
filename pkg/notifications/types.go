// Package notifications routes lifecycle events to humans across
// Mattermost, MS Teams, ticket comments, and arbitrary webhooks.
//
// Mattermost and MS Teams backends land in §6. For now this package
// defines just the interface so the reactions engine has something to
// call into. The default implementation in §6 ships ticket-comment
// behaviour preserved from the existing failure-comment paths.
package notifications

import (
	"context"

	"code-agent/pkg/tasks"
)

// Priority drives backend routing. The engine maps reaction events to a
// priority; routing config in boards.yaml maps priority → backend list.
type Priority string

const (
	PriorityUrgent  Priority = "urgent"  // stuck, errored, needs_input
	PriorityAction  Priority = "action"  // PR ready to merge
	PriorityWarning Priority = "warning" // auto-fix failed
	PriorityInfo    Priority = "info"    // summary, all done
)

// Notification is the canonical payload. Backends decide how to render —
// Mattermost uses Markdown + threading via root_id; Teams uses adaptive
// cards; ticket-comment posts to the originating Jira/ClickUp ticket.
type Notification struct {
	Priority Priority
	Title    string
	Body     string
	Task     *tasks.Task
	// MR is the dominant merge request, when relevant. Optional.
	MR *tasks.MergeRef
	// EventKey is the reactions-engine event that fired this notification
	// (e.g. "ci-failed", "approved-and-green"). Used by backends to pick
	// an icon/colour/badge.
	EventKey string
}

// Notifier is one delivery target.
type Notifier interface {
	Name() string // "mattermost" | "teams" | "ticket" | "webhook"
	Send(ctx context.Context, n Notification) error
}

// Router fans a Notification across the right backends based on
// Priority. nil-safe; returns no error if no backends are registered.
type Router interface {
	Send(ctx context.Context, n Notification) error
}
