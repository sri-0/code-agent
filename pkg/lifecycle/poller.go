// Package lifecycle owns the canonical multi-track state machine — probes,
// PR-state enrichment, and the rules that decide when a task moves from
// `working` to `idle`/`stuck`/`done`. Tracks live on tasks.Lifecycle; this
// package is the *writer* for the PR + Runtime tracks. Session-track
// transitions are still owned by the stage engine.
package lifecycle

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"code-agent/internal/config"
	"code-agent/pkg/tasks"
	"code-agent/pkg/vcs"
)

// PRPoller wakes periodically and refreshes Lifecycle.PR for every task
// with at least one open merge request. Each tick:
//  1. List tasks per board.
//  2. For each task with an open MR, build a vcs.Client.
//  3. Refresh state (open/merged/closed) + ClosedAt/MergedAt via GetMR.
//  4. Pull CI summary + review decision; project to PRTrack.
//  5. Persist if anything changed.
//
// Designed to coexist with pkg/review.Poller — that one looks at
// individual comments; this one tracks the rolled-up MR state. They both
// run on the same default interval (CODE_AGENT_POD_GC_INTERVAL).
type PRPoller struct {
	tasks    tasks.Store
	boards   *config.BoardsConfig
	interval time.Duration
	logger   zerolog.Logger

	// onTransition is called after a task's lifecycle is mutated in
	// memory and persisted. The reactions engine subscribes here.
	onTransition func(ctx context.Context, t *tasks.Task, prev, next tasks.Lifecycle)
}

// NewPRPoller — onTransition may be nil; if so, no reactions fire.
func NewPRPoller(
	store tasks.Store,
	boards *config.BoardsConfig,
	interval time.Duration,
	logger zerolog.Logger,
	onTransition func(ctx context.Context, t *tasks.Task, prev, next tasks.Lifecycle),
) *PRPoller {
	if interval <= 0 {
		interval = 2 * time.Minute
	}
	return &PRPoller{
		tasks:        store,
		boards:       boards,
		interval:     interval,
		logger:       logger.With().Str("component", "lifecycle_poller").Logger(),
		onTransition: onTransition,
	}
}

func (p *PRPoller) Start(ctx context.Context) {
	p.logger.Info().Dur("interval", p.interval).Msg("lifecycle PR poller started")
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.tick(ctx)
		}
	}
}

func (p *PRPoller) tick(ctx context.Context) {
	if p.tasks == nil || p.boards == nil {
		return
	}
	for _, b := range p.boards.Boards {
		ts, err := p.tasks.ListByBoard(ctx, b.ID)
		if err != nil {
			p.logger.Warn().Err(err).Str("board", b.ID).Msg("list tasks failed")
			continue
		}
		for _, t := range ts {
			if !hasInterestingMR(t) {
				continue
			}
			p.processTask(ctx, b, t)
		}
	}
}

// hasInterestingMR returns true if the task has any MR worth polling
// (open, or recently transitioned). Closed/merged MRs older than 30
// minutes are skipped to keep the tick cheap.
func hasInterestingMR(t *tasks.Task) bool {
	for _, mr := range t.MergeRequests {
		if mr.State == "" || mr.State == "open" {
			return true
		}
	}
	return false
}

func (p *PRPoller) processTask(ctx context.Context, board config.Board, t *tasks.Task) {
	now := time.Now()
	prev := t.Lifecycle
	tasks.EnsureLifecycle(t, now)

	dom := dominantOpenMR(t.MergeRequests)
	if dom == nil {
		return
	}
	br, ok := findBoardRepo(board, dom.Repo)
	if !ok {
		return
	}
	client, err := vcs.Build(br)
	if err != nil {
		p.logger.Debug().Err(err).Str("repo", dom.Repo).Msg("vcs.Build failed")
		return
	}
	ref := vcs.MergeRequest{Repo: dom.Repo, IID: dom.IID, Number: dom.Number, URL: dom.URL}

	getCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	cur, err := client.GetMR(getCtx, ref)
	cancel()
	if err != nil {
		p.logger.Debug().Err(err).Str("repo", dom.Repo).Msg("get mr failed")
		return
	}

	// Refresh canonical fields on the dominant MergeRef.
	for i := range t.MergeRequests {
		if t.MergeRequests[i].Repo == dom.Repo &&
			t.MergeRequests[i].URL == dom.URL {
			t.MergeRequests[i].State = cur.State
			t.MergeRequests[i].ClosedAt = cur.ClosedAt
			t.MergeRequests[i].MergedAt = cur.MergedAt
			break
		}
	}

	// PR closed or merged → terminal. Update lifecycle without further
	// CI/review queries.
	if cur.State == "merged" || cur.State == "closed" {
		applyTerminalPRDecision(&t.Lifecycle, cur, now)
		p.persistAndNotify(ctx, t, prev)
		return
	}

	// Open. Enrich with CI + review decision.
	ciCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	ci, ciErr := client.GetCIStatus(ciCtx, ref)
	cancel()
	if ciErr != nil {
		p.logger.Debug().Err(ciErr).Str("repo", dom.Repo).Msg("ci status failed")
	}
	rvCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	rv, rvErr := client.GetReviewDecision(rvCtx, ref)
	cancel()
	if rvErr != nil {
		p.logger.Debug().Err(rvErr).Str("repo", dom.Repo).Msg("review decision failed")
	}

	applyOpenPRDecision(&t.Lifecycle, cur, ci, rv, now)
	p.persistAndNotify(ctx, t, prev)
}

