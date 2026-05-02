package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"code-agent/pkg/tasks"
)

// ProbeState is the rolled-up runtime liveness signal. Mirrors
// agent-orchestrator's ProbeState ("alive" | "dead" | "unknown") but
// extended with a "probe_failed" terminal for cases where the probe
// itself errored (network, auth) — distinguished from "dead" because
// we shouldn't escalate to stuck purely on probe-error.
type ProbeState string

const (
	ProbeAlive       ProbeState = "alive"
	ProbeDead        ProbeState = "dead"
	ProbeUnknown     ProbeState = "unknown"
	ProbeFailedToRun ProbeState = "probe_failed"
)

// ProbeResult bundles the multi-signal probe output. Each sub-probe
// reports independently; the decision math considers the *combination*
// of signals (e.g. pod alive + opencode dead = signal disagreement →
// detecting). Evidence is a short human string describing why we got
// this state — surfaced into the ticket comment when escalating.
type ProbeResult struct {
	Pod      ProbeState
	OpenCode ProbeState
	Evidence string
}

// PodProber resolves the k8s pod phase. Implementation lives in the
// persistent runtime (which has the k8s client). Local + ephemeral
// runtimes return ProbeUnknown.
type PodProber interface {
	ProbePod(ctx context.Context, t *tasks.Task) (ProbeState, string)
}

// OpenCodeProber pings the worker's opencode HTTP. Returns alive on
// 2xx/3xx, dead on connection error or 5xx, probe_failed on timeout.
type OpenCodeProber interface {
	ProbeOpenCode(ctx context.Context, t *tasks.Task) (ProbeState, string)
}

// HTTPOpenCodeProber is the default OpenCodeProber — does a HEAD against
// the worker URL with a short timeout. Doesn't require the password
// (every opencode HTTP server responds with auth headers, not 5xx, on
// missing creds — a 401 is a healthy server).
type HTTPOpenCodeProber struct {
	Client  *http.Client
	Timeout time.Duration
}

func NewHTTPOpenCodeProber() *HTTPOpenCodeProber {
	return &HTTPOpenCodeProber{
		Client:  &http.Client{Timeout: 5 * time.Second},
		Timeout: 5 * time.Second,
	}
}

func (h *HTTPOpenCodeProber) ProbeOpenCode(ctx context.Context, t *tasks.Task) (ProbeState, string) {
	url := t.WorkerRef.URL
	if url == "" {
		return ProbeUnknown, "no worker url"
	}
	rctx, cancel := context.WithTimeout(ctx, h.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, url+"/", nil)
	if err != nil {
		return ProbeFailedToRun, "build request: " + err.Error()
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return ProbeDead, "http: " + err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return ProbeDead, fmt.Sprintf("opencode 5xx: %d", resp.StatusCode)
	}
	return ProbeAlive, fmt.Sprintf("opencode %d", resp.StatusCode)
}

// Probe runs all sub-probes and aggregates. PodProber may be nil for
// non-k8s runtimes — we just take it as Unknown and let the decision
// rules handle disagreement with opencode.
func Probe(ctx context.Context, t *tasks.Task, pod PodProber, oc OpenCodeProber) ProbeResult {
	out := ProbeResult{Pod: ProbeUnknown, OpenCode: ProbeUnknown}
	var ev []string
	if pod != nil {
		st, why := pod.ProbePod(ctx, t)
		out.Pod = st
		ev = append(ev, "pod="+string(st)+":"+why)
	}
	if oc != nil {
		st, why := oc.ProbeOpenCode(ctx, t)
		out.OpenCode = st
		ev = append(ev, "opencode="+string(st)+":"+why)
	}
	out.Evidence = strings.Join(ev, " ")
	return out
}

// ---- decision math --------------------------------------------------------

const (
	// DetectingMaxAttempts mirrors agent-orchestrator's
	// DETECTING_MAX_ATTEMPTS — escalate to stuck after this many
	// consecutive probe-disagreement ticks.
	DetectingMaxAttempts = 3
	// DetectingMaxDuration — wall-clock budget regardless of attempts.
	// Either trips → stuck.
	DetectingMaxDuration = 5 * time.Minute
)

// Decision is the output of ResolveProbe — what the runtime track and
// (optionally) the session track should be set to. Empty NextRuntime
// means "leave the runtime track alone".
type Decision struct {
	NextRuntime tasks.RuntimeState
	NextRuntimeReason tasks.RuntimeReason
	NextSession tasks.SessionState  // optional override (e.g. force stuck)
	NextSessionReason tasks.SessionReason
	NextDetect  tasks.DetectingState
	Evidence    string
	// Stuck is true when this decision flips the session to stuck.
	Stuck bool
}

