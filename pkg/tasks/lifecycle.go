package tasks

import "time"

// Lifecycle is a three-track snapshot of where a Task is right now. Each
// track has independent (state, reason) so we can represent things the
// flat Stage enum can't — e.g. session=idle while pr=changes_requested
// and runtime=alive. The dashboard's flat status is *derived* from this
// via DeriveStage; we never write the dashboard column directly.
//
// Modelled after agent-orchestrator's `lifecycle-state.ts` v2 schema.
type Lifecycle struct {
	Version int            `json:"version"`
	Session SessionTrack   `json:"session"`
	PR      PRTrack        `json:"pr"`
	Runtime RuntimeTrack   `json:"runtime"`
	Detect  DetectingState `json:"detect,omitempty"`
}

// SessionTrack — what the agent itself is doing.
type SessionTrack struct {
	State            SessionState  `json:"state"`
	Reason           SessionReason `json:"reason"`
	StartedAt        *time.Time    `json:"started_at,omitempty"`
	CompletedAt      *time.Time    `json:"completed_at,omitempty"`
	TerminatedAt    *time.Time     `json:"terminated_at,omitempty"`
	LastTransitionAt *time.Time    `json:"last_transition_at,omitempty"`
}

// PRTrack — the dominant PR's state. We collapse multi-repo here by
// taking the "least progressed" track (e.g. one open + one merged → open).
// For per-MR detail, walk Task.MergeRequests directly.
type PRTrack struct {
	State          PRState    `json:"state"`
	Reason         PRReason   `json:"reason"`
	Number         int        `json:"number,omitempty"`
	URL            string     `json:"url,omitempty"`
	LastObservedAt *time.Time `json:"last_observed_at,omitempty"`
}

// RuntimeTrack — pod / opencode liveness.
type RuntimeTrack struct {
	State          RuntimeState  `json:"state"`
	Reason         RuntimeReason `json:"reason"`
	LastObservedAt *time.Time    `json:"last_observed_at,omitempty"`
}

