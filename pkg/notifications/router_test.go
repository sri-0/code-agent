package notifications

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"
)

type stubNotifier struct {
	name  string
	calls int
	err   error
}

func (s *stubNotifier) Name() string { return s.name }
func (s *stubNotifier) Send(ctx context.Context, n Notification) error {
	s.calls++
	return s.err
}

func TestFanoutRouter_FansToConfiguredBackends(t *testing.T) {
	mm := &stubNotifier{name: "mattermost"}
	teams := &stubNotifier{name: "teams"}
	tk := &stubNotifier{name: "ticket"}
	r := NewFanoutRouter(zerolog.Nop(), DefaultRouting, mm, teams, tk)

	if err := r.Send(context.Background(), Notification{Priority: PriorityUrgent}); err != nil {
		t.Fatal(err)
	}
	if mm.calls != 1 || teams.calls != 1 || tk.calls != 1 {
		t.Errorf("urgent should hit all 3, got mm=%d teams=%d ticket=%d", mm.calls, teams.calls, tk.calls)
	}

	if err := r.Send(context.Background(), Notification{Priority: PriorityInfo}); err != nil {
		t.Fatal(err)
	}
	if mm.calls != 2 {
		t.Errorf("info should hit mattermost only; mm=%d", mm.calls)
	}
	if teams.calls != 1 || tk.calls != 1 {
		t.Errorf("info should not hit teams/ticket; teams=%d ticket=%d", teams.calls, tk.calls)
	}
}

func TestFanoutRouter_TolerantOfBackendErrors(t *testing.T) {
	mm := &stubNotifier{name: "mattermost", err: errors.New("boom")}
	tk := &stubNotifier{name: "ticket"}
	r := NewFanoutRouter(zerolog.Nop(), DefaultRouting, mm, tk)

	// urgent → both attempted; mm errors but ticket should still be called
	if err := r.Send(context.Background(), Notification{Priority: PriorityUrgent}); err != nil {
		t.Fatal(err)
	}
	if mm.calls != 1 || tk.calls != 1 {
		t.Errorf("expected both attempted; mm=%d ticket=%d", mm.calls, tk.calls)
	}
}

func TestFanoutRouter_NoBackendsForPriorityIsHarmless(t *testing.T) {
	mm := &stubNotifier{name: "mattermost"}
	r := NewFanoutRouter(zerolog.Nop(), Routing{
		PriorityUrgent: {"mattermost"},
	}, mm)
	// info isn't in routing → drop silently.
	if err := r.Send(context.Background(), Notification{Priority: PriorityInfo}); err != nil {
		t.Fatal(err)
	}
	if mm.calls != 0 {
		t.Errorf("info should be dropped; mm=%d", mm.calls)
	}
}
