// Package llm is a tiny OpenAI-compatible chat client used by the
// orchestrator for utility LLM calls (commit messages, PR descriptions).
// We deliberately don't route these through opencode — they're routine,
// fire-and-forget, and have no session semantics.
//
// Configuration is env-driven to avoid YAML churn for a simple dep:
//
//	CODE_AGENT_LLM_BASE_URL   default https://openrouter.ai/api/v1
//	CODE_AGENT_LLM_API_KEY    required (alias: AI_API_KEY)
//	CODE_AGENT_LLM_MODEL      default anthropic/claude-haiku-4.5
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type Options struct {
	BaseURL string
	APIKey  string
	Model   string
	Timeout time.Duration
}

type Client struct {
	opts Options
	http *http.Client
}

// New reads config from env (with overrides from Options) and returns a
// client. Returns an error only if no API key is available.
func New(overrides Options) (*Client, error) {
	o := overrides
	if o.BaseURL == "" {
		o.BaseURL = envOr("CODE_AGENT_LLM_BASE_URL", "https://openrouter.ai/api/v1")
	}
	if o.APIKey == "" {
		o.APIKey = envOr("CODE_AGENT_LLM_API_KEY", os.Getenv("AI_API_KEY"))
	}
	if o.Model == "" {
		o.Model = envOr("CODE_AGENT_LLM_MODEL", "anthropic/claude-haiku-4.5")
	}
	if o.Timeout == 0 {
		o.Timeout = 60 * time.Second
	}
	if o.APIKey == "" {
		return nil, fmt.Errorf("llm: no api key (set CODE_AGENT_LLM_API_KEY or AI_API_KEY)")
	}
	return &Client{
		opts: o,
		http: &http.Client{Timeout: o.Timeout},
	}, nil
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Chat sends a single round-trip chat and returns the assistant's text.
func (c *Client) Chat(ctx context.Context, system, user string) (string, error) {
	msgs := []Message{}
	if system != "" {
		msgs = append(msgs, Message{Role: "system", Content: system})
	}
	msgs = append(msgs, Message{Role: "user", Content: user})
	body, err := json.Marshal(chatRequest{
		Model:       c.opts.Model,
		Messages:    msgs,
		Temperature: 0.2,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.opts.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.opts.APIKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm post: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("llm http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", fmt.Errorf("llm decode: %w (%s)", err, strings.TrimSpace(string(raw)))
	}
	if cr.Error != nil {
		return "", fmt.Errorf("llm error: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("llm: empty response")
	}
	return cr.Choices[0].Message.Content, nil
}

// ChangeSummary is the structured result from SummarizeChange.
type ChangeSummary struct {
	CommitMessage string `json:"commit_message"`
	PRTitle       string `json:"pr_title"`
	PRBody        string `json:"pr_body"`
}

// SummarizeChange asks the LLM to produce commit + PR text for a diff.
// Diff is truncated if larger than maxDiffChars. Returns a structured
// ChangeSummary. Falls back to a generic message on parse failure so the
// orchestrator never blocks on LLM flakiness.
func (c *Client) SummarizeChange(ctx context.Context, ticket, title, description, repoName, diff string) (ChangeSummary, error) {
	const maxDiffChars = 40000
	truncated := false
	if len(diff) > maxDiffChars {
		diff = diff[:maxDiffChars] + "\n\n[... diff truncated ...]"
		truncated = true
	}
	system := `You write commit messages and PR descriptions for an automated coding agent.
Respond with a single JSON object on one line, no markdown, no prose, no code fences.
Schema:
{
  "commit_message": "subject-line (<=72 chars, mentions ticket id)\n\nbody paragraph (2-5 lines, what changed and why)",
  "pr_title":       "Short PR title (<=80 chars, mentions ticket id)",
  "pr_body":        "Markdown with sections: ## Summary, ## Changes, ## Testing notes"
}`

	var sb strings.Builder
	fmt.Fprintf(&sb, "Ticket: %s — %s\n", ticket, title)
	if description != "" {
		sb.WriteString("\nTicket description / plan:\n")
		sb.WriteString(description)
		sb.WriteString("\n")
	}
	fmt.Fprintf(&sb, "\nRepo: %s\n\n", repoName)
	if truncated {
		sb.WriteString("Note: diff was truncated due to size. Summarise what you can see.\n\n")
	}
	sb.WriteString("Diff:\n```diff\n")
	sb.WriteString(diff)
	sb.WriteString("\n```\n")

	raw, err := c.Chat(ctx, system, sb.String())
	if err != nil {
		return fallbackSummary(ticket, title, repoName), err
	}
	raw = strings.TrimSpace(raw)
	// Some models still wrap JSON in ```json fences despite instructions.
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)
	var s ChangeSummary
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return fallbackSummary(ticket, title, repoName), fmt.Errorf("llm: parse summary: %w (%s)", err, truncatePreview(raw))
	}
	if s.CommitMessage == "" || s.PRTitle == "" {
		return fallbackSummary(ticket, title, repoName), fmt.Errorf("llm: summary missing fields")
	}
	return s, nil
}

func fallbackSummary(ticket, title, repoName string) ChangeSummary {
	return ChangeSummary{
		CommitMessage: fmt.Sprintf("%s %s\n\nAutomated change (LLM summary unavailable).", ticket, title),
		PRTitle:       fmt.Sprintf("%s %s", ticket, title),
		PRBody:        fmt.Sprintf("Automated change for ticket %s in %s.\n\n(LLM summary was not available.)", ticket, repoName),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func truncatePreview(s string) string {
	if len(s) > 160 {
		return s[:160] + "..."
	}
	return s
}
