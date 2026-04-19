package clickup

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"code-agent/pkg/providers"
)

// envelope is the shape ClickUp posts for task webhooks.
type envelope struct {
	Event     string `json:"event"`
	TaskID    string `json:"task_id"`
	WebhookID string `json:"webhook_id"`
}

// HandleWebhook validates the X-Signature HMAC and emits an Event after
// fetching the full task body (the webhook payload itself is sparse).
func (p *Provider) HandleWebhook(w http.ResponseWriter, r *http.Request, out chan<- providers.Event) {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	if secret := p.board.Triggers.Webhook.Secret.Value(); secret != "" {
		got := r.Header.Get("X-Signature")
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		want := hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(got), []byte(want)) {
			p.logger.Warn().Msg("clickup webhook: bad signature")
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
	}

	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if env.TaskID == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	ticket, err := p.Get(ctx, env.TaskID)
	if err != nil {
		p.logger.Error().Err(err).Str("task", env.TaskID).Msg("fetch ticket failed")
		http.Error(w, "fetch", http.StatusBadGateway)
		return
	}
	if !p.matchesFilters(ticket) {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	out <- providers.Event{
		BoardID:    p.board.ID,
		Provider:   "clickup",
		Source:     "webhook",
		Type:       classifyClickupEvent(env.Event),
		Ticket:     ticket,
		Key:        env.TaskID + ":" + env.Event,
		ReceivedAt: time.Now(),
	}
	w.WriteHeader(http.StatusAccepted)
}

func classifyClickupEvent(name string) string {
	switch {
	case strings.Contains(name, "Created"):
		return "created"
	case strings.Contains(name, "Comment"):
		return "commented"
	case strings.Contains(name, "Status"):
		return "transitioned"
	default:
		return "updated"
	}
}
