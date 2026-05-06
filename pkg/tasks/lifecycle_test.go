package tasks

import (
	"testing"
	"time"
)

func TestDeriveStage(t *testing.T) {
	tests := []struct {
		name string
		l    Lifecycle
		want string
	}{
		{
			name: "fresh spawn",
			l: Lifecycle{Session: SessionTrack{
				State: SessionStateNotStarted, Reason: SessionReasonSpawnRequested,
			}},
			want: "spawning",
		},
		{
			name: "agent working, no pr",
			l: Lifecycle{Session: SessionTrack{
				State: SessionStateWorking, Reason: SessionReasonTaskInProgress,
			}},
			want: "working",
		},
		{
			name: "pr open, ci failing",
			l: Lifecycle{
				Session: SessionTrack{State: SessionStateWorking},
				PR:      PRTrack{State: PRStateOpen, Reason: PRReasonCIFailing},
			},
			want: "ci_failed",
		},
		{
			name: "pr open, changes requested",
			l: Lifecycle{
				Session: SessionTrack{State: SessionStateIdle},
				PR:      PRTrack{State: PRStateOpen, Reason: PRReasonChangesRequested},
			},
			want: "changes_requested",
		},
		{
			name: "pr open, mergeable",
			l: Lifecycle{
				Session: SessionTrack{State: SessionStateIdle},
				PR:      PRTrack{State: PRStateOpen, Reason: PRReasonMergeReady},
			},
			want: "mergeable",
		},
		{
			name: "pr merged dominates session",
			l: Lifecycle{
				Session: SessionTrack{State: SessionStateWorking},
				PR:      PRTrack{State: PRStateMerged, Reason: PRReasonMerged},
			},
			want: "merged",
		},
		{
			name: "stuck",
			l: Lifecycle{Session: SessionTrack{
				State: SessionStateStuck, Reason: SessionReasonProbeFailure,
			}},
			want: "stuck",
		},
		{
			name: "terminated by error",
			l: Lifecycle{Session: SessionTrack{
				State: SessionStateTerminated, Reason: SessionReasonErrorInProcess,
			}},
			want: "errored",
		},
		{
			name: "terminated by manual kill",
			l: Lifecycle{Session: SessionTrack{
				State: SessionStateTerminated, Reason: SessionReasonManuallyKilled,
			}},
			want: "killed",
		},
		{
			name: "terminated by auto cleanup after merge",
			l: Lifecycle{Session: SessionTrack{
				State: SessionStateTerminated, Reason: SessionReasonPRMerged,
			}},
			want: "cleanup",
		},
		{
			name: "needs input dominates pr",
			l: Lifecycle{
				Session: SessionTrack{State: SessionStateNeedsInput, Reason: SessionReasonAwaitingUserInput},
				PR:      PRTrack{State: PRStateOpen, Reason: PRReasonInProgress},
			},
			want: "needs_input",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveStage(tc.l); got != tc.want {
				t.Errorf("DeriveStage = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSynthesizeLifecycle(t *testing.T) {
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		task        Task
		wantSession SessionState
		wantPR      PRState
	}{
		{
			name:        "empty task → not_started",
			task:        Task{},
			wantSession: SessionStateNotStarted,
			wantPR:      PRStateNone,
		},
		{
			name:        "in mid stage → working",
			task:        Task{Stage: "implement", StageStarted: now},
			wantSession: SessionStateWorking,
			wantPR:      PRStateNone,
		},
		{
			name: "task with open PR → idle / pr_created",
			task: Task{
				Stage:         "wait_for_review",
				MergeRequests: []MergeRef{{Repo: "x", State: "open", URL: "u"}},
			},
			wantSession: SessionStateIdle,
			wantPR:      PRStateOpen,
		},
		{
			name: "task with merged PR → idle / merged_waiting_decision",
			task: Task{
				Stage:         "done",
				MergeRequests: []MergeRef{{Repo: "x", State: "merged", URL: "u"}},
			},
			wantSession: SessionStateIdle,
			wantPR:      PRStateMerged,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := SynthesizeLifecycle(&tc.task, now)
			if l.Session.State != tc.wantSession {
				t.Errorf("session state = %q, want %q", l.Session.State, tc.wantSession)
			}
			if l.PR.State != tc.wantPR {
				t.Errorf("pr state = %q, want %q", l.PR.State, tc.wantPR)
			}
			if l.Version != 2 {
				t.Errorf("version = %d, want 2", l.Version)
			}
		})
	}
}

func TestEnsureLifecycle(t *testing.T) {
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)

	t.Run("blank task gets synthesised", func(t *testing.T) {
		task := &Task{Stage: "implement"}
		got := EnsureLifecycle(task, now)
		if got.Version != 2 {
			t.Fatalf("version = %d, want 2", got.Version)
		}
		if task.Lifecycle.Version != 2 {
			t.Errorf("not written back onto task")
		}
		if got.Session.State != SessionStateWorking {
			t.Errorf("expected working, got %q", got.Session.State)
		}
	})

	t.Run("populated lifecycle is left alone", func(t *testing.T) {
		task := &Task{
			Lifecycle: Lifecycle{
				Version: 2,
				Session: SessionTrack{State: SessionStateStuck},
			},
		}
		got := EnsureLifecycle(task, now)
		if got.Session.State != SessionStateStuck {
			t.Errorf("stomped existing lifecycle")
		}
	})
}
