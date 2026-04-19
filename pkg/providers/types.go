// Package providers defines the BoardProvider abstraction and shared types
// used by orchestrator-side board integrations (Jira, ClickUp, ...).
//
// Concrete providers live in subpackages (jira/, clickup/) and translate
// provider-specific REST/webhook payloads into the generic Ticket/Event types
// here. The orchestrator is provider-agnostic.
package providers

import (
	"context"
	"net/http"
	"time"
)

// Ticket is the provider-neutral view of a board item.
type Ticket struct {
	BoardID     string
	ExternalID  string // e.g. "ALPHA-123" (jira) or "abc1234" (clickup)
	Title       string
	Description string
	URL         string
	Status      string // raw status string from the provider
	Stage       string // mapped stage name (filled by stage engine, optional here)
	Labels      []string
	Assignee    string
	Updated     time.Time
}

// Comment is a comment to be posted back to the provider.
type Comment struct {
	Body string
}

// Event represents a change observed via webhook or poller. Source is one of
// "webhook" or "poll"; Type is provider-neutral ("created", "updated",
// "transitioned", "commented").
type Event struct {
	BoardID    string
	Provider   string
	Source     string
	Type       string
	Ticket     Ticket
	ReceivedAt time.Time
	// Key is a dedupe key. For webhooks it's the event id; for polls it's
	// typically "<external_id>:<updated_unix>".
	Key string
}

// BoardProvider is implemented by each provider package. Methods are called
// from the orchestrator and must be safe for concurrent use.
type BoardProvider interface {
	// ID is the board id from boards.yaml.
	ID() string
	// Provider returns "jira" | "clickup" | ...
	Provider() string

	// ListTasks returns all tickets currently matching board filters.
	ListTasks(ctx context.Context) ([]Ticket, error)
	// Get returns a single ticket by external id.
	Get(ctx context.Context, externalID string) (Ticket, error)

	// Transition moves a ticket to a target status (provider-specific
	// status name; the stage engine maps stage -> status before calling).
	Transition(ctx context.Context, externalID, targetStatus string) error
	// Comment posts a comment on the ticket.
	Comment(ctx context.Context, externalID string, c Comment) error
	// AddTag attaches a tag/label to the ticket. Used by run_agent to mark
	// tickets it parks (e.g. "needs-more-info"). Idempotent: attaching an
	// already-present tag should not error.
	AddTag(ctx context.Context, externalID, tag string) error
	// UpdateDescription replaces the ticket's description body with the
	// given markdown. The orchestrator uses this to write agent plans
	// back onto the ticket at the end of the plan stage.
	UpdateDescription(ctx context.Context, externalID, body string) error

	// HandleWebhook validates and parses a webhook request, emitting any
	// resulting events on out. Implementations write the HTTP response.
	HandleWebhook(w http.ResponseWriter, r *http.Request, out chan<- Event)

	// Poll runs one polling cycle, emitting events on out. The orchestrator
	// invokes this on the configured interval.
	Poll(ctx context.Context, out chan<- Event) error
}
