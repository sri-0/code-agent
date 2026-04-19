package clickup

import (
	"context"
	"strconv"

	"code-agent/pkg/providers"
)

func (p *Provider) Poll(ctx context.Context, out chan<- providers.Event) error {
	tickets, err := p.ListTasks(ctx)
	if err != nil {
		return err
	}
	for _, t := range tickets {
		out <- providers.Event{
			BoardID:  p.board.ID,
			Provider: "clickup",
			Source:   "poll",
			Type:     "updated",
			Ticket:   t,
			Key:      t.ExternalID + ":" + strconv.FormatInt(t.Updated.UnixMilli(), 10),
		}
	}
	return nil
}
