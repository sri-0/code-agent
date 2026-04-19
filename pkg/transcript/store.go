// Package transcript stores per-session opencode SSE event streams so a
// session's chat history survives worker death.
//
// Storage layout (valkey):
//   - transcript:{session_id}        -> RPUSH-able list of JSON event lines
//   - transcript:{session_id}:meta   -> JSON { task_id, board_id, started_at, last_at }
package transcript

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	valkeylib "github.com/valkey-io/valkey-go"

	"code-agent/pkg/db/valkey"
)

const (
	maxLen = 10_000 // keep at most this many events per session
)

// Meta is small per-session metadata.
type Meta struct {
	SessionID  string    `json:"session_id"`
	TaskID     string    `json:"task_id"`
	BoardID    string    `json:"board_id"`
	StartedAt  time.Time `json:"started_at"`
	LastAt     time.Time `json:"last_at"`
	EventCount int       `json:"event_count"`
}

// Store persists transcript events.
type Store interface {
	Init(ctx context.Context, m Meta) error
	Append(ctx context.Context, sessionID string, raw string) error
	Read(ctx context.Context, sessionID string, limit int) ([]string, error)
	GetMeta(ctx context.Context, sessionID string) (Meta, error)
}

// ValkeyStore is the default backend.
type ValkeyStore struct{ vk *valkey.Valkey }

func NewValkey(vk *valkey.Valkey) *ValkeyStore { return &ValkeyStore{vk: vk} }

func keyList(sid string) string { return "transcript:" + sid }
func keyMeta(sid string) string { return "transcript:" + sid + ":meta" }

func (s *ValkeyStore) Init(ctx context.Context, m Meta) error {
	if m.StartedAt.IsZero() {
		m.StartedAt = time.Now()
	}
	if m.LastAt.IsZero() {
		m.LastAt = m.StartedAt
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	cmd := s.vk.Client.B().Set().Key(keyMeta(m.SessionID)).Value(string(b)).Build()
	return s.vk.Client.Do(ctx, cmd).Error()
}

func (s *ValkeyStore) Append(ctx context.Context, sessionID, raw string) error {
	c := s.vk.Client
	push := c.B().Rpush().Key(keyList(sessionID)).Element(raw).Build()
	if err := c.Do(ctx, push).Error(); err != nil {
		return fmt.Errorf("rpush: %w", err)
	}
	trim := c.B().Ltrim().Key(keyList(sessionID)).Start(int64(-maxLen)).Stop(-1).Build()
	if err := c.Do(ctx, trim).Error(); err != nil {
		return fmt.Errorf("ltrim: %w", err)
	}
	// best-effort meta update
	m, err := s.GetMeta(ctx, sessionID)
	if err == nil {
		m.LastAt = time.Now()
		m.EventCount++
		if b, err := json.Marshal(m); err == nil {
			c.Do(ctx, c.B().Set().Key(keyMeta(sessionID)).Value(string(b)).Build())
		}
	}
	return nil
}

func (s *ValkeyStore) Read(ctx context.Context, sessionID string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 1000
	}
	c := s.vk.Client
	cmd := c.B().Lrange().Key(keyList(sessionID)).Start(int64(-limit)).Stop(-1).Build()
	res := c.Do(ctx, cmd)
	if err := res.Error(); err != nil {
		return nil, err
	}
	arr, err := res.AsStrSlice()
	if err != nil {
		if isNil(err) {
			return nil, nil
		}
		return nil, err
	}
	return arr, nil
}

func (s *ValkeyStore) GetMeta(ctx context.Context, sessionID string) (Meta, error) {
	c := s.vk.Client
	cmd := c.B().Get().Key(keyMeta(sessionID)).Build()
	res := c.Do(ctx, cmd)
	if err := res.Error(); err != nil {
		if isNil(err) {
			return Meta{}, nil
		}
		return Meta{}, err
	}
	str, err := res.ToString()
	if err != nil {
		return Meta{}, err
	}
	var m Meta
	if err := json.Unmarshal([]byte(str), &m); err != nil {
		return Meta{}, err
	}
	return m, nil
}

func isNil(err error) bool {
	if err == nil {
		return false
	}
	return valkeylib.IsValkeyNil(err)
}
