package actions

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	"code-agent/internal/config"
	"code-agent/pkg/reactions"
	"code-agent/pkg/vcs"
)

// AutoMerge merges an approved+green PR via the VCS client. Method is
// taken from the per-board reactions config; defaults to squash.
type AutoMerge struct {
	// MergeOptionsForKey returns the merge method/title/message
	// configured for the given event on the given board. Allows the
	// orchestrator to pick squash vs merge vs rebase per-board without
	// AutoMerge taking a config dep itself.
	MergeOptionsForKey func(b config.Board, key reactions.EventKey) vcs.MergeOptions
	Logger             zerolog.Logger
}

func (a *AutoMerge) Name() string { return "auto-merge" }

func (a *AutoMerge) Run(ctx context.Context, e reactions.Event) error {
	if e.Key != reactions.EventApprovedAndGreen {
		return fmt.Errorf("auto-merge only valid for approved-and-green; got %s", e.Key)
	}
	if len(e.Task.MergeRequests) == 0 {
		return fmt.Errorf("no merge requests to merge")
	}
	for _, mr := range e.Task.MergeRequests {
		if mr.State != "" && mr.State != "open" {
			continue
		}
		br, ok := findBoardRepo(e.Board, mr.Repo)
		if !ok {
			a.Logger.Warn().Str("repo", mr.Repo).Msg("repo not in board; skipping")
			continue
		}
		client, err := vcs.Build(br)
		if err != nil {
			return fmt.Errorf("vcs.Build: %w", err)
		}
		opts := vcs.MergeOptions{Method: "squash"}
		if a.MergeOptionsForKey != nil {
			opts = a.MergeOptionsForKey(e.Board, e.Key)
		}
		ref := vcs.MergeRequest{Repo: mr.Repo, IID: mr.IID, Number: mr.Number, URL: mr.URL}
		if err := client.MergeMR(ctx, ref, opts); err != nil {
			return fmt.Errorf("merge %s: %w", mr.URL, err)
		}
		a.Logger.Info().Str("pr", mr.URL).Str("method", opts.Method).Msg("auto-merged")
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
