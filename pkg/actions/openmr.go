// Package actions hosts stages.ActionRunner implementations that don't fit
// neatly in pkg/runtime (which is for runtime-adjacent actions like
// run_agent). Each action is standalone and registered in the orchestrator.
//
// OpenMRRunner is the "commit → push → open PR" action. It's orchestrator-
// driven: the agent leaves modified/created files on disk, this action
// detects them, generates a commit message and PR body via an LLM call
// (pkg/llm), commits + pushes to ai/<ticket-id> per repo, and opens a PR
// against the configured base branch.
package actions

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"code-agent/internal/config"
	"code-agent/pkg/llm"
	"code-agent/pkg/stages"
	"code-agent/pkg/tasks"
	"code-agent/pkg/vcs"
)

// OpenMRRunner commits + pushes + opens PRs for a task. VCS clients and the
// LLM client are cached per-runner.
type OpenMRRunner struct {
	mu      sync.Mutex
	clients map[string]vcs.Client // repo_name -> client
	llm     *llm.Client           // nil if no API key configured
	llmErr  error
}

func NewOpenMRRunner() *OpenMRRunner {
	r := &OpenMRRunner{clients: map[string]vcs.Client{}}
	c, err := llm.New(llm.Options{})
	if err != nil {
		r.llmErr = err
	} else {
		r.llm = c
	}
	return r
}

// Run satisfies stages.ActionRunner.
func (r *OpenMRRunner) Run(ctx context.Context, in stages.ActionInput) (stages.ActionResult, error) {
	logger := in.Logger.With().Str("action", "open_mr").Logger()

	if len(in.Task.Repos) == 0 {
		return stages.ActionResult{Outcome: "success", Comment: "open_mr: no repos configured"}, nil
	}

	workspace := os.Getenv("CODE_AGENT_WORKSPACE")
	if workspace == "" {
		return stages.ActionResult{Outcome: "failure"}, fmt.Errorf("CODE_AGENT_WORKSPACE env not set — cannot locate repos on disk")
	}

	if r.llm == nil {
		logger.Warn().Err(r.llmErr).Msg("LLM client unavailable; falling back to template commit/pr text")
	}

	var opened []vcs.MergeRequest
	var newRefs []tasks.MergeRef
	var summary bytes.Buffer

	for _, rp := range in.Task.Repos {
		repoDir := filepath.Join(workspace, rp.Name)
		if _, err := os.Stat(filepath.Join(repoDir, ".git")); err != nil {
			logger.Warn().Str("repo", rp.Name).Str("dir", repoDir).Msg("not a git checkout; skipping")
			continue
		}
		if alreadyOpen(in.Task.MergeRequests, rp.Name) {
			logger.Info().Str("repo", rp.Name).Msg("MR already exists; skipping")
			continue
		}

		// any local changes (tracked + untracked)?
		statCtx, cancelStat := context.WithTimeout(ctx, 30*time.Second)
		dirty, err := hasChanges(statCtx, repoDir)
		cancelStat()
		if err != nil {
			return stages.ActionResult{Outcome: "failure"}, fmt.Errorf("git status %s: %w", rp.Name, err)
		}
		if !dirty {
			logger.Info().Str("repo", rp.Name).Msg("no local changes; skipping")
			continue
		}

		branch := rp.Branch
		if branch == "" {
			branch = "ai/" + in.Task.ExternalID
		}

		// stage everything first so `git diff --cached` covers untracked too
		if err := runGit(ctx, repoDir, "checkout", "-B", branch, rp.BaseBranch); err != nil {
			return stages.ActionResult{Outcome: "failure"}, fmt.Errorf("checkout %s %s: %w", rp.Name, branch, err)
		}
		// bring files forward onto the new branch
		// (checkout -B already keeps working tree; nothing to do)
		if err := runGit(ctx, repoDir, "add", "-A"); err != nil {
			return stages.ActionResult{Outcome: "failure"}, fmt.Errorf("git add %s: %w", rp.Name, err)
		}
		diffCtx, cancelDiff := context.WithTimeout(ctx, 60*time.Second)
		diff, err := gitDiffCached(diffCtx, repoDir)
		cancelDiff()
		if err != nil {
			return stages.ActionResult{Outcome: "failure"}, fmt.Errorf("git diff --cached %s: %w", rp.Name, err)
		}
		if strings.TrimSpace(diff) == "" {
			logger.Info().Str("repo", rp.Name).Msg("diff empty after staging; skipping")
			continue
		}

		// LLM-generated commit + PR text
		var change llm.ChangeSummary
		if r.llm != nil {
			llmCtx, cancelLLM := context.WithTimeout(ctx, 90*time.Second)
			s, err := r.llm.SummarizeChange(llmCtx, in.Task.ExternalID, in.Task.Title, in.Task.Description, rp.Name, diff)
			cancelLLM()
			change = s
			if err != nil {
				logger.Warn().Err(err).Str("repo", rp.Name).Msg("LLM summary degraded; using fallback text")
			}
		} else {
			change = llm.ChangeSummary{
				CommitMessage: fmt.Sprintf("%s %s", in.Task.ExternalID, in.Task.Title),
				PRTitle:       fmt.Sprintf("%s %s", in.Task.ExternalID, in.Task.Title),
				PRBody:        fmt.Sprintf("Automated change for %s (%s).", in.Task.ExternalID, rp.Name),
			}
		}

		// commit
		if err := runGit(ctx, repoDir, "commit", "-m", change.CommitMessage); err != nil {
			return stages.ActionResult{Outcome: "failure"}, fmt.Errorf("git commit %s: %w", rp.Name, err)
		}
		// push
		pushCtx, cancelPush := context.WithTimeout(ctx, 120*time.Second)
		if err := runGitCtx(pushCtx, repoDir, "push", "-u", "origin", branch); err != nil {
			cancelPush()
			return stages.ActionResult{Outcome: "failure"}, fmt.Errorf("git push %s: %w", rp.Name, err)
		}
		cancelPush()
		logger.Info().Str("repo", rp.Name).Str("branch", branch).Msg("branch pushed")

		// open PR
		boardRepo, ok := findBoardRepo(in.Board, rp.Name)
		if !ok {
			logger.Warn().Str("repo", rp.Name).Msg("repo not in board config; skipping PR open")
			continue
		}
		vcli, err := r.clientFor(boardRepo)
		if err != nil {
			return stages.ActionResult{Outcome: "failure"}, fmt.Errorf("vcs client for %s: %w", rp.Name, err)
		}
		mr, err := vcli.OpenMR(ctx, vcs.OpenMRRequest{
			RepoName:     rp.Name,
			SourceBranch: branch,
			TargetBranch: rp.BaseBranch,
			Title:        change.PRTitle,
			Description:  change.PRBody,
		})
		if err != nil {
			return stages.ActionResult{Outcome: "failure"}, fmt.Errorf("open mr %s: %w", rp.Name, err)
		}
		logger.Info().Str("repo", rp.Name).Str("url", mr.URL).Msg("PR opened")
		opened = append(opened, mr)
		newRefs = append(newRefs, tasks.MergeRef{
			Repo:   rp.Name,
			URL:    mr.URL,
			IID:    mr.IID,
			Number: mr.Number,
			State:  "open",
		})
		fmt.Fprintf(&summary, "- %s: %s\n", rp.Name, mr.URL)
	}

	if len(opened) == 0 {
		return stages.ActionResult{Outcome: "success", Comment: "open_mr: no changes to commit"}, nil
	}
	if len(opened) > 1 {
		r.crosslink(ctx, logger, opened)
	}
	return stages.ActionResult{
		Outcome: "success",
		Comment: "Opened PRs:\n" + summary.String(),
		Mutate: func(t *tasks.Task) {
			t.MergeRequests = append(t.MergeRequests, newRefs...)
		},
	}, nil
}

