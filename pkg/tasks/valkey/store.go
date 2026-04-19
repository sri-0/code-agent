// Package valkey is the Valkey/Redis-backed implementation of tasks.Store.
//
// Keying scheme:
//
//	task:{id}                    -> JSON blob
//	task:ext:{board}:{externalID} -> id  (secondary index)
//	board:{id}:tasks             -> set of ids
//	stage:{name}:tasks           -> set of ids
package valkey

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"code-agent/pkg/db/valkey"
	"code-agent/pkg/tasks"

	vk "github.com/valkey-io/valkey-go"
)

type Store struct {
	v *valkey.Valkey
}

func New(v *valkey.Valkey) *Store { return &Store{v: v} }

func keyTask(id string) string        { return "task:" + id }
func keyExt(board, ext string) string { return "task:ext:" + board + ":" + ext }
func keyBoard(board string) string    { return "board:" + board + ":tasks" }
func keyStage(stage string) string    { return "stage:" + stage + ":tasks" }

func (s *Store) Create(ctx context.Context, t *tasks.Task) error {
	now := time.Now()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	return s.writeAll(ctx, t, "")
}

func (s *Store) Update(ctx context.Context, t *tasks.Task) error {
	prev, err := s.Get(ctx, t.ID)
	if err != nil && err != tasks.ErrNotFound {
		return err
	}
	prevStage := ""
	if prev != nil {
		prevStage = prev.Stage
	}
	t.UpdatedAt = time.Now()
	return s.writeAll(ctx, t, prevStage)
}

func (s *Store) writeAll(ctx context.Context, t *tasks.Task, prevStage string) error {
	blob, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("marshal task: %w", err)
	}
	if err := s.v.Set(ctx, keyTask(t.ID), string(blob), 0); err != nil {
		return err
	}
	if err := s.v.Set(ctx, keyExt(t.BoardID, t.ExternalID), t.ID, 0); err != nil {
		return err
	}
	if err := s.v.SAdd(ctx, keyBoard(t.BoardID), t.ID); err != nil {
		return err
	}
	if prevStage != "" && prevStage != t.Stage {
		if err := s.v.SRem(ctx, keyStage(prevStage), t.ID); err != nil {
			return err
		}
	}
	if t.Stage != "" {
		if err := s.v.SAdd(ctx, keyStage(t.Stage), t.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Get(ctx context.Context, id string) (*tasks.Task, error) {
	raw, err := s.v.Get(ctx, keyTask(id))
	if err != nil {
		if vk.IsValkeyNil(err) {
			return nil, tasks.ErrNotFound
		}
		return nil, err
	}
	var t tasks.Task
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return nil, fmt.Errorf("unmarshal task: %w", err)
	}
	return &t, nil
}

func (s *Store) GetByExternal(ctx context.Context, boardID, externalID string) (*tasks.Task, error) {
	id, err := s.v.Get(ctx, keyExt(boardID, externalID))
	if err != nil {
		if vk.IsValkeyNil(err) {
			return nil, tasks.ErrNotFound
		}
		return nil, err
	}
	return s.Get(ctx, id)
}

func (s *Store) ListByBoard(ctx context.Context, boardID string) ([]*tasks.Task, error) {
	ids, err := s.v.SMembers(ctx, keyBoard(boardID))
	if err != nil {
		return nil, err
	}
	return s.getMany(ctx, ids)
}

func (s *Store) ListByStage(ctx context.Context, stage string) ([]*tasks.Task, error) {
	ids, err := s.v.SMembers(ctx, keyStage(stage))
	if err != nil {
		return nil, err
	}
	return s.getMany(ctx, ids)
}

func (s *Store) getMany(ctx context.Context, ids []string) ([]*tasks.Task, error) {
	out := make([]*tasks.Task, 0, len(ids))
	for _, id := range ids {
		t, err := s.Get(ctx, id)
		if err != nil {
			if err == tasks.ErrNotFound {
				continue
			}
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func (s *Store) Delete(ctx context.Context, id string) error {
	t, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := s.v.Del(ctx, keyTask(id), keyExt(t.BoardID, t.ExternalID)); err != nil {
		return err
	}
	if err := s.v.SRem(ctx, keyBoard(t.BoardID), id); err != nil {
		return err
	}
	if t.Stage != "" {
		if err := s.v.SRem(ctx, keyStage(t.Stage), id); err != nil {
			return err
		}
	}
	return nil
}
