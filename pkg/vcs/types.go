// Package vcs abstracts MR/PR operations across GitLab and GitHub.
//
// Each concrete provider lives in a subpackage (gitlab/, github/) and
// implements Client. The orchestrator uses Factory to build a Client per
// BoardRepo, keyed by {provider, host, token}.
//
// We deliberately only expose the few operations open_mr needs (create a
// merge/pull request, look it up, comment on it). Everything else goes
// through the agent.
package vcs

import (
	"context"
	"fmt"
	"sync"
	"time"

	"code-agent/internal/config"
)

// MergeRequest is the provider-neutral view of a merge/pull request.
type MergeRequest struct {
	Repo         string // boards.yaml repo name
	Number       int    // GitHub PR number (or 0)
	IID          int    // GitLab MR IID (or 0)
	URL          string
	State        string     // open | merged | closed
	Title        string
	SourceBranch string     // feature branch (e.g. ai/<ticket>)
	TargetBranch string     // base branch
	ClosedAt     *time.Time // set when state=closed/merged
	MergedAt     *time.Time // set when merged
}

// OpenMRRequest is the input to Client.OpenMR.
type OpenMRRequest struct {
	RepoName     string
	SourceBranch string
	TargetBranch string
	Title        string
	Description  string
	// DraftIfEmpty marks the MR as draft when the branch has no commits
	// ahead of target. Left false here; runners decide.
	Draft bool
}

// ReviewComment is one comment on a merge/pull request. We treat both
// GitHub issue comments and GitHub review comments as the same shape for
// review-feedback purposes. ParentID, when non-empty, means this comment
// was posted as a reply to another comment (used to avoid self-loops).
type ReviewComment struct {
	ID       string // provider-specific stringified id
	ParentID string // optional; empty if top-level
	Author   string // login / username
	Body     string
	CreatedAt time.Time
	URL      string
}

// CIStatus is the rolled-up state of all CI checks on the MR's head sha.
// Mirrors agent-orchestrator's `getCISummary().status`.
type CIStatus string

const (
	CIStatusUnknown CIStatus = "unknown"
	CIStatusPending CIStatus = "pending"
	CIStatusPassing CIStatus = "passing"
	CIStatusFailing CIStatus = "failing"
)

// CIRun is one named check. Captures just enough to surface to the agent
// (name + URL + conclusion) without bloating the type.
type CIRun struct {
	Name       string
	URL        string
	Conclusion string // success | failure | neutral | pending | cancelled | skipped
}

// CISummary is the rolled-up CI state plus the failing runs (so we can
// pass them straight to the agent for "fix CI" turns without re-querying).
type CISummary struct {
	Status      CIStatus
	HeadSHA     string
	FailingRuns []CIRun
}

// ReviewDecision is the rolled-up reviewer disposition. Mirrors GitHub's
// `pullRequest.reviewDecision` GraphQL enum and is synthesised on GitLab
// from the approvals API.
type ReviewDecision string

const (
	ReviewDecisionNone             ReviewDecision = "none"               // no reviewers / not required
	ReviewDecisionPending          ReviewDecision = "pending"            // requested but undecided
	ReviewDecisionApproved         ReviewDecision = "approved"
	ReviewDecisionChangesRequested ReviewDecision = "changes_requested"
)

// MergeOptions controls how Client.MergeMR closes a PR. Method is
// "merge" | "squash" | "rebase" — provider-specific defaults if empty.
type MergeOptions struct {
	Method        string
	CommitTitle   string
	CommitMessage string
}

// Client is the MR/PR API surface we need.
type Client interface {
	Provider() string // "gitlab" | "github"
	OpenMR(ctx context.Context, in OpenMRRequest) (MergeRequest, error)
	GetMR(ctx context.Context, ref MergeRequest) (MergeRequest, error)
	Comment(ctx context.Context, ref MergeRequest, body string) error
	// ListComments returns all review comments on the MR whose CreatedAt
	// is strictly after `since`. Ordered oldest first. Used by the review
	// feedback poller.
	ListComments(ctx context.Context, ref MergeRequest, since time.Time) ([]ReviewComment, error)
	// ReplyToComment posts `body` as a reply to the given comment on the
	// MR. If the provider doesn't support threaded replies, falls back
	// to a top-level comment that quotes the parent's first line.
	ReplyToComment(ctx context.Context, ref MergeRequest, parent ReviewComment, body string) error
	// BotIdentity returns the username/login of the authenticated actor
	// (so pollers can skip the bot's own comments). May be empty if the
	// provider can't resolve it; callers treat empty as "don't dedupe".
	BotIdentity(ctx context.Context) (string, error)
	// GetCIStatus rolls up all checks on the MR's head sha. Pending if
	// any are pending; failing if any are failing; passing if all pass.
	// Returns CIStatusUnknown if the provider doesn't expose check data.
	GetCIStatus(ctx context.Context, ref MergeRequest) (CISummary, error)
	// GetReviewDecision returns the rolled-up reviewer state.
	GetReviewDecision(ctx context.Context, ref MergeRequest) (ReviewDecision, error)
	// MergeMR closes an open MR/PR by merging it. Used by auto-merge.
	MergeMR(ctx context.Context, ref MergeRequest, opts MergeOptions) error
}

// Factory builds a Client from a BoardRepo's VCS config.
type Factory func(repo config.BoardRepo) (Client, error)

var (
	factMu    sync.RWMutex
	factories = map[string]Factory{}
)

// Register a provider factory under its name.
func Register(provider string, f Factory) {
	factMu.Lock()
	defer factMu.Unlock()
	factories[provider] = f
}

// Build returns a Client for the repo's VCS config.
func Build(repo config.BoardRepo) (Client, error) {
	factMu.RLock()
	f, ok := factories[repo.VCS.Provider]
	factMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown vcs provider %q", repo.VCS.Provider)
	}
	return f(repo)
}
