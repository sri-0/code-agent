package opencode

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"code-agent/pkg/httpclient"
)

// Client talks to a single OpenCode server (URL + optional bearer password).
type Client struct {
	base   string
	http   *httpclient.Client
	logger zerolog.Logger
	pw     string
}

type Options struct {
	BaseURL  string // e.g. http://localhost:4096
	Password string // OPENCODE_SERVER_PASSWORD; empty for local dev
	Timeout  time.Duration
}

func New(opts Options, logger zerolog.Logger) *Client {
	if opts.Timeout == 0 {
		// 60s is generous for the short calls we make (session create,
		// prompt_async submit, health). We never use the sync /message
		// endpoint any more — long-running work is tracked via SSE after
		// a fire-and-forget prompt_async.
		opts.Timeout = 60 * time.Second
	}
	headers := map[string]string{}
	if opts.Password != "" {
		headers["Authorization"] = "Bearer " + opts.Password
	}
	hc := httpclient.New(httpclient.Options{
		BaseURL:        strings.TrimRight(opts.BaseURL, "/"),
		Timeout:        opts.Timeout,
		MaxRetries:     3,
		DefaultHeaders: headers,
	}, logger.With().Str("component", "opencode").Logger())
	return &Client{
		base:   strings.TrimRight(opts.BaseURL, "/"),
		http:   hc,
		logger: logger.With().Str("component", "opencode").Logger(),
		pw:     opts.Password,
	}
}

// BaseURL returns the configured base URL (used by SSE which needs a raw http.Client).
func (c *Client) BaseURL() string { return c.base }

// AuthHeader returns the Authorization header value, or empty.
func (c *Client) AuthHeader() string {
	if c.pw == "" {
		return ""
	}
	return "Bearer " + c.pw
}

// Health pings the server's /app endpoint as a liveness check.
func (c *Client) Health(ctx context.Context) error {
	return c.http.Do(ctx, http.MethodGet, "/app", nil, nil, nil)
}

// Project mirrors the subset of opencode's project shape we need.
type Project struct {
	ID       string `json:"id"`
	Worktree string `json:"worktree"`
	VCS      string `json:"vcs"`
}

// CurrentProject returns the project opencode has active.
func (c *Client) CurrentProject(ctx context.Context) (*Project, error) {
	var p Project
	if err := c.http.Do(ctx, http.MethodGet, "/project/current", nil, &p, nil); err != nil {
		return nil, fmt.Errorf("get current project: %w", err)
	}
	return &p, nil
}

// EnsureGitProject calls POST /project/git/init which runs `git init` in
// opencode's current worktree. This is a no-op if the dir is already a git
// repo. Returns the resulting project — if its id is "global" it means
// opencode needs to be restarted for the new project entry to take effect
// (opencode only registers a real hash-id'd project when it starts inside
// an existing git repo).
func (c *Client) EnsureGitProject(ctx context.Context) (*Project, error) {
	var p Project
	if err := c.http.Do(ctx, http.MethodPost, "/project/git/init", map[string]any{}, &p, nil); err != nil {
		return nil, fmt.Errorf("project/git/init: %w", err)
	}
	return &p, nil
}

// CreateSession opens a new session and returns it.
func (c *Client) CreateSession(ctx context.Context, title string) (*Session, error) {
	body := map[string]any{}
	if title != "" {
		body["title"] = title
	}
	var s Session
	if err := c.http.Do(ctx, http.MethodPost, "/session", body, &s, nil); err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return &s, nil
}

// PostMessage sends a user message in a session and blocks until the model
// finishes. Prefer PostMessageAsync for non-trivial work — /message holds
// the HTTP connection open for the full model turn (potentially many
// minutes) and hits timeouts.
func (c *Client) PostMessage(ctx context.Context, sessionID string, req PostMessageRequest) error {
	if sessionID == "" {
		return fmt.Errorf("sessionID required")
	}
	if len(req.Parts) == 0 {
		return fmt.Errorf("parts required")
	}
	return c.http.Do(ctx, http.MethodPost, "/session/"+sessionID+"/message", req, nil, nil)
}

// PostMessageAsync submits a prompt and returns as soon as opencode has
// queued it. Completion is observed via the SSE /event stream — watch for
// message.completed / session.idle / session.error.
func (c *Client) PostMessageAsync(ctx context.Context, sessionID string, req PostMessageRequest) error {
	if sessionID == "" {
		return fmt.Errorf("sessionID required")
	}
	if len(req.Parts) == 0 {
		return fmt.Errorf("parts required")
	}
	return c.http.Do(ctx, http.MethodPost, "/session/"+sessionID+"/prompt_async", req, nil, nil)
}

// AbortSession cancels the in-flight turn for the session.
func (c *Client) AbortSession(ctx context.Context, sessionID string) error {
	return c.http.Do(ctx, http.MethodPost, "/session/"+sessionID+"/abort", nil, nil, nil)
}

// SessionMessage is the envelope opencode returns from
// GET /session/{id}/message. Only the fields we need are decoded.
type SessionMessage struct {
	Info  SessionMessageInfo `json:"info"`
	Parts []MessagePart      `json:"parts"`
}

type SessionMessageInfo struct {
	ID         string `json:"id"`
	Role       string `json:"role"` // "user" | "assistant"
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
}

// Messages returns the full message history for a session. Ordered oldest
// first — callers wanting the latest assistant reply should scan from the
// tail.
func (c *Client) Messages(ctx context.Context, sessionID string) ([]SessionMessage, error) {
	var out []SessionMessage
	if err := c.http.Do(ctx, http.MethodGet, "/session/"+sessionID+"/message", nil, &out, nil); err != nil {
		return nil, fmt.Errorf("get messages: %w", err)
	}
	return out, nil
}

// LastAssistantText returns the concatenated text of the most recent
// assistant message in the session, or "" if none. Useful for parsing
// sentinel markers the agent emits in its final response.
func (c *Client) LastAssistantText(ctx context.Context, sessionID string) (string, error) {
	msgs, err := c.Messages(ctx, sessionID)
	if err != nil {
		return "", err
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Info.Role != "assistant" {
			continue
		}
		var b strings.Builder
		for _, p := range m.Parts {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String(), nil
	}
	return "", nil
}
