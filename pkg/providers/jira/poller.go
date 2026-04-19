package jira

import (
	"context"
	"strconv"

	"code-agent/pkg/providers"
)

// Poll fetches recently-updated issues and emits an event for any whose
// updated timestamp differs from the last seen.
func (p *Provider) Poll(ctx context.Context, out chan<- providers.Event) error {
	tickets, err := p.ListTasks(ctx)
	if err != nil {
		return err
	}
	for _, t := range tickets {
		out <- providers.Event{
			BoardID:  p.board.ID,
			Provider: "jira",
			Source:   "poll",
			Type:     "updated",
			Ticket:   t,
			Key:      t.ExternalID + ":" + strconv.FormatInt(t.Updated.Unix(), 10),
		}
	}
	return nil
}
