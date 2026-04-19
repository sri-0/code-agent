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

	"code-agent/internal/config"
)

// MergeRequest is the provider-neutral view of a merge/pull request.
type MergeRequest struct {
	Repo   string // boards.yaml repo name
	Number int    // GitHub PR number (or 0)
	IID    int    // GitLab MR IID (or 0)
	URL    string
	State  string // open | merged | closed
	Title  string
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

// Client is the MR/PR API surface we need.
type Client interface {
	Provider() string // "gitlab" | "github"
	OpenMR(ctx context.Context, in OpenMRRequest) (MergeRequest, error)
	GetMR(ctx context.Context, ref MergeRequest) (MergeRequest, error)
	Comment(ctx context.Context, ref MergeRequest, body string) error
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