// DetectingState carries the probe-attempt budget when the runtime is in
// `detecting` (signals disagree). Cleared when we leave detecting.
//
// Mirrors `lifecycle-status-decisions.ts` — both an attempt counter
// (max 3) and a wall-clock budget (5 min) so a slow probe can't trap us
// in detecting forever. EvidenceHash prevents the attempt counter from
// resetting when the same flaky symptom re-presents across polls.
type DetectingState struct {
	Attempts      int        `json:"attempts,omitempty"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	EvidenceHash  string     `json:"evidence_hash,omitempty"`
}

// ---- enums ----------------------------------------------------------------

type SessionState string

const (
	SessionStateNotStarted SessionState = "not_started"
	SessionStateWorking    SessionState = "working"
	SessionStateIdle       SessionState = "idle"
	SessionStateNeedsInput SessionState = "needs_input"
	SessionStateStuck      SessionState = "stuck"
	SessionStateDetecting  SessionState = "detecting"
	SessionStateDone       SessionState = "done"
	SessionStateTerminated SessionState = "terminated"
)

type SessionReason string

const (
	SessionReasonSpawnRequested           SessionReason = "spawn_requested"
	SessionReasonAgentAcknowledged        SessionReason = "agent_acknowledged"
	SessionReasonTaskInProgress           SessionReason = "task_in_progress"
	SessionReasonPRCreated                SessionReason = "pr_created"
	SessionReasonFixingCI                 SessionReason = "fixing_ci"
	SessionReasonResolvingReviewComments  SessionReason = "resolving_review_comments"
	SessionReasonAwaitingUserInput        SessionReason = "awaiting_user_input"
	SessionReasonAwaitingExternalReview   SessionReason = "awaiting_external_review"
	SessionReasonResearchComplete         SessionReason = "research_complete"
	SessionReasonMergedWaitingDecision    SessionReason = "merged_waiting_decision"
	SessionReasonPRClosedWaitingDecision  SessionReason = "pr_closed_waiting_decision"
	SessionReasonManuallyKilled           SessionReason = "manually_killed"
	SessionReasonRuntimeLost              SessionReason = "runtime_lost"
	SessionReasonAgentProcessExited       SessionReason = "agent_process_exited"
	SessionReasonProbeFailure             SessionReason = "probe_failure"
	SessionReasonErrorInProcess           SessionReason = "error_in_process"
	SessionReasonAutoCleanup              SessionReason = "auto_cleanup"
	SessionReasonPRMerged                 SessionReason = "pr_merged"
)

type PRState string

const (
	PRStateNone   PRState = "none"
	PRStateOpen   PRState = "open"
	PRStateMerged PRState = "merged"
	PRStateClosed PRState = "closed"
)

type PRReason string

const (
	PRReasonNotCreated       PRReason = "not_created"
	PRReasonInProgress       PRReason = "in_progress"
	PRReasonCIFailing        PRReason = "ci_failing"
	PRReasonCIPending        PRReason = "ci_pending"
	PRReasonReviewPending    PRReason = "review_pending"
	PRReasonChangesRequested PRReason = "changes_requested"
	PRReasonApproved         PRReason = "approved"
	PRReasonMergeReady       PRReason = "merge_ready"
	PRReasonMerged           PRReason = "merged"
	PRReasonClosedUnmerged   PRReason = "closed_unmerged"
)

type RuntimeState string

const (
	RuntimeStateUnknown     RuntimeState = "unknown"
	RuntimeStateAlive       RuntimeState = "alive"
	RuntimeStateExited      RuntimeState = "exited"
	RuntimeStateMissing     RuntimeState = "missing"
	RuntimeStateProbeFailed RuntimeState = "probe_failed"
)

type RuntimeReason string

const (
	RuntimeReasonSpawnIncomplete    RuntimeReason = "spawn_incomplete"
	RuntimeReasonProcessRunning     RuntimeReason = "process_running"
	RuntimeReasonProcessMissing     RuntimeReason = "process_missing"
	RuntimeReasonManualKillRequested RuntimeReason = "manual_kill_requested"
	RuntimeReasonProbeError         RuntimeReason = "probe_error"
	RuntimeReasonPRMergedCleanup    RuntimeReason = "pr_merged_cleanup"
	RuntimeReasonAutoCleanup        RuntimeReason = "auto_cleanup"
)

// ---- derivation -----------------------------------------------------------

// DeriveStage projects a Lifecycle to a flat dashboard column. Mirrors
// `deriveLegacyStatus()` in agent-orchestrator — terminal session states
// dominate, then PR state, then session state.
func DeriveStage(l Lifecycle) string {
	switch l.Session.State {
	case SessionStateNotStarted:
		return "spawning"
	case SessionStateNeedsInput:
		return "needs_input"
	case SessionStateStuck:
		return "stuck"
	case SessionStateDetecting:
		return "detecting"
	case SessionStateDone:
		return "done"
	case SessionStateTerminated:
		switch l.Session.Reason {
		case SessionReasonManuallyKilled, SessionReasonRuntimeLost:
			return "killed"
		case SessionReasonAutoCleanup, SessionReasonPRMerged:
			return "cleanup"
		case SessionReasonErrorInProcess, SessionReasonProbeFailure:
			return "errored"
		default:
			return "terminated"
		}
	}

	if l.PR.State == PRStateMerged {
		return "merged"
	}
	if l.PR.State == PRStateOpen {
		switch l.PR.Reason {
		case PRReasonCIFailing:
			return "ci_failed"
		case PRReasonChangesRequested:
			return "changes_requested"
		case PRReasonReviewPending:
			return "review_pending"
		case PRReasonApproved:
			return "approved"
		case PRReasonMergeReady:
			return "mergeable"
		default:
			return "pr_open"
		}
	}

	switch l.Session.State {
	case SessionStateIdle:
		return "idle"
	case SessionStateWorking:
		return "working"
	}
	return "working"
}

// SynthesizeLifecycle builds an initial Lifecycle from a task's flat
// fields when no canonical record exists yet. Used at first read of a
// pre-Lifecycle task and as the starting point for new tasks.
func SynthesizeLifecycle(t *Task, now time.Time) Lifecycle {
	l := Lifecycle{
		Version: 2,
		Session: SessionTrack{
			State:            SessionStateNotStarted,
			Reason:           SessionReasonSpawnRequested,
			LastTransitionAt: &now,
		},
		PR: PRTrack{
			State:  PRStateNone,
			Reason: PRReasonNotCreated,
		},
		Runtime: RuntimeTrack{
			State:  RuntimeStateUnknown,
			Reason: RuntimeReasonSpawnIncomplete,
		},
	}

	// Promote based on Stage — best-effort backfill so an upgraded
	// orchestrator doesn't see every existing task as "not_started".
	if t.Stage != "" && t.Stage != "intake" {
		l.Session.State = SessionStateWorking
		l.Session.Reason = SessionReasonTaskInProgress
		started := t.StageStarted
		if !started.IsZero() {
			l.Session.StartedAt = &started
		}
	}
	if t.WorkerRef.URL != "" {
		l.Runtime.State = RuntimeStateAlive
		l.Runtime.Reason = RuntimeReasonProcessRunning
		l.Runtime.LastObservedAt = &now
	}

	// Pick the dominant MR (first open, otherwise first merged, otherwise first).
	dom := dominantMR(t.MergeRequests)
	if dom != nil {
		l.PR.Number = dom.Number
		if dom.IID > 0 {
			l.PR.Number = dom.IID
		}
		l.PR.URL = dom.URL
		l.PR.LastObservedAt = &now
		switch dom.State {
		case "merged":
			l.PR.State = PRStateMerged
			l.PR.Reason = PRReasonMerged
			l.Session.State = SessionStateIdle
			l.Session.Reason = SessionReasonMergedWaitingDecision
		case "closed":
			l.PR.State = PRStateClosed
			l.PR.Reason = PRReasonClosedUnmerged
			l.Session.State = SessionStateIdle
			l.Session.Reason = SessionReasonPRClosedWaitingDecision
		default:
			l.PR.State = PRStateOpen
			l.PR.Reason = PRReasonInProgress
			l.Session.State = SessionStateIdle
			l.Session.Reason = SessionReasonPRCreated
		}
	}

	return l
}

func dominantMR(mrs []MergeRef) *MergeRef {
	if len(mrs) == 0 {
		return nil
	}
	for i := range mrs {
		if mrs[i].State == "open" || mrs[i].State == "" {
			return &mrs[i]
		}
	}
	for i := range mrs {
		if mrs[i].State == "merged" {
			return &mrs[i]
		}
	}
	return &mrs[0]
}

// EnsureLifecycle returns t.Lifecycle if populated, otherwise synthesises
// one from t's flat fields and writes it back. Callers that mutate must
// re-persist the task.
func EnsureLifecycle(t *Task, now time.Time) *Lifecycle {
	if t.Lifecycle.Version == 0 {
		t.Lifecycle = SynthesizeLifecycle(t, now)
	}
	return &t.Lifecycle
}
