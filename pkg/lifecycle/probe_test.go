package lifecycle

import (
	"testing"
	"time"

	"code-agent/pkg/tasks"
)

func TestResolveProbe_BothAlive(t *testing.T) {
	now := time.Now()
	d := ResolveProbe(ProbeResult{Pod: ProbeAlive, OpenCode: ProbeAlive, Evidence: "ok"}, tasks.DetectingState{}, now)
	if d.NextRuntime != tasks.RuntimeStateAlive {
		t.Errorf("runtime = %q, want alive", d.NextRuntime)
	}
	if d.Stuck {
		t.Errorf("should not be stuck")
	}
	if d.NextDetect.Attempts != 0 {
		t.Errorf("detect should be cleared, got %+v", d.NextDetect)
	}
}

func TestResolveProbe_PodUnknownOpencodeAlive(t *testing.T) {
	// Local runtime case: no pod prober, but opencode is reachable. Should
	// still resolve to alive (pod unobservable ≠ unhealthy).
	now := time.Now()
	d := ResolveProbe(ProbeResult{Pod: ProbeUnknown, OpenCode: ProbeAlive}, tasks.DetectingState{}, now)
	if d.NextRuntime != tasks.RuntimeStateAlive {
		t.Errorf("expected alive, got %q", d.NextRuntime)
	}
}

func TestResolveProbe_BothDead(t *testing.T) {
	now := time.Now()
	d := ResolveProbe(ProbeResult{Pod: ProbeDead, OpenCode: ProbeDead, Evidence: "gone"}, tasks.DetectingState{}, now)
	if d.NextRuntime != tasks.RuntimeStateMissing {
		t.Errorf("runtime = %q, want missing", d.NextRuntime)
	}
	if d.NextSession != tasks.SessionStateTerminated {
		t.Errorf("session = %q, want terminated", d.NextSession)
	}
	if d.Stuck {
		t.Errorf("dead != stuck — should be terminated")
	}
}

func TestResolveProbe_Disagreement_FirstAttempt(t *testing.T) {
	now := time.Now()
	d := ResolveProbe(
		ProbeResult{Pod: ProbeAlive, OpenCode: ProbeDead, Evidence: "opencode 5xx"},
		tasks.DetectingState{},
		now,
	)
	if d.NextSession != tasks.SessionStateDetecting {
		t.Errorf("first disagreement should enter detecting, got %q", d.NextSession)
	}
	if d.NextDetect.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", d.NextDetect.Attempts)
	}
	if d.NextDetect.StartedAt == nil {
		t.Errorf("StartedAt should be set on first detect")
	}
}

func TestResolveProbe_Disagreement_AttemptsCarryWithStableEvidence(t *testing.T) {
	start := time.Now()
	current := tasks.DetectingState{
		Attempts: 1, StartedAt: &start,
		EvidenceHash: EvidenceHashStable("opencode 5xx"),
	}
	d := ResolveProbe(
		ProbeResult{Pod: ProbeAlive, OpenCode: ProbeDead, Evidence: "opencode 5xx"},
		current,
		start.Add(30*time.Second),
	)
	if d.NextDetect.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 (carried)", d.NextDetect.Attempts)
	}
	if d.Stuck {
		t.Errorf("should not be stuck on attempt 2")
	}
}

func TestResolveProbe_ResetsAttemptsOnNewEvidence(t *testing.T) {
	start := time.Now()
	current := tasks.DetectingState{
		Attempts: 2, StartedAt: &start,
		EvidenceHash: EvidenceHashStable("opencode 5xx"),
	}
	d := ResolveProbe(
		ProbeResult{Pod: ProbeAlive, OpenCode: ProbeDead, Evidence: "opencode timeout"},
		current,
		start.Add(30*time.Second),
	)
	if d.NextDetect.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 (reset on new evidence)", d.NextDetect.Attempts)
	}
}

func TestResolveProbe_EscalatesAfterMaxAttempts(t *testing.T) {
	start := time.Now()
	current := tasks.DetectingState{
		Attempts: 3, StartedAt: &start,
		EvidenceHash: EvidenceHashStable("opencode 5xx"),
	}
	d := ResolveProbe(
		ProbeResult{Pod: ProbeAlive, OpenCode: ProbeDead, Evidence: "opencode 5xx"},
		current,
		start.Add(30*time.Second),
	)
	if !d.Stuck {
		t.Errorf("should be stuck after exceeding max attempts")
	}
	if d.NextSession != tasks.SessionStateStuck {
		t.Errorf("session = %q, want stuck", d.NextSession)
	}
}

func TestResolveProbe_EscalatesAfterMaxDuration(t *testing.T) {
	start := time.Now()
	current := tasks.DetectingState{
		Attempts: 2, StartedAt: &start,
		EvidenceHash: EvidenceHashStable("opencode 5xx"),
	}
	// Past the duration budget.
	d := ResolveProbe(
		ProbeResult{Pod: ProbeAlive, OpenCode: ProbeDead, Evidence: "opencode 5xx"},
		current,
		start.Add(DetectingMaxDuration+time.Second),
	)
	if !d.Stuck {
		t.Errorf("should be stuck once duration budget elapsed")
	}
}
