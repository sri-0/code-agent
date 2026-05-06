package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Webhook is a fire-and-forget JSON POST notifier. The body is the
// Notification serialised to JSON. Used for custom integrations
// (PagerDuty events, Sentry, internal log services, etc.).
type Webhook struct {
	URL  string
	HTTP *http.Client
}

func NewWebhook(url string) *Webhook {
	return &Webhook{URL: url, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

func (w *Webhook) Name() string { return "webhook" }

func (w *Webhook) Send(ctx context.Context, n Notification) error {
	if w.URL == "" {
		return fmt.Errorf("webhook: URL not configured")
	}
	taskID := ""
	if n.Task != nil {
		taskID = n.Task.ID
	}
	prURL := ""
	if n.MR != nil {
		prURL = n.MR.URL
	}
	body, err := json.Marshal(map[string]any{
		"event":    n.EventKey,
		"priority": string(n.Priority),
		"title":    n.Title,
		"body":     n.Body,
		"task_id":  taskID,
		"pr_url":   prURL,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook http %d", resp.StatusCode)
	}
	return nil
}