func (r *OpenMRRunner) clientFor(repo config.BoardRepo) (vcs.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.clients[repo.Name]; ok {
		return c, nil
	}
	c, err := vcs.Build(repo)
	if err != nil {
		return nil, err
	}
	r.clients[repo.Name] = c
	return c, nil
}

func (r *OpenMRRunner) crosslink(ctx context.Context, logger zerolog.Logger, mrs []vcs.MergeRequest) {
	for i := range mrs {
		var b bytes.Buffer
		b.WriteString("Related MRs/PRs for this ticket:\n")
		for j, mr := range mrs {
			b.WriteString("- ")
			b.WriteString(mr.Repo)
			b.WriteString(": ")
			b.WriteString(mr.URL)
			if j == i {
				b.WriteString("  (this)")
			}
			b.WriteByte('\n')
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		c, ok := r.clients[mrs[i].Repo]
		if !ok {
			cancel()
			continue
		}
		if err := c.Comment(cctx, mrs[i], b.String()); err != nil {
			logger.Warn().Err(err).Str("repo", mrs[i].Repo).Msg("crosslink comment failed")
		}
		cancel()
	}
}

// ---- git helpers ----

func hasChanges(ctx context.Context, dir string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func gitDiffCached(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "diff", "--cached", "--no-color")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func runGit(ctx context.Context, dir string, args ...string) error {
	return runGitCtx(ctx, dir, args...)
}

func runGitCtx(ctx context.Context, dir string, args ...string) error {
	full := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ---- helpers retained from previous impl ----

func findBoardRepo(b config.Board, name string) (config.BoardRepo, bool) {
	for _, r := range b.Repos {
		if r.Name == name {
			return r, true
		}
	}
	return config.BoardRepo{}, false
}

func alreadyOpen(existing []tasks.MergeRef, repoName string) bool {
	for _, m := range existing {
		if m.Repo == repoName && (m.State == "" || m.State == "open") {
			return true
		}
	}
	return false
}