func (p *PRPoller) persistAndNotify(ctx context.Context, t *tasks.Task, prev tasks.Lifecycle) {
	if !lifecycleChanged(prev, t.Lifecycle) {
		// Still write — ClosedAt/MergedAt fields may have moved on the
		// MergeRef even when track state didn't.
		_ = p.tasks.Update(ctx, t)
		return
	}
	if err := p.tasks.Update(ctx, t); err != nil {
		p.logger.Warn().Err(err).Str("task", t.ID).Msg("persist lifecycle failed")
		return
	}
	if p.onTransition != nil {
		p.onTransition(ctx, t, prev, t.Lifecycle)
	}
}

// applyTerminalPRDecision mirrors agent-orchestrator's
// resolveTerminalPRStateDecision — terminal PR state dominates the
// session track too (idle/merged_waiting_decision or pr_closed_waiting_decision).
func applyTerminalPRDecision(l *tasks.Lifecycle, cur vcs.MergeRequest, now time.Time) {
	l.PR.URL = cur.URL
	l.PR.LastObservedAt = &now
	if cur.Number > 0 {
		l.PR.Number = cur.Number
	}
	switch cur.State {
	case "merged":
		l.PR.State = tasks.PRStateMerged
		l.PR.Reason = tasks.PRReasonMerged
		l.Session.State = tasks.SessionStateIdle
		l.Session.Reason = tasks.SessionReasonMergedWaitingDecision
	case "closed":
		l.PR.State = tasks.PRStateClosed
		l.PR.Reason = tasks.PRReasonClosedUnmerged
		l.Session.State = tasks.SessionStateIdle
		l.Session.Reason = tasks.SessionReasonPRClosedWaitingDecision
	}
	l.Session.LastTransitionAt = &now
}

// applyOpenPRDecision is the open-PR analogue of
// resolveOpenPRDecision in agent-orchestrator. Order of checks matters:
// CI failing > changes_requested > approved+mergeable > approved >
// pending > in_progress.
func applyOpenPRDecision(l *tasks.Lifecycle, cur vcs.MergeRequest, ci vcs.CISummary, rv vcs.ReviewDecision, now time.Time) {
	l.PR.State = tasks.PRStateOpen
	l.PR.URL = cur.URL
	l.PR.LastObservedAt = &now
	if cur.Number > 0 {
		l.PR.Number = cur.Number
	}
	l.Session.LastTransitionAt = &now

	switch {
	case ci.Status == vcs.CIStatusFailing:
		l.PR.Reason = tasks.PRReasonCIFailing
		l.Session.State = tasks.SessionStateWorking
		l.Session.Reason = tasks.SessionReasonFixingCI
	case rv == vcs.ReviewDecisionChangesRequested:
		l.PR.Reason = tasks.PRReasonChangesRequested
		l.Session.State = tasks.SessionStateWorking
		l.Session.Reason = tasks.SessionReasonResolvingReviewComments
	case rv == vcs.ReviewDecisionApproved && ci.Status == vcs.CIStatusPassing:
		l.PR.Reason = tasks.PRReasonMergeReady
		l.Session.State = tasks.SessionStateIdle
		l.Session.Reason = tasks.SessionReasonAwaitingExternalReview
	case rv == vcs.ReviewDecisionApproved:
		l.PR.Reason = tasks.PRReasonApproved
		l.Session.State = tasks.SessionStateIdle
		l.Session.Reason = tasks.SessionReasonAwaitingExternalReview
	case rv == vcs.ReviewDecisionPending:
		l.PR.Reason = tasks.PRReasonReviewPending
		l.Session.State = tasks.SessionStateIdle
		l.Session.Reason = tasks.SessionReasonAwaitingExternalReview
	case ci.Status == vcs.CIStatusPending:
		l.PR.Reason = tasks.PRReasonCIPending
		l.Session.State = tasks.SessionStateIdle
		l.Session.Reason = tasks.SessionReasonAwaitingExternalReview
	default:
		l.PR.Reason = tasks.PRReasonInProgress
		// Don't downgrade Session.State here — stage engine owns it.
	}
}

func lifecycleChanged(a, b tasks.Lifecycle) bool {
	return a.Session.State != b.Session.State ||
		a.Session.Reason != b.Session.Reason ||
		a.PR.State != b.PR.State ||
		a.PR.Reason != b.PR.Reason ||
		a.Runtime.State != b.Runtime.State ||
		a.Runtime.Reason != b.Runtime.Reason
}

func dominantOpenMR(mrs []tasks.MergeRef) *tasks.MergeRef {
	for i := range mrs {
		if mrs[i].State == "" || mrs[i].State == "open" {
			return &mrs[i]
		}
	}
	return nil
}

func findBoardRepo(b config.Board, name string) (config.BoardRepo, bool) {
	for _, r := range b.Repos {
		if r.Name == name {
			return r, true
		}
	}
	return config.BoardRepo{}, false
}
