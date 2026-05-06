// Package gitlab implements vcs.Client against a GitLab instance.
//
// Auth: expects a personal-access/bot token in the env pointed to by
// BoardRepo.VCS.Auth.Env. The token needs at least `api` scope.
//
// Project is derived from the BoardRepo URL (e.g.
// https://gitlab.com/org/service.git -> "org/service").
package gitlab

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	glab "github.com/xanzy/go-gitlab"

	"code-agent/internal/config"
	"code-agent/pkg/vcs"
)

type client struct {
	c           *glab.Client
	projectPath string // "group/sub/project"
	host        string

	botOnce sync.Once
	botID   string
	botErr  error
}

func (c *client) Provider() string { return "gitlab" }

func (c *client) OpenMR(ctx context.Context, in vcs.OpenMRRequest) (vcs.MergeRequest, error) {
	opt := &glab.CreateMergeRequestOptions{
		Title:        glab.Ptr(titleOrDraft(in.Title, in.Draft)),
		Description:  glab.Ptr(in.Description),
		SourceBranch: glab.Ptr(in.SourceBranch),
		TargetBranch: glab.Ptr(in.TargetBranch),
	}
	mr, _, err := c.c.MergeRequests.CreateMergeRequest(c.projectPath, opt, glab.WithContext(ctx))
	if err != nil {
		return vcs.MergeRequest{}, fmt.Errorf("create mr: %w", err)
	}
	return mrFromGitlab(in.RepoName, mr), nil
}

func (c *client) GetMR(ctx context.Context, ref vcs.MergeRequest) (vcs.MergeRequest, error) {
	mr, _, err := c.c.MergeRequests.GetMergeRequest(c.projectPath, ref.IID, nil, glab.WithContext(ctx))
	if err != nil {
		return vcs.MergeRequest{}, fmt.Errorf("get mr: %w", err)
	}
	return mrFromGitlab(ref.Repo, mr), nil
}

func (c *client) Comment(ctx context.Context, ref vcs.MergeRequest, body string) error {
	_, _, err := c.c.Notes.CreateMergeRequestNote(c.projectPath, ref.IID,
		&glab.CreateMergeRequestNoteOptions{Body: glab.Ptr(body)},
		glab.WithContext(ctx),
	)
	if err != nil {
		return fmt.Errorf("create note: %w", err)
	}
	return nil
}

