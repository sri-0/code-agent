// Package github implements vcs.Client against GitHub (github.com or
// self-hosted Enterprise). Auth via personal/bot token from env.
package github

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v67/github"

	"code-agent/internal/config"
	"code-agent/pkg/vcs"
)

type client struct {
	c     *gh.Client
	owner string
	repo  string
	host  string

	botOnce sync.Once
	botID   string
	botErr  error
}

func (c *client) Provider() string { return "github" }

func (c *client) OpenMR(ctx context.Context, in vcs.OpenMRRequest) (vcs.MergeRequest, error) {
	title := in.Title
	if in.Draft {
		title = "Draft: " + strings.TrimPrefix(title, "Draft: ")
	}
	newPR := &gh.NewPullRequest{
		Title: gh.String(title),
		Head:  gh.String(in.SourceBranch),
		Base:  gh.String(in.TargetBranch),
		Body:  gh.String(in.Description),
		Draft: gh.Bool(in.Draft),
	}
	pr, _, err := c.c.PullRequests.Create(ctx, c.owner, c.repo, newPR)
	if err != nil {
		return vcs.MergeRequest{}, fmt.Errorf("create pr: %w", err)
	}
	return mrFromPR(in.RepoName, pr), nil
}

func (c *client) GetMR(ctx context.Context, ref vcs.MergeRequest) (vcs.MergeRequest, error) {
	pr, _, err := c.c.PullRequests.Get(ctx, c.owner, c.repo, ref.Number)
	if err != nil {
		return vcs.MergeRequest{}, fmt.Errorf("get pr: %w", err)
	}
	return mrFromPR(ref.Repo, pr), nil
}

func (c *client) Comment(ctx context.Context, ref vcs.MergeRequest, body string) error {
	_, _, err := c.c.Issues.CreateComment(ctx, c.owner, c.repo, ref.Number,
		&gh.IssueComment{Body: gh.String(body)},
	)
	if err != nil {
		return fmt.Errorf("create issue comment: %w", err)
	}
	return nil
}

