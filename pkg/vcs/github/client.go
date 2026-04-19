// Package github implements vcs.Client against GitHub (github.com or
// self-hosted Enterprise). Auth via personal/bot token from env.
package github

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	gh "github.com/google/go-github/v67/github"

	"code-agent/internal/config"
	"code-agent/pkg/vcs"
)

type client struct {
	c     *gh.Client
	owner string
	repo  string
	host  string
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
	return vcs.MergeRequest{
		Repo:   in.RepoName,
		Number: pr.GetNumber(),
		URL:    pr.GetHTMLURL(),
		State:  pr.GetState(),
		Title:  pr.GetTitle(),
	}, nil
}

func (c *client) GetMR(ctx context.Context, ref vcs.MergeRequest) (vcs.MergeRequest, error) {
	pr, _, err := c.c.PullRequests.Get(ctx, c.owner, c.repo, ref.Number)
	if err != nil {
		return vcs.MergeRequest{}, fmt.Errorf("get pr: %w", err)
	}
	return vcs.MergeRequest{
		Repo:   ref.Repo,
		Number: pr.GetNumber(),
		URL:    pr.GetHTMLURL(),
		State:  pr.GetState(),
		Title:  pr.GetTitle(),
	}, nil
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
