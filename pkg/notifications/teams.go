package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Teams posts to a Microsoft Teams Incoming Webhook with adaptive cards.
// Workflow: in Teams → channel → Incoming Webhook connector → save the
// generated URL into CODE_AGENT_TEAMS_WEBHOOK_URL.
//
// Adaptive cards (1.5) are richer than legacy MessageCards (per-section
// styling, action buttons). Schema:
// https://adaptivecards.io/explorer/AdaptiveCard.html
type Teams struct {
	WebhookURL string
	HTTP       *http.Client
}

func NewTeams(url string) *Teams {
	return &Teams{
		WebhookURL: url,
		HTTP:       &http.Client{Timeout: 10 * time.Second},
	}
}

func (t *Teams) Name() string { return "teams" }

func (t *Teams) Send(ctx context.Context, n Notification) error {
	if t.WebhookURL == "" {
		return fmt.Errorf("teams: webhook URL not configured")
	}

	card := teamsAdaptiveCard(n)
	buf, err := json.Marshal(card)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.WebhookURL, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("teams http %d", resp.StatusCode)
	}
	return nil
}

// teamsAdaptiveCard builds a minimal adaptive card with priority colour,
// title, body, and (when MR present) a "View PR" action.
func teamsAdaptiveCard(n Notification) map[string]any {
	colour := priorityColour(n.Priority)
	body := []any{
		map[string]any{
			"type":   "TextBlock",
			"text":   n.Title,
			"weight": "Bolder",
			"size":   "Medium",
			"color":  colour,
		},
		map[string]any{
			"type":   "TextBlock",
			"text":   n.Body,
			"wrap":   true,
			"isSubtle": false,
		},
	}
	actions := []any{}
	if n.MR != nil && n.MR.URL != "" {
		actions = append(actions, map[string]any{
			"type":  "Action.OpenUrl",
			"title": "View PR",
			"url":   n.MR.URL,
		})
	}
	if n.Task != nil && n.Task.URL != "" {
		actions = append(actions, map[string]any{
			"type":  "Action.OpenUrl",
			"title": "View Ticket",
			"url":   n.Task.URL,
		})
	}

	card := map[string]any{
		"type":    "AdaptiveCard",
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"version": "1.5",
		"body":    body,
	}
	if len(actions) > 0 {
		card["actions"] = actions
	}

	return map[string]any{
		"type": "message",
		"attachments": []any{
			map[string]any{
				"contentType": "application/vnd.microsoft.card.adaptive",
				"content":     card,
			},
		},
	}
}

// priorityColour maps our priority enum to Adaptive Card text colours.
// Adaptive Cards 1.5 supports: default, dark, light, accent, good,
// warning, attention.
func priorityColour(p Priority) string {
	switch p {
	case PriorityUrgent:
		return "attention"
	case PriorityWarning:
		return "warning"
	case PriorityAction:
		return "accent"
	case PriorityInfo:
		return "good"
	}
	return "default"
}
