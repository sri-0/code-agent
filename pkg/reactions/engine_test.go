package reactions

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"code-agent/internal/config"
	"code-agent/pkg/notifications"
	"code-agent/pkg/tasks"
)

type stubAction struct {
	name  string
	calls int
	err   error
	mu    sync.Mutex
}

func (s *stubAction) Name() string { return s.name }
func (s *stubAction) Run(ctx context.Context, e Event) error {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return s.err
}
func (s *stubAction) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type stubRouter struct {
	calls int
}

func (s *stubRouter) Send(ctx context.Context, n notifications.Notification) error {
	s.calls++
	return nil
}

type stubStore struct{ writes int }

func (s *stubStore) Create(ctx context.Context, t *tasks.Task) error { return nil }
func (s *stubStore) Update(ctx context.Context, t *tasks.Task) error { s.writes++; return nil }
func (s *stubStore) Get(ctx context.Context, id string) (*tasks.Task, error) {
	return nil, tasks.ErrNotFound
}
func (s *stubStore) GetByExternal(ctx context.Context, board, ext string) (*tasks.Task, error) {
	return nil, tasks.ErrNotFound
}
func (s *stubStore) ListByBoard(ctx context.Context, board string) ([]*tasks.Task, error) {
	return nil, nil
}
func (s *stubStore) ListByStage(ctx context.Context, stage string) ([]*tasks.Task, error) {
	return nil, nil
}
func (s *stubStore) Delete(ctx context.Context, id string) error { return nil }

func newEngine(t *testing.T) (*Engine, *stubAction, *stubAction, *stubRouter, *stubStore) {
	t.Helper()
	logger := zerolog.Nop()
	router := &stubRouter{}
	store := &stubStore{}
	e := NewEngine(Deps{Tasks: store, Notifications: router, Logger: logger})
	send := &stubAction{name: "send-to-agent"}
	notify := &stubAction{name: "notify"}
	e.Register(send)
	e.Register(notify)
	return e, send, notify, router, store
}

func makeEvent(key EventKey, taskID string) Event {
	t := &tasks.Task{ID: taskID, ExternalID: "TICK-1", BoardID: "alpha"}
	return Event{
		Key:          key,
		Task:         t,
		Board:        config.Board{ID: "alpha"},
		EvidenceHash: "abc123",
	}
}

func TestProcess_FiresAction(t *testing.T) {
	e, send, _, _, _ := newEngine(t)
	e.Process(context.Background(), makeEvent(EventCIFailed, "t1"))
	if send.Calls() != 1 {
		t.Errorf("send-to-agent calls = %d, want 1", send.Calls())
	}
}

func TestProcess_DedupesSameEvidence(t *testing.T) {
	e, send, _, _, _ := newEngine(t)
	evt := makeEvent(EventCIFailed, "t1")
	e.Process(context.Background(), evt)
	e.Process(context.Background(), evt) // same evidence hash
	if send.Calls() != 1 {
		t.Errorf("expected dedupe, got %d calls", send.Calls())
	}
}

func TestProcess_RetryCapEscalatesToNotify(t *testing.T) {
	e, send, _, router, _ := newEngine(t)
	// Default config: ci-failed Retries=2, so Retries+1 = 3 fires allowed.
	task := &tasks.Task{ID: "t1", ExternalID: "TICK-1", BoardID: "alpha"}
	for _, h := range []string{"h1", "h2", "h3"} {
		evt := Event{
			Key: EventCIFailed, Task: task,
			Board:        config.Board{ID: "alpha"},
			EvidenceHash: h,
		}
		e.Process(context.Background(), evt)
	}
	if send.Calls() != 3 {
		t.Fatalf("setup: expected 3 send calls, got %d", send.Calls())
	}
	// 4th attempt with new evidence should hit the retry cap → notify.
	evt4 := Event{
		Key: EventCIFailed, Task: task,
		Board:        config.Board{ID: "alpha"},
		EvidenceHash: "h4",
	}
	e.Process(context.Background(), evt4)
	if send.Calls() != 3 {
		t.Errorf("send calls grew past cap: %d", send.Calls())
	}
	if router.calls < 1 {
		t.Errorf("expected escalation notify, got %d", router.calls)
	}
}

