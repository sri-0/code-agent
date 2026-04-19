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

// PostMessage sends a user message in a session.
func (c *Client) PostMessage(ctx context.Context, sessionID string, req PostMessageRequest) error {
	if sessionID == "" {
		return fmt.Errorf("sessionID required")
	}
	if len(req.Parts) == 0 {
		return fmt.Errorf("parts required")
	}
	return c.http.Do(ctx, http.MethodPost, "/session/"+sessionID+"/message", req, nil, nil)
}

// AbortSession cancels the in-flight turn for the session.
func (c *Client) AbortSession(ctx context.Context, sessionID string) error {
	return c.http.Do(ctx, http.MethodPost, "/session/"+sessionID+"/abort", nil, nil, nil)
}
