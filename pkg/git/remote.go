// Package git provides small go-git helpers the orchestrator needs. The
// heavy lifting (commits, pushes) happens on the worker; this package is
// for *checks* — e.g. confirming that a branch actually exists on origin
// before open_mr runs.
package git

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	gogithttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
)

// RemoteBranchExists returns true if `branch` is present on `remoteURL`.
// It runs a ls-remote against the URL (in-memory; no clone). Auth uses
// HTTP basic with username "x-access-token" and the provided token when
// the URL is http(s); empty token means anonymous.
func RemoteBranchExists(ctx context.Context, remoteURL, branch, token string) (bool, error) {
	if branch == "" {
		return false, fmt.Errorf("branch required")
	}
	// Create an in-memory repo just so we can LS the remote.
	repo, err := git.InitWithOptions(memory.NewStorage(), nil, git.InitOptions{
		DefaultBranch: "refs/heads/_init",
	})
	if err != nil {
		return false, fmt.Errorf("git init memory: %w", err)
	}
	rem, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{remoteURL}})
	if err != nil {
		return false, fmt.Errorf("create remote: %w", err)
	}
	opts := &git.ListOptions{}
	if token != "" && isHTTP(remoteURL) {
		opts.Auth = &gogithttp.BasicAuth{Username: "x-access-token", Password: token}
	}
	refs, err := rem.ListContext(ctx, opts)
	if err != nil {
		return false, fmt.Errorf("ls-remote: %w", err)
	}
	target := "refs/heads/" + branch
	for _, r := range refs {
		if r.Name().String() == target {
			return true, nil
		}
	}
	return false, nil
}

func isHTTP(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https")
}