func TestProcess_AutoFalseRoutesToNotify(t *testing.T) {
	e, send, _, router, _ := newEngine(t)
	auto := false
	evt := makeEvent(EventCIFailed, "t1")
	evt.Board.Reactions = map[string]config.ReactionConfig{
		"ci-failed": {Auto: &auto, Action: "send-to-agent"},
	}
	e.Process(context.Background(), evt)
	if send.Calls() != 0 {
		t.Errorf("auto=false should skip send, got %d calls", send.Calls())
	}
	if router.calls != 1 {
		t.Errorf("expected notify, got %d", router.calls)
	}
}

func TestProcess_ActionFailureEscalates(t *testing.T) {
	logger := zerolog.Nop()
	router := &stubRouter{}
	store := &stubStore{}
	e := NewEngine(Deps{Tasks: store, Notifications: router, Logger: logger})
	failing := &stubAction{name: "send-to-agent", err: errors.New("boom")}
	e.Register(failing)
	e.Process(context.Background(), makeEvent(EventCIFailed, "t1"))
	if failing.Calls() != 1 {
		t.Errorf("expected one attempt, got %d", failing.Calls())
	}
	if router.calls != 1 {
		t.Errorf("expected escalation notify on failure, got %d", router.calls)
	}
}

func TestDeriveEvents(t *testing.T) {
	prev := tasks.Lifecycle{
		Session: tasks.SessionTrack{State: tasks.SessionStateWorking},
		PR:      tasks.PRTrack{State: tasks.PRStateOpen, Reason: tasks.PRReasonInProgress},
	}
	tests := []struct {
		name string
		next tasks.Lifecycle
		want []EventKey
	}{
		{
			name: "ci goes failing",
			next: tasks.Lifecycle{
				Session: tasks.SessionTrack{State: tasks.SessionStateWorking},
				PR:      tasks.PRTrack{State: tasks.PRStateOpen, Reason: tasks.PRReasonCIFailing},
			},
			want: []EventKey{EventCIFailed},
		},
		{
			name: "changes requested",
			next: tasks.Lifecycle{
				Session: tasks.SessionTrack{State: tasks.SessionStateWorking},
				PR:      tasks.PRTrack{State: tasks.PRStateOpen, Reason: tasks.PRReasonChangesRequested},
			},
			want: []EventKey{EventChangesRequested},
		},
		{
			name: "approved + green",
			next: tasks.Lifecycle{
				Session: tasks.SessionTrack{State: tasks.SessionStateIdle},
				PR:      tasks.PRTrack{State: tasks.PRStateOpen, Reason: tasks.PRReasonMergeReady},
			},
			want: []EventKey{EventApprovedAndGreen},
		},
		{
			name: "stuck",
			next: tasks.Lifecycle{
				Session: tasks.SessionTrack{State: tasks.SessionStateStuck},
				PR:      tasks.PRTrack{State: tasks.PRStateOpen, Reason: tasks.PRReasonInProgress},
			},
			want: []EventKey{EventAgentStuck},
		},
		{
			name: "merged",
			next: tasks.Lifecycle{
				Session: tasks.SessionTrack{State: tasks.SessionStateIdle},
				PR:      tasks.PRTrack{State: tasks.PRStateMerged, Reason: tasks.PRReasonMerged},
			},
			want: []EventKey{EventPRMerged},
		},
		{
			name: "no change",
			next: prev,
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveEvents(prev, tc.next)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i, k := range got {
				if k != tc.want[i] {
					t.Errorf("got %v at %d, want %v", k, i, tc.want[i])
				}
			}
		})
	}
}
