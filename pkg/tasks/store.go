package tasks

import (
	"context"
	"errors"
)

var ErrNotFound = errors.New("task not found")

// Store is the persistence interface for tasks. Implementations live in
// subpackages (e.g. pkg/tasks/valkey).
type Store interface {
	Create(ctx context.Context, t *Task) error
	Update(ctx context.Context, t *Task) error
	Get(ctx context.Context, id string) (*Task, error)
	GetByExternal(ctx context.Context, boardID, externalID string) (*Task, error)
	ListByBoard(ctx context.Context, boardID string) ([]*Task, error)
	ListByStage(ctx context.Context, stage string) ([]*Task, error)
	Delete(ctx context.Context, id string) error
}
