package reactions

import (
	"crypto/sha256"
	"encoding/hex"

	"code-agent/pkg/tasks"
)

// DeriveEvents diffs two lifecycle snapshots and yields the reaction
// events that should fire. Empty slice if nothing reaction-worthy
// changed. Mirrors the transition-detection logic in
// agent-orchestrator's lifecycle-manager.ts (which inlines this in a
// big switch).
func DeriveEvents(prev, next tasks.Lifecycle) []EventKey {
	var out []EventKey

	// CI failing → ci-failed (only when entering the state, not on
	// every poll while still failing).
	if prev.PR.Reason != tasks.PRReasonCIFailing && next.PR.Reason == tasks.PRReasonCIFailing {
		out = append(out, EventCIFailed)
	}
	// Changes requested → changes-requested.
	if prev.PR.Reason != tasks.PRReasonChangesRequested && next.PR.Reason == tasks.PRReasonChangesRequested {
		out = append(out, EventChangesRequested)
	}
	// Approved + green → approved-and-green (mergeable). Fires once on
	// entry; auto-merge action records on ledger so we don't loop.
	if prev.PR.Reason != tasks.PRReasonMergeReady && next.PR.Reason == tasks.PRReasonMergeReady {
		out = append(out, EventApprovedAndGreen)
	}
	// Agent stuck → agent-stuck.
	if prev.Session.State != tasks.SessionStateStuck && next.Session.State == tasks.SessionStateStuck {
		out = append(out, EventAgentStuck)
	}
	// PR merged terminal → pr-merged (used for cleanup/notify).
	if prev.PR.State != tasks.PRStateMerged && next.PR.State == tasks.PRStateMerged {
		out = append(out, EventPRMerged)
	}
	return out
}

// EvidenceHash returns a short stable hash of the evidence behind an
// event — the ledger uses this so the same failing-CI run doesn't
// re-trigger ci-failed every time the lifecycle poller ticks while the
// state is still failing. Caller is the lifecycle poller, which knows
// what specific signal made it pick this key.
func EvidenceHash(key EventKey, evidence string) string {
	h := sha256.Sum256([]byte(string(key) + "|" + evidence))
	return hex.EncodeToString(h[:])[:12]
}
