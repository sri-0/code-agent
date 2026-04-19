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
	"strings"

	glab "github.com/xanzy/go-gitlab"

	"code-agent/internal/config"
	"code-agent/pkg/vcs"
)

type client struct {
	c           *glab.Client
	projectPath string // "group/sub/project"
	host        string
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
	return vcs.MergeRequest{
		Repo:  in.RepoName,
		IID:   mr.IID,
		URL:   mr.WebURL,
		State: mr.State,
		Title: mr.Title,
	}, nil
}

func (c *client) GetMR(ctx context.Context, ref vcs.MergeRequest) (vcs.MergeRequest, error) {
	mr, _, err := c.c.MergeRequests.GetMergeRequest(c.projectPath, ref.IID, nil, glab.WithContext(ctx))
	if err != nil {
		return vcs.MergeRequest{}, fmt.Errorf("get mr: %w", err)
	}
	return vcs.MergeRequest{
		Repo:  ref.Repo,
		IID:   mr.IID,
		URL:   mr.WebURL,
		State: mr.State,
		Title: mr.Title,
	}, nil
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