// ListComments returns issue comments on the PR. We don't include the
// file-level "review" comments (PullRequestsService.ListComments) for
// now — reviewers typically use the main thread for "please change X"
// and code-level comments are harder to respond to cleanly.
func (c *client) ListComments(ctx context.Context, ref vcs.MergeRequest, since time.Time) ([]vcs.ReviewComment, error) {
	opts := &gh.IssueListCommentsOptions{
		Sort:      gh.String("created"),
		Direction: gh.String("asc"),
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	if !since.IsZero() {
		s := since
		opts.Since = &s
	}
	var out []vcs.ReviewComment
	for {
		comments, resp, err := c.c.Issues.ListComments(ctx, c.owner, c.repo, ref.Number, opts)
		if err != nil {
			return nil, fmt.Errorf("list issue comments: %w", err)
		}
		for _, com := range comments {
			created := com.GetCreatedAt().Time
			if !since.IsZero() && !created.After(since) {
				continue
			}
			out = append(out, vcs.ReviewComment{
				ID:        strconv.FormatInt(com.GetID(), 10),
				Author:    com.GetUser().GetLogin(),
				Body:      com.GetBody(),
				CreatedAt: created,
				URL:       com.GetHTMLURL(),
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

// ReplyToComment posts a top-level issue comment that quotes the parent's
// first line. GitHub issue comments are flat — there's no native threading
// for PR conversation (code-review comments support threading, but this
// client only deals with issue comments).
func (c *client) ReplyToComment(ctx context.Context, ref vcs.MergeRequest, parent vcs.ReviewComment, body string) error {
	var b strings.Builder
	if parent.Author != "" && parent.Body != "" {
		first := strings.TrimSpace(strings.SplitN(parent.Body, "\n", 2)[0])
		if len(first) > 160 {
			first = first[:157] + "…"
		}
		fmt.Fprintf(&b, "> @%s: %s\n\n", parent.Author, first)
	}
	b.WriteString(body)
	return c.Comment(ctx, ref, b.String())
}

// GetCIStatus rolls up GitHub Checks API + the legacy combined Statuses
// API for the PR's head sha. Anything pending → pending; any failure →
// failing; otherwise passing. We collect failing run names + URLs so the
// `ci-failed` reaction has them in hand without re-querying.
func (c *client) GetCIStatus(ctx context.Context, ref vcs.MergeRequest) (vcs.CISummary, error) {
	pr, _, err := c.c.PullRequests.Get(ctx, c.owner, c.repo, ref.Number)
	if err != nil {
		return vcs.CISummary{Status: vcs.CIStatusUnknown}, fmt.Errorf("get pr: %w", err)
	}
	sha := pr.GetHead().GetSHA()
	out := vcs.CISummary{Status: vcs.CIStatusUnknown, HeadSHA: sha}
	if sha == "" {
		return out, nil
	}

	checks, _, err := c.c.Checks.ListCheckRunsForRef(ctx, c.owner, c.repo, sha,
		&gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}})
	if err != nil {
		return out, fmt.Errorf("list check runs: %w", err)
	}
	pending := false
	failing := false
	for _, run := range checks.CheckRuns {
		status := run.GetStatus()       // queued | in_progress | completed
		conclusion := run.GetConclusion() // success | failure | neutral | cancelled | skipped | timed_out | action_required
		if status != "completed" {
			pending = true
			continue
		}
		switch conclusion {
		case "failure", "timed_out", "action_required", "cancelled":
			failing = true
			out.FailingRuns = append(out.FailingRuns, vcs.CIRun{
				Name: run.GetName(), URL: run.GetHTMLURL(), Conclusion: conclusion,
			})
		}
	}

	combined, _, err := c.c.Repositories.GetCombinedStatus(ctx, c.owner, c.repo, sha,
		&gh.ListOptions{PerPage: 100})
	if err == nil {
		switch combined.GetState() {
		case "pending":
			pending = true
		case "failure", "error":
			failing = true
			for _, st := range combined.Statuses {
				if s := st.GetState(); s == "failure" || s == "error" {
					out.FailingRuns = append(out.FailingRuns, vcs.CIRun{
						Name: st.GetContext(), URL: st.GetTargetURL(), Conclusion: s,
					})
				}
			}
		}
	}

	switch {
	case failing:
		out.Status = vcs.CIStatusFailing
	case pending:
		out.Status = vcs.CIStatusPending
	default:
		out.Status = vcs.CIStatusPassing
	}
	return out, nil
}

// GetReviewDecision aggregates `pulls/{n}/reviews`. Latest review per
// reviewer wins (mirrors GitHub UI semantics). If any reviewer asked for
// changes → changes_requested; else if at least one approved →
// approved; else if reviewers exist with no decision → pending; else none.
func (c *client) GetReviewDecision(ctx context.Context, ref vcs.MergeRequest) (vcs.ReviewDecision, error) {
	opts := &gh.ListOptions{PerPage: 100}
	latest := map[string]string{} // login -> state
	for {
		reviews, resp, err := c.c.PullRequests.ListReviews(ctx, c.owner, c.repo, ref.Number, opts)
		if err != nil {
			return vcs.ReviewDecisionNone, fmt.Errorf("list reviews: %w", err)
		}
		for _, r := range reviews {
			user := r.GetUser().GetLogin()
			state := r.GetState() // APPROVED | CHANGES_REQUESTED | COMMENTED | DISMISSED | PENDING
			if state == "COMMENTED" || state == "DISMISSED" || state == "PENDING" {
				continue
			}
			latest[user] = state
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	if len(latest) == 0 {
		// No completed reviews. Distinguish between "reviewers were
		// requested but haven't reviewed yet" (pending) and "nobody asked
		// for review" (none).
		pr, _, err := c.c.PullRequests.Get(ctx, c.owner, c.repo, ref.Number)
		if err == nil && (len(pr.RequestedReviewers) > 0 || len(pr.RequestedTeams) > 0) {
			return vcs.ReviewDecisionPending, nil
		}
		return vcs.ReviewDecisionNone, nil
	}
	approved := false
	for _, state := range latest {
		if state == "CHANGES_REQUESTED" {
			return vcs.ReviewDecisionChangesRequested, nil
		}
		if state == "APPROVED" {
			approved = true
		}
	}
	if approved {
		return vcs.ReviewDecisionApproved, nil
	}
	return vcs.ReviewDecisionPending, nil
}

// MergeMR closes a PR by merging it. Method defaults to "squash" — most
// teams want a clean merge by default; override per board if needed.
func (c *client) MergeMR(ctx context.Context, ref vcs.MergeRequest, opts vcs.MergeOptions) error {
	method := opts.Method
	if method == "" {
		method = "squash"
	}
	_, _, err := c.c.PullRequests.Merge(ctx, c.owner, c.repo, ref.Number, opts.CommitMessage, &gh.PullRequestOptions{
		MergeMethod: method,
		CommitTitle: opts.CommitTitle,
	})
	if err != nil {
		return fmt.Errorf("merge pr: %w", err)
	}
	return nil
}

func (c *client) BotIdentity(ctx context.Context) (string, error) {
	c.botOnce.Do(func() {
		u, _, err := c.c.Users.Get(ctx, "")
		if err != nil {
			c.botErr = fmt.Errorf("github get authenticated user: %w", err)
			return
		}
		c.botID = u.GetLogin()
	})
	return c.botID, c.botErr
}

func mrFromPR(repoName string, pr *gh.PullRequest) vcs.MergeRequest {
	mr := vcs.MergeRequest{
		Repo:         repoName,
		Number:       pr.GetNumber(),
		URL:          pr.GetHTMLURL(),
		State:        pr.GetState(),
		Title:        pr.GetTitle(),
		SourceBranch: pr.GetHead().GetRef(),
		TargetBranch: pr.GetBase().GetRef(),
	}
	if pr.GetMerged() {
		mr.State = "merged"
	}
	if t := pr.GetClosedAt().Time; !t.IsZero() {
		mr.ClosedAt = &t
	}
	if t := pr.GetMergedAt().Time; !t.IsZero() {
		mr.MergedAt = &t
	}
	return mr
}

// parseOwnerRepo extracts "owner", "repo" from a clone URL.
func parseOwnerRepo(raw string) (string, string, error) {
	u, err := url.Parse(raw)
	if err == nil && u.Scheme != "" {
		p := strings.Trim(u.Path, "/")
		p = strings.TrimSuffix(p, ".git")
		parts := strings.SplitN(p, "/", 2)
		if len(parts) != 2 {
			return "", "", fmt.Errorf("cannot extract owner/repo from %q", raw)
		}
		return parts[0], parts[1], nil
	}
	if idx := strings.Index(raw, ":"); idx >= 0 {
		p := strings.TrimSuffix(raw[idx+1:], ".git")
		parts := strings.SplitN(p, "/", 2)
		if len(parts) != 2 {
			return "", "", fmt.Errorf("cannot extract owner/repo from %q", raw)
		}
		return parts[0], parts[1], nil
	}
	return "", "", fmt.Errorf("cannot parse repo url %q", raw)
}

func hostFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err == nil && u.Host != "" {
		return u.Host
	}
	if at := strings.Index(raw, "@"); at >= 0 {
		rest := raw[at+1:]
		if colon := strings.Index(rest, ":"); colon >= 0 {
			return rest[:colon]
		}
	}
	return ""
}

func init() {
	vcs.Register("github", func(repo config.BoardRepo) (vcs.Client, error) {
		tok := os.Getenv(repo.VCS.Auth.Env)
		if tok == "" {
			return nil, fmt.Errorf("github token env %s is empty", repo.VCS.Auth.Env)
		}
		owner, name, err := parseOwnerRepo(repo.URL)
		if err != nil {
			return nil, err
		}
		host := repo.VCS.Host
		if host == "" {
			host = hostFromURL(repo.URL)
		}

		c := gh.NewClient(nil).WithAuthToken(tok)
		if host != "" && host != "github.com" {
			base := "https://" + host + "/api/v3/"
			uploadBase := "https://" + host + "/api/uploads/"
			c, err = c.WithEnterpriseURLs(base, uploadBase)
			if err != nil {
				return nil, fmt.Errorf("github enterprise urls: %w", err)
			}
		}
		return &client{c: c, owner: owner, repo: name, host: host}, nil
	})
}
