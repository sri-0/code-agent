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
