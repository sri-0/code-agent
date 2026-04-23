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
	// directory, when non-empty, is attached as the `x-opencode-directory`
	// header on every request. Opencode's InstanceMiddleware resolves
	// this to a per-request project/worktree — letting multiple projects
	// live under one server (e.g. /workspace/repoA vs /workspace/repoB).
	// See packages/opencode/src/server/routes/instance/middleware.ts.
	directory string
}

type Options struct {
	BaseURL  string // e.g. http://localhost:4096
	Password string // OPENCODE_SERVER_PASSWORD; empty for local dev
	Timeout  time.Duration
}

func New(opts Options, logger zerolog.Logger) *Client {
	// PostMessage is synchronous — it blocks for the entire model turn
	// (can be many minutes). pkg/httpclient's default timeout is 30s,
	// which would cut it off. Set a generous 2-hour cap; the per-call
	// context (stage.Timeout, ~45m) bounds it tighter in practice.
	// Short utility calls (Health, CreateSession) wrap their own
	// shorter context at the call site.
	if opts.Timeout == 0 {
		opts.Timeout = 2 * time.Hour
	}
	headers := map[string]string{}
	if opts.Password != "" {
		headers["Authorization"] = "Bearer " + opts.Password
	}
	hc := httpclient.New(httpclient.Options{
		BaseURL: strings.TrimRight(opts.BaseURL, "/"),
		Timeout: opts.Timeout,
		// No retries — PostMessage is not idempotent. A transport-level
		// retry would submit the same user message twice (or more), and
		// opencode queues each one separately. Leave retry decisions to
		// the caller (stage engine re-entry + action-level ctx).
		MaxRetries:     0,
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

// WithDirectory returns a shallow clone of the client with its per-request
// opencode directory pinned to dir. Every request made via the returned
// client carries `x-opencode-directory: <dir>`, which opencode uses to
// resolve the project/worktree. Empty dir = unpinned.
func (c *Client) WithDirectory(dir string) *Client {
	cp := *c
	cp.directory = dir
	return &cp
}

// Directory returns the directory this client is scoped to, or empty.
func (c *Client) Directory() string { return c.directory }

// do wraps http.Do injecting the directory header when set.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var h http.Header
	if c.directory != "" {
		h = http.Header{}
		h.Set("x-opencode-directory", c.directory)
	}
	return c.http.Do(ctx, method, path, body, out, h)
}

// AuthHeader returns the Authorization header value, or empty.
func (c *Client) AuthHeader() string {
	if c.pw == "" {
		return ""
	}
	return "Bearer " + c.pw
}

// Health pings the server's /app endpoint as a liveness check.
func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/app", nil, nil)
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
	if err := c.do(ctx, http.MethodGet, "/project/current", nil, &p); err != nil {
		return nil, fmt.Errorf("get current project: %w", err)
	}
	return &p, nil
}

// ConfigSnapshot is the subset of GET /config we care about. Anything we
// don't decode is ignored.
type ConfigSnapshot struct {
	Model      string            `json:"model"`
	Permission map[string]string `json:"permission"`
}

// Config fetches the server's effective config.
func (c *Client) Config(ctx context.Context) (*ConfigSnapshot, error) {
	var s ConfigSnapshot
	if err := c.do(ctx, http.MethodGet, "/config", nil, &s); err != nil {
		return nil, fmt.Errorf("get config: %w", err)
	}
	return &s, nil
}

// EnsureGitProject calls POST /project/git/init which runs `git init` in
// opencode's current worktree. This is a no-op if the dir is already a git
// repo. Returns the resulting project — if its id is "global" it means
// opencode needs to be restarted for the new project entry to take effect
// (opencode only registers a real hash-id'd project when it starts inside
// an existing git repo).
func (c *Client) EnsureGitProject(ctx context.Context) (*Project, error) {
	var p Project
	if err := c.do(ctx, http.MethodPost, "/project/git/init", map[string]any{}, &p); err != nil {
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
	if err := c.do(ctx, http.MethodPost, "/session", body, &s); err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return &s, nil
}

// PostMessage appends a user message to a session without executing it.
// Despite the "message" name this is fire-and-forget from opencode 1.14 —
// it returns ~immediately while any running turn continues in the
// background, and it does NOT kick off a new model turn on its own.
// Callers that want the model to respond should use PostMessageAsync.
func (c *Client) PostMessage(ctx context.Context, sessionID string, req PostMessageRequest) error {
	if sessionID == "" {
		return fmt.Errorf("sessionID required")
	}
	if len(req.Parts) == 0 {
		return fmt.Errorf("parts required")
	}
	return c.do(ctx, http.MethodPost, "/session/"+sessionID+"/message", req, nil)
}

// PostMessageAsync submits a prompt and returns as soon as opencode has
// queued it. Completion is observed by polling GET /session/{id}/message
// (via WaitForTurn) — SSE terminal events are unreliable on provider
// errors.
func (c *Client) PostMessageAsync(ctx context.Context, sessionID string, req PostMessageRequest) error {
	if sessionID == "" {
		return fmt.Errorf("sessionID required")
	}
	if len(req.Parts) == 0 {
		return fmt.Errorf("parts required")
	}
	return c.do(ctx, http.MethodPost, "/session/"+sessionID+"/prompt_async", req, nil)
}

// AbortSession cancels the in-flight turn for the session.
func (c *Client) AbortSession(ctx context.Context, sessionID string) error {
	return c.do(ctx, http.MethodPost, "/session/"+sessionID+"/abort", nil, nil)
}

// SessionMessage is the envelope opencode returns from
// GET /session/{id}/message. Only the fields we need are decoded.
type SessionMessage struct {
	Info  SessionMessageInfo `json:"info"`
	Parts []MessagePart      `json:"parts"`
}

type SessionMessageInfo struct {
	ID         string           `json:"id"`
	Role       string           `json:"role"` // "user" | "assistant"
	ProviderID string           `json:"providerID"`
	ModelID    string           `json:"modelID"`
	Time       MessageTimeInfo  `json:"time"`
	Error      *MessageErrorBox `json:"error,omitempty"`
}

// MessageTimeInfo is the message lifecycle timestamps opencode attaches to
// every message. `completed` is only set once the full turn is done
// (including any tool calls + subagents the assistant fires off). Zero
// means the turn is still in flight.
type MessageTimeInfo struct {
	Created   int64 `json:"created"`
	Completed int64 `json:"completed"`
}

// MessageErrorBox is opencode's error shape on assistant messages when the
// provider call fails (credit limit, rate limit, auth, 5xx, etc.). The
// inner Data.Message is what we surface to the user.
type MessageErrorBox struct {
	Name string           `json:"name"`
	Data MessageErrorData `json:"data"`
}

type MessageErrorData struct {
	Message    string `json:"message"`
	StatusCode int    `json:"statusCode,omitempty"`
}

// Messages returns the full message history for a session. Ordered oldest
// first — callers wanting the latest assistant reply should scan from the
// tail.
func (c *Client) Messages(ctx context.Context, sessionID string) ([]SessionMessage, error) {
	var out []SessionMessage
	if err := c.do(ctx, http.MethodGet, "/session/"+sessionID+"/message", nil, &out); err != nil {
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

// TurnResult summarises the outcome of a single prompt turn as observed
// from opencode's message history.
type TurnResult struct {
	// Message is the final assistant message (the one whose time.completed
	// first becomes non-zero after we submitted the prompt).
	Message SessionMessage
	// Text is the concatenated text parts of the final assistant message.
	Text string
	// Error, if non-nil, is the provider error opencode recorded on the
	// message (credit limit, rate limit, auth, etc.). Turn is still
	// "completed" from opencode's POV, but the caller should treat it
	// as a failure and surface the reason.
	Error *MessageErrorBox
	// ToolCalls is the number of tool-use parts observed in the final
	// assistant message. Zero indicates the model never executed any
	// tools this turn — often a symptom of a silent failure.
	ToolCalls int
}

// WaitForTurn polls GET /session/{id}/message until the tail assistant
// message's time.completed becomes non-zero, or ctx is cancelled. `after`
// is the Unix-ms cutoff: we only consider assistant messages created at
// or after this time so a stale completed message from a prior turn
// can't short-circuit the wait.
//
// We poll instead of relying on SSE terminal events because opencode
// does not always emit session.idle / session.error on provider failures
// (observed with 402/credit errors — the server logs the error, sets
// time.completed, and never fires an SSE terminal).
func (c *Client) WaitForTurn(ctx context.Context, sessionID string, after int64) (*TurnResult, error) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		res, err := c.checkTurn(ctx, sessionID, after)
		if err != nil {
			return nil, err
		}
		if res != nil {
			return res, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *Client) checkTurn(ctx context.Context, sessionID string, after int64) (*TurnResult, error) {
	msgs, err := c.Messages(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Info.Role != "assistant" {
			continue
		}
		if m.Info.Time.Created < after {
			// Older assistant message from a prior prompt; earlier messages
			// are only older, so stop scanning.
			return nil, nil
		}
		if m.Info.Time.Completed == 0 {
			return nil, nil
		}
		res := &TurnResult{Message: m, Error: m.Info.Error}
		var b strings.Builder
		for _, p := range m.Parts {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
			if p.Type == "tool" {
				res.ToolCalls++
			}
		}
		res.Text = b.String()
		return res, nil
	}
	return nil, nil
}
