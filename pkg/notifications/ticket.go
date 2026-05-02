package notifications

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	"code-agent/internal/config"
	"code-agent/pkg/providers"
)

// Ticket posts a comment back to the originating board ticket via the
// provider that owns it. Replaces the per-callsite "post comment on
// failure" pattern that was scattered across openMRFailureComment +
// agent_runner. Idempotent caller responsibility — tickets dedupe
// upstream by content (Jira) but ClickUp accepts dupes silently.
type Ticket struct {
	// ProviderForBoard returns the provider for a given board id. The
	// orchestrator builds this closure when wiring the router so
	// notifications doesn't take a direct dep on the dispatcher.
	ProviderForBoard func(boardID string) (providers.BoardProvider, bool)
	// BoardForID — same shape; used to enrich messages with board name.
	BoardForID func(boardID string) (config.Board, bool)
	Logger     zerolog.Logger
}

func (t *Ticket) Name() string { return "ticket" }

func (t *Ticket) Send(ctx context.Context, n Notification) error {
	if n.Task == nil {
		return fmt.Errorf("ticket: no task on notification")
	}
	if t.ProviderForBoard == nil {
		return fmt.Errorf("ticket: no provider lookup configured")
	}
	prov, ok := t.ProviderForBoard(n.Task.BoardID)
	if !ok {
		return fmt.Errorf("ticket: provider for board %q not registered", n.Task.BoardID)
	}
	body := n.Title
	if n.Body != "" {
		if body != "" {
			body += "\n\n"
		}
		body += n.Body
	}
	return prov.Comment(ctx, n.Task.ExternalID, providers.Comment{Body: body})
}
