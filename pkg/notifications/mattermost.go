package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Mattermost is a bot-account-backed notifier. Posts via REST API, with
// per-task threading via root_id. The first notification on a task
// becomes the thread root; subsequent notifications post under it.
//
// Auth: bot account access token (NOT a personal access token). Create
// the bot under System Console → Integrations → Bot Accounts; copy the
// token; assign it to a channel.
type Mattermost struct {
	BaseURL          string // e.g. https://mm.example.com
	Token            string
	DefaultChannelID string
	HTTP             *http.Client

	// rootIDs caches the root post ID per task so subsequent
	// notifications thread under the same root. Cleared on process restart.
	mu       sync.Mutex
	rootIDs  map[string]string // task ID → root_id
}

func NewMattermost(baseURL, token, defaultChannel string) *Mattermost {
	return &Mattermost{
		BaseURL:          strings.TrimRight(baseURL, "/"),
		Token:            token,
		DefaultChannelID: defaultChannel,
		HTTP:             &http.Client{Timeout: 10 * time.Second},
		rootIDs:          map[string]string{},
	}
}

func (m *Mattermost) Name() string { return "mattermost" }

func (m *Mattermost) Send(ctx context.Context, n Notification) error {
	if m.BaseURL == "" || m.Token == "" || m.DefaultChannelID == "" {
		return fmt.Errorf("mattermost: missing config (base URL, token, or channel)")
	}

	taskID := ""
	if n.Task != nil {
		taskID = n.Task.ID
	}

	body := struct {
		ChannelID string `json:"channel_id"`
		Message   string `json:"message"`
		RootID    string `json:"root_id,omitempty"`
		Props     map[string]any `json:"props,omitempty"`
	}{
		ChannelID: m.DefaultChannelID,
		Message:   formatMessage(n),
		Props: map[string]any{
			"code_agent_event":    n.EventKey,
			"code_agent_priority": string(n.Priority),
			"code_agent_task":     taskID,
		},
	}

	// Thread under the existing root post if we have one for this task.
	if taskID != "" {
		m.mu.Lock()
		if root, ok := m.rootIDs[taskID]; ok {
			body.RootID = root
		}
		m.mu.Unlock()
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.BaseURL+"/api/v4/posts", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("mattermost http %d", resp.StatusCode)
	}

	// Cache the root post id on first successful send for this task.
	if taskID != "" && body.RootID == "" {
		var post struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&post); err == nil && post.ID != "" {
			m.mu.Lock()
			m.rootIDs[taskID] = post.ID
			m.mu.Unlock()
		}
	}
	return nil
}

func formatMessage(n Notification) string {
	var b strings.Builder
	if n.Title != "" {
		b.WriteString("**")
		b.WriteString(n.Title)
		b.WriteString("**\n")
	}
	if n.Body != "" {
		b.WriteString(n.Body)
	}
	return b.String()
}