// ResolveProbe is the pure-function decision: given a probe result and
// the task's current detecting budget, return the next state.
//
// Order:
//  1. Both alive → runtime alive, clear detecting.
//  2. Both dead → runtime missing, clear detecting (terminated, not stuck).
//  3. Disagreement (one alive, one dead) → detecting +1 attempt;
//     escalate to stuck if attempts > MAX or duration > BUDGET.
//  4. Probe-itself-failed → detecting; no penalty.
func ResolveProbe(result ProbeResult, current tasks.DetectingState, now time.Time) Decision {
	// Aggregate sentinel states: if either probe says probe_failed and
	// the other isn't dead, treat as detecting. probe_failed alone
	// shouldn't escalate to stuck — the orchestrator just doesn't know.
	bothAlive := result.Pod == ProbeAlive && result.OpenCode == ProbeAlive
	// "Either alive" needed when one prober is unknown (e.g. local runtime
	// has no PodProber). If opencode is alive and pod is unknown, that's
	// alive — pod state is just unobservable, not unhealthy.
	eitherAlive := result.Pod == ProbeAlive || result.OpenCode == ProbeAlive
	bothDead := result.Pod == ProbeDead && result.OpenCode == ProbeDead

	switch {
	case bothAlive || (eitherAlive && result.Pod != ProbeDead && result.OpenCode != ProbeDead):
		return Decision{
			NextRuntime:       tasks.RuntimeStateAlive,
			NextRuntimeReason: tasks.RuntimeReasonProcessRunning,
			NextDetect:        tasks.DetectingState{}, // clear
			Evidence:          result.Evidence,
		}
	case bothDead:
		return Decision{
			NextRuntime:       tasks.RuntimeStateMissing,
			NextRuntimeReason: tasks.RuntimeReasonProcessMissing,
			NextSession:       tasks.SessionStateTerminated,
			NextSessionReason: tasks.SessionReasonRuntimeLost,
			NextDetect:        tasks.DetectingState{},
			Evidence:          result.Evidence,
			// Not stuck — terminated. Stuck is for live-but-unresponsive.
		}
	}

	// Disagreement or partial uncertainty → detecting.
	hash := EvidenceHashStable(result.Evidence)
	startedAt := current.StartedAt
	attempts := current.Attempts
	if startedAt == nil || current.EvidenceHash != hash {
		// Fresh evidence (or first time entering detecting). Restart budget.
		t := now
		startedAt = &t
		attempts = 1
	} else {
		attempts++
	}

	overAttempts := attempts > DetectingMaxAttempts
	overDuration := startedAt != nil && now.Sub(*startedAt) > DetectingMaxDuration

	if overAttempts || overDuration {
		return Decision{
			NextRuntime:       tasks.RuntimeStateProbeFailed,
			NextRuntimeReason: tasks.RuntimeReasonProbeError,
			NextSession:       tasks.SessionStateStuck,
			NextSessionReason: tasks.SessionReasonProbeFailure,
			NextDetect: tasks.DetectingState{
				Attempts:     attempts,
				StartedAt:    startedAt,
				EvidenceHash: hash,
			},
			Evidence: result.Evidence,
			Stuck:    true,
		}
	}

	return Decision{
		NextRuntime:       tasks.RuntimeStateProbeFailed,
		NextRuntimeReason: tasks.RuntimeReasonProbeError,
		NextSession:       tasks.SessionStateDetecting,
		NextSessionReason: tasks.SessionReasonProbeFailure,
		NextDetect: tasks.DetectingState{
			Attempts:     attempts,
			StartedAt:    startedAt,
			EvidenceHash: hash,
		},
		Evidence: result.Evidence,
	}
}

// EvidenceHashStable hashes the evidence string with a normalisation
// pass that strips per-tick noise (timestamps, request ids) so the same
// underlying symptom presents with the same hash across polls.
func EvidenceHashStable(evidence string) string {
	// Currently a straight sha256 over the raw string. The agent-orchestrator
	// version strips `activity=...` and `at=...` tokens; our evidence
	// strings are short and don't carry those, so the straight hash is
	// fine for now. Promote to normalised form when the evidence
	// vocabulary grows.
	h := sha256.Sum256([]byte(evidence))
	return hex.EncodeToString(h[:])[:12]
}
