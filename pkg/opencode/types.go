// Package opencode is a minimal client for an OpenCode server's HTTP/SSE API.
//
// We talk to the same `opencode serve` process the user runs locally (and the
// same image we'll pack into worker pods later). Only the surface area the
// orchestrator needs is implemented: create session, post message, stream
// events.
package opencode

import "time"

// Session is the subset of fields we need from POST /session.
type Session struct {
	ID        string    `json:"id"`
	Title     string    `json:"title,omitempty"`
	CreatedAt time.Time `json:"createdAt,omitempty"`
}

// MessagePart is one element of a chat message body. Only "text" is used.
type MessagePart struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

// PostMessageRequest matches POST /session/{id}/message.
type PostMessageRequest struct {
	ProviderID string        `json:"providerID,omitempty"`
	ModelID    string        `json:"modelID,omitempty"`
	Mode       string        `json:"mode,omitempty"`
	System     string        `json:"system,omitempty"`
	Parts      []MessagePart `json:"parts"`
}

// Event is one decoded SSE event from GET /event.
//
// OpenCode emits a wide schema; we keep the envelope and pass the raw
// properties through so callers (transcript, stage engine) can inspect what
// they care about without us tracking every type.
type Event struct {
	Type       string         `json:"type"`
	SessionID  string         `json:"sessionID,omitempty"`
	MessageID  string         `json:"messageID,omitempty"`
	Properties map[string]any `json:"properties,omitempty"`
	RawJSON    string         `json:"-"` // original line for transcript
}

// IsTerminal returns true for events that mark the session as fully done
// with the current prompt — i.e. no more work in flight, safe for the
// orchestrator to read the final assistant message.
//
// Intentionally does NOT include "message.completed": opencode emits that
// after every assistant turn, including ones that are about to kick off
// tool calls that will produce further assistant messages. Relying on it
// made us grab the wrong "final" message for plan extraction.
func (e Event) IsTerminal() bool {
	switch e.Type {
	case "session.idle", "session.error":
		return true
	}
	return false
}
