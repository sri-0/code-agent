package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Stream subscribes to GET /event and pushes decoded Events on the returned
// channel until ctx is cancelled or the connection drops. The returned errCh
// emits at most one error then closes.
//
// Reconnects are the caller's responsibility; this is a one-shot stream so
// callers can cleanly distinguish "stream ended because ctx done" from
// "transport error". Phase 4 wires a reconnect loop in pkg/runtime/local.
func (c *Client) Stream(ctx context.Context) (<-chan Event, <-chan error, error) {
	url := c.base + "/event"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if a := c.AuthHeader(); a != "" {
		req.Header.Set("Authorization", a)
	}

	httpClient := &http.Client{
		Timeout: 0, // long-lived
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("sse connect: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		return nil, nil, fmt.Errorf("sse: status %d: %s", resp.StatusCode, string(body))
	}

	events := make(chan Event, 32)
	errs := make(chan error, 1)

	go func() {
		defer close(events)
		defer close(errs)
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		// SSE lines can be large (e.g. tool outputs); raise the buffer.
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 4*1024*1024)

		var dataBuf strings.Builder
		var eventName string

		flush := func() {
			if dataBuf.Len() == 0 {
				return
			}
			raw := dataBuf.String()
			dataBuf.Reset()
			defer func() { eventName = "" }()

			ev := Event{RawJSON: raw}
			if err := json.Unmarshal([]byte(raw), &ev); err != nil {
				// not JSON; emit as opaque event with type from `event:` header
				ev = Event{Type: eventName, RawJSON: raw}
			}
			if ev.Type == "" && eventName != "" {
				ev.Type = eventName
			}
			select {
			case events <- ev:
			case <-ctx.Done():
			}
		}

		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case line == "":
				flush()
			case strings.HasPrefix(line, ":"):
				// SSE comment / keepalive
			case strings.HasPrefix(line, "event:"):
				eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				if dataBuf.Len() > 0 {
					dataBuf.WriteByte('\n')
				}
				dataBuf.WriteString(strings.TrimPrefix(line, "data:"))
				// SSE allows a leading space after data:
				if strings.HasPrefix(dataBuf.String(), " ") {
					dataBuf.Reset()
					dataBuf.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
				}
			}
			if ctx.Err() != nil {
				return
			}
		}
		if err := scanner.Err(); err != nil && ctx.Err() == nil {
			errs <- fmt.Errorf("sse read: %w", err)
		}
	}()

	return events, errs, nil
}

// WaitForReady polls /app until the server responds OK or the deadline hits.
func (c *Client) WaitForReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	backoff := 200 * time.Millisecond
	for {
		if err := c.Health(ctx); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("opencode not ready after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 2*time.Second {
			backoff *= 2
		}
	}
}
