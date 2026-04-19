// Package httpclient provides a thin wrapper around net/http with sensible
// defaults for timeouts, retries, and structured logging. All external
// integrations (Jira, ClickUp, GitLab, GitHub, opencode) should use this.
package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"time"

	"github.com/rs/zerolog"
)

type Options struct {
	BaseURL        string
	Timeout        time.Duration
	MaxRetries     int
	BackoffBase    time.Duration
	BackoffMax     time.Duration
	DefaultHeaders map[string]string
}

type Client struct {
	base    string
	http    *http.Client
	retries int
	backoff time.Duration
	maxBack time.Duration
	headers map[string]string
	logger  zerolog.Logger
}

func New(opts Options, logger zerolog.Logger) *Client {
	if opts.Timeout == 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.BackoffBase == 0 {
		opts.BackoffBase = 250 * time.Millisecond
	}
	if opts.BackoffMax == 0 {
		opts.BackoffMax = 5 * time.Second
	}
	return &Client{
		base:    opts.BaseURL,
		http:    &http.Client{Timeout: opts.Timeout},
		retries: opts.MaxRetries,
		backoff: opts.BackoffBase,
		maxBack: opts.BackoffMax,
		headers: opts.DefaultHeaders,
		logger:  logger,
	}
}

// Do performs a request with retry+backoff on transient failures. The body, if
// non-nil, is JSON-encoded. If out is non-nil, the response body is decoded
// into it. Non-2xx responses return a *HTTPError.
func (c *Client) Do(ctx context.Context, method, path string, body, out any, extraHeaders http.Header) error {
	url := path
	if c.base != "" {
		url = c.base + path
	}

	var bodyBytes []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
		}
		bodyBytes = b
	}

	var lastErr error
	for attempt := 0; attempt <= c.retries; attempt++ {
		if attempt > 0 {
			wait := c.backoffFor(attempt)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}

		var reqBody io.Reader
		if bodyBytes != nil {
			reqBody = bytes.NewReader(bodyBytes)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
		if err != nil {
			return fmt.Errorf("new request: %w", err)
		}
		if bodyBytes != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		for k, vv := range extraHeaders {
			for _, v := range vv {
				req.Header.Add(k, v)
			}
		}

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			c.logger.Debug().Err(err).Str("method", method).Str("url", url).Int("attempt", attempt).Msg("http transport error")
			continue
		}

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode >= 500 || resp.StatusCode == 429 {
			lastErr = &HTTPError{Status: resp.StatusCode, Body: string(respBody), URL: url, Method: method}
			c.logger.Debug().Int("status", resp.StatusCode).Str("method", method).Str("url", url).Int("attempt", attempt).Msg("http retryable")
			continue
		}
		if resp.StatusCode >= 400 {
			return &HTTPError{Status: resp.StatusCode, Body: string(respBody), URL: url, Method: method}
		}
		if out != nil && len(respBody) > 0 {
			if err := json.Unmarshal(respBody, out); err != nil {
				return fmt.Errorf("decode response: %w (body=%s)", err, truncate(string(respBody), 200))
			}
		}
		return nil
	}
	return fmt.Errorf("exhausted %d retries: %w", c.retries, lastErr)
}

func (c *Client) backoffFor(attempt int) time.Duration {
	d := time.Duration(float64(c.backoff) * math.Pow(2, float64(attempt-1)))
	if d > c.maxBack {
		d = c.maxBack
	}
	// +/- 20% jitter
	j := time.Duration(rand.Int63n(int64(d) / 5))
	return d + j
}

type HTTPError struct {
	Status int
	Body   string
	URL    string
	Method string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s %s: %d %s", e.Method, e.URL, e.Status, truncate(e.Body, 200))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
