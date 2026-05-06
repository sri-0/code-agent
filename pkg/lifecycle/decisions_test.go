package lifecycle

import (
	"testing"
	"time"

	"code-agent/pkg/tasks"
	"code-agent/pkg/vcs"
)

func TestApplyOpenPRDecision(t *testing.T) {
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	cur := vcs.MergeRequest{State: "open", Number: 42, URL: "https://x/pr/42"}

	tests := []struct {
		name        string
		ci          vcs.CISummary
		rv          vcs.ReviewDecision
		wantPR      tasks.PRReason
		wantSession tasks.SessionState
	}{
		{
			name:        "ci failing dominates everything",
			ci:          vcs.CISummary{Status: vcs.CIStatusFailing},
			rv:          vcs.ReviewDecisionApproved,
			wantPR:      tasks.PRReasonCIFailing,
			wantSession: tasks.SessionStateWorking,
		},
		{
			name:        "changes_requested before approved",
			ci:          vcs.CISummary{Status: vcs.CIStatusPassing},
			rv:          vcs.ReviewDecisionChangesRequested,
			wantPR:      tasks.PRReasonChangesRequested,
			wantSession: tasks.SessionStateWorking,
		},
		{
			name:        "approved + green = merge ready",
			ci:          vcs.CISummary{Status: vcs.CIStatusPassing},
			rv:          vcs.ReviewDecisionApproved,
			wantPR:      tasks.PRReasonMergeReady,
			wantSession: tasks.SessionStateIdle,
		},
		{
			name:        "approved + ci pending = approved (not yet mergeable)",
			ci:          vcs.CISummary{Status: vcs.CIStatusPending},
			rv:          vcs.ReviewDecisionApproved,
			wantPR:      tasks.PRReasonApproved,
			wantSession: tasks.SessionStateIdle,
		},
		{
			name:        "review pending",
			ci:          vcs.CISummary{Status: vcs.CIStatusPassing},
			rv:          vcs.ReviewDecisionPending,
			wantPR:      tasks.PRReasonReviewPending,
			wantSession: tasks.SessionStateIdle,
		},
		{
			name:        "ci pending, no review",
			ci:          vcs.CISummary{Status: vcs.CIStatusPending},
			rv:          vcs.ReviewDecisionNone,
			wantPR:      tasks.PRReasonCIPending,
			wantSession: tasks.SessionStateIdle,
		},
		{
			name:   "no signals → in_progress",
			ci:     vcs.CISummary{Status: vcs.CIStatusUnknown},
			rv:     vcs.ReviewDecisionNone,
			wantPR: tasks.PRReasonInProgress,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := &tasks.Lifecycle{Version: 2}
			applyOpenPRDecision(l, cur, tc.ci, tc.rv, now)
			if l.PR.Reason != tc.wantPR {
				t.Errorf("PR.Reason = %q, want %q", l.PR.Reason, tc.wantPR)
			}
			if tc.wantSession != "" && l.Session.State != tc.wantSession {
				t.Errorf("Session.State = %q, want %q", l.Session.State, tc.wantSession)
			}
			if l.PR.Number != 42 {
				t.Errorf("PR.Number not propagated")
			}
			if l.PR.URL == "" {
				t.Errorf("PR.URL not propagated")
			}
		})
	}
}

func TestApplyTerminalPRDecision(t *testing.T) {
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	merged := vcs.MergeRequest{State: "merged", Number: 7, URL: "u"}
	closed := vcs.MergeRequest{State: "closed", Number: 8, URL: "u"}

	t.Run("merged → idle/merged_waiting_decision", func(t *testing.T) {
		l := &tasks.Lifecycle{Version: 2}
		applyTerminalPRDecision(l, merged, now)
		if l.PR.State != tasks.PRStateMerged {
			t.Errorf("pr state %q", l.PR.State)
		}
		if l.Session.Reason != tasks.SessionReasonMergedWaitingDecision {
			t.Errorf("session reason %q", l.Session.Reason)
		}
	})

	t.Run("closed → idle/pr_closed_waiting_decision", func(t *testing.T) {
		l := &tasks.Lifecycle{Version: 2}
		applyTerminalPRDecision(l, closed, now)
		if l.PR.State != tasks.PRStateClosed {
			t.Errorf("pr state %q", l.PR.State)
		}
		if l.Session.Reason != tasks.SessionReasonPRClosedWaitingDecision {
			t.Errorf("session reason %q", l.Session.Reason)
		}
	})
}