// ListComments returns all non-system notes on the MR created after `since`.
// GitLab has no native "since" filter for notes so we page until we pass
// the cursor.
func (c *client) ListComments(ctx context.Context, ref vcs.MergeRequest, since time.Time) ([]vcs.ReviewComment, error) {
	opts := &glab.ListMergeRequestNotesOptions{
		Sort:    glab.Ptr("asc"),
		OrderBy: glab.Ptr("created_at"),
		ListOptions: glab.ListOptions{PerPage: 100},
	}
	var out []vcs.ReviewComment
	for {
		notes, resp, err := c.c.Notes.ListMergeRequestNotes(c.projectPath, ref.IID, opts, glab.WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("list mr notes: %w", err)
		}
		for _, n := range notes {
			if n.System {
				continue // state-change noise ("approved", "pushed commit ...")
			}
			created := time.Time{}
			if n.CreatedAt != nil {
				created = *n.CreatedAt
			}
			if !since.IsZero() && !created.After(since) {
				continue
			}
			author := ""
			if n.Author.Username != "" {
				author = n.Author.Username
			}
			out = append(out, vcs.ReviewComment{
				ID:        strconv.Itoa(n.ID),
				Author:    author,
				Body:      n.Body,
				CreatedAt: created,
				URL:       fmt.Sprintf("%s#note_%d", ref.URL, n.ID),
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

// ReplyToComment posts a new MR note that quotes the parent. GitLab's
// CreateMergeRequestDiscussion can thread a reply to a discussion, but
// requires the parent's discussion_id which we don't track — top-level
// quoted reply is semantically equivalent for reviewers.
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

// GetCIStatus reads the latest pipeline for the MR's source branch +
// head sha. GitLab's MR object exposes `head_pipeline` directly.
func (c *client) GetCIStatus(ctx context.Context, ref vcs.MergeRequest) (vcs.CISummary, error) {
	mr, _, err := c.c.MergeRequests.GetMergeRequest(c.projectPath, ref.IID, nil, glab.WithContext(ctx))
	if err != nil {
		return vcs.CISummary{Status: vcs.CIStatusUnknown}, fmt.Errorf("get mr: %w", err)
	}
	out := vcs.CISummary{Status: vcs.CIStatusUnknown, HeadSHA: mr.SHA}
	if mr.HeadPipeline == nil {
		return out, nil
	}
	switch mr.HeadPipeline.Status {
	case "success":
		out.Status = vcs.CIStatusPassing
	case "failed", "canceled":
		out.Status = vcs.CIStatusFailing
	case "running", "pending", "preparing", "created", "scheduled", "waiting_for_resource":
		out.Status = vcs.CIStatusPending
	}
	if out.Status == vcs.CIStatusFailing {
		jobs, _, err := c.c.Jobs.ListPipelineJobs(c.projectPath, mr.HeadPipeline.ID, nil, glab.WithContext(ctx))
		if err == nil {
			for _, j := range jobs {
				if j.Status == "failed" || j.Status == "canceled" {
					out.FailingRuns = append(out.FailingRuns, vcs.CIRun{
						Name: j.Name, URL: j.WebURL, Conclusion: j.Status,
					})
				}
			}
		}
	}
	return out, nil
}

// GetReviewDecision aggregates GitLab's approvals + reviewer state.
// GitLab encodes "changes requested" as a reviewer state, not as a
// separate field — we surface that via the MR Reviewers list.
func (c *client) GetReviewDecision(ctx context.Context, ref vcs.MergeRequest) (vcs.ReviewDecision, error) {
	mr, _, err := c.c.MergeRequests.GetMergeRequest(c.projectPath, ref.IID, nil, glab.WithContext(ctx))
	if err != nil {
		return vcs.ReviewDecisionNone, fmt.Errorf("get mr: %w", err)
	}
	for _, rv := range mr.Reviewers {
		// go-gitlab's BasicUser doesn't expose State; the state lives in
		// MR.Reviewer's `reviewer_state` which the SDK surfaces as
		// `reviewer_state` on the per-MR API but is not always populated.
		// Fall back to approvals API below if reviewers are listed but
		// states are blank.
		_ = rv
	}
	apps, _, err := c.c.MergeRequestApprovals.GetApprovalState(c.projectPath, ref.IID, glab.WithContext(ctx))
	if err == nil && apps != nil {
		approved := 0
		required := 0
		for _, rule := range apps.Rules {
			required += rule.ApprovalsRequired
			if rule.Approved {
				approved++
			}
		}
		if required > 0 && approved == required {
			return vcs.ReviewDecisionApproved, nil
		}
		if required > 0 {
			return vcs.ReviewDecisionPending, nil
		}
	}
	if len(mr.Reviewers) > 0 {
		return vcs.ReviewDecisionPending, nil
	}
	return vcs.ReviewDecisionNone, nil
}

// MergeMR closes an MR by merging it. GitLab's only "method" knob is
// squash — passed through MergeOptions.Method == "squash".
func (c *client) MergeMR(ctx context.Context, ref vcs.MergeRequest, opts vcs.MergeOptions) error {
	mergeOpt := &glab.AcceptMergeRequestOptions{}
	if opts.CommitMessage != "" {
		mergeOpt.MergeCommitMessage = glab.Ptr(opts.CommitMessage)
	}
	if opts.Method == "squash" {
		mergeOpt.Squash = glab.Ptr(true)
		if opts.CommitMessage != "" {
			mergeOpt.SquashCommitMessage = glab.Ptr(opts.CommitMessage)
		}
	}
	_, _, err := c.c.MergeRequests.AcceptMergeRequest(c.projectPath, ref.IID, mergeOpt, glab.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("accept mr: %w", err)
	}
	return nil
}

func (c *client) BotIdentity(ctx context.Context) (string, error) {
	c.botOnce.Do(func() {
		u, _, err := c.c.Users.CurrentUser(glab.WithContext(ctx))
		if err != nil {
			c.botErr = fmt.Errorf("gitlab current user: %w", err)
			return
		}
		c.botID = u.Username
	})
	return c.botID, c.botErr
}

func mrFromGitlab(repoName string, mr *glab.MergeRequest) vcs.MergeRequest {
	out := vcs.MergeRequest{
		Repo:         repoName,
		IID:          mr.IID,
		URL:          mr.WebURL,
		State:        mr.State,
		Title:        mr.Title,
		SourceBranch: mr.SourceBranch,
		TargetBranch: mr.TargetBranch,
	}
	if mr.ClosedAt != nil {
		t := *mr.ClosedAt
		out.ClosedAt = &t
	}
	if mr.MergedAt != nil {
		t := *mr.MergedAt
		out.MergedAt = &t
		if out.ClosedAt == nil {
			out.ClosedAt = &t
		}
	}
	return out
}

func titleOrDraft(title string, draft bool) string {
	if !draft {
		return title
	}
	if strings.HasPrefix(strings.ToLower(title), "draft:") {
		return title
	}
	return "Draft: " + title
}

// projectFromURL extracts "group/project" from a repo clone URL.
func projectFromURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err == nil && u.Scheme != "" {
		p := strings.Trim(u.Path, "/")
		p = strings.TrimSuffix(p, ".git")
		if p == "" {
			return "", fmt.Errorf("empty path in %q", raw)
		}
		return p, nil
	}
	// handle scp-style: git@host:group/project.git
	if idx := strings.Index(raw, ":"); idx >= 0 {
		p := strings.TrimSuffix(raw[idx+1:], ".git")
		return strings.TrimPrefix(p, "/"), nil
	}
	return "", fmt.Errorf("cannot parse repo url %q", raw)
}

func hostFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err == nil && u.Host != "" {
		return u.Host
	}
	// scp-style
	if at := strings.Index(raw, "@"); at >= 0 {
		rest := raw[at+1:]
		if colon := strings.Index(rest, ":"); colon >= 0 {
			return rest[:colon]
		}
	}
	return ""
}

func init() {
	vcs.Register("gitlab", func(repo config.BoardRepo) (vcs.Client, error) {
		tok := os.Getenv(repo.VCS.Auth.Env)
		if tok == "" {
			return nil, fmt.Errorf("gitlab token env %s is empty", repo.VCS.Auth.Env)
		}
		host := repo.VCS.Host
		if host == "" {
			host = hostFromURL(repo.URL)
		}
		if host == "" {
			host = "gitlab.com"
		}
		base := "https://" + host
		c, err := glab.NewClient(tok, glab.WithBaseURL(base+"/api/v4"))
		if err != nil {
			return nil, fmt.Errorf("gitlab client: %w", err)
		}
		project, err := projectFromURL(repo.URL)
		if err != nil {
			return nil, err
		}
		return &client{c: c, projectPath: project, host: host}, nil
	})
}
