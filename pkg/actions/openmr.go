// Package actions hosts stages.ActionRunner implementations that don't fit
// neatly in pkg/runtime (which is for runtime-adjacent actions like
// run_agent). Each action is standalone and registered in the orchestrator.
//
// OpenMRRunner is the "commit → push → open PR" action. It has two code
// paths:
//
//  1. Pod-based runtimes (ephemeral/persistent/shared): the task's
//     WorkerRef.AdminURL is non-empty. The orchestrator calls
//     cmd/runner's admin HTTP over that URL to get the diff, craft a
//     commit message + PR body via LLM, then POST /commit-push to have
//     the runner do the actual git. Orchestrator never touches the
//     pod's filesystem.
//
//  2. Local runtime: AdminURL is empty, CODE_AGENT_WORKSPACE env points
//     at the host dir containing the repos. Orchestrator shells git
//     directly against that dir.
package actions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

type OpenMRRunner struct {
	mu      sync.Mutex
	clients map[string]vcs.Client
	llm     *llm.Client
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

func (r *OpenMRRunner) Run(ctx context.Context, in stages.ActionInput) (stages.ActionResult, error) {
	logger := in.Logger.With().Str("action", "open_mr").Logger()

	if len(in.Task.Repos) == 0 {
		return stages.ActionResult{Outcome: "success", Comment: "open_mr: no repos configured"}, nil
	}
	if r.llm == nil {
		logger.Warn().Err(r.llmErr).Msg("LLM client unavailable; falling back to template commit/pr text")
	}

	// Decide mode based on whether the worker exposes an admin HTTP.
	adminURL := in.Task.WorkerRef.AdminURL
	var driver gitDriver
	if adminURL != "" {
		driver = &podDriver{adminURL: adminURL, logger: logger}
		logger.Info().Str("admin_url", adminURL).Msg("using pod admin HTTP for git ops")
	} else {
		workspace := os.Getenv("CODE_AGENT_WORKSPACE")
		if workspace == "" {
			return stages.ActionResult{Outcome: "failure"}, fmt.Errorf("no pod AdminURL and CODE_AGENT_WORKSPACE unset — cannot do git")
		}
		driver = &hostDriver{workspace: workspace, logger: logger}
		logger.Info().Str("workspace", workspace).Msg("using host filesystem for git ops")
	}

	var opened []vcs.MergeRequest
	var newRefs []tasks.MergeRef
	var summary bytes.Buffer

	for _, rp := range in.Task.Repos {
		if alreadyOpen(in.Task.MergeRequests, rp.Name) {
			logger.Info().Str("repo", rp.Name).Msg("MR already exists; skipping")
			continue
		}
		branch := rp.Branch
		if branch == "" {
			branch = "ai/" + in.Task.ExternalID
		}

		statCtx, cancelStat := context.WithTimeout(ctx, 30*time.Second)
		dirty, diff, err := driver.getStatusAndDiff(statCtx, rp.Name)
		cancelStat()
		if err != nil {
			return stages.ActionResult{
				Outcome: "failure",
				Comment: openMRFailureComment(in, rp.Name, "git status/diff failed: "+err.Error()),
			}, fmt.Errorf("status %s: %w", rp.Name, err)
		}
		if !dirty || strings.TrimSpace(diff) == "" {
			logger.Info().Str("repo", rp.Name).Msg("no local changes; skipping")
			continue
		}

		// LLM commit/PR summary
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

		// commit + push via the driver
		pushCtx, cancelPush := context.WithTimeout(ctx, 180*time.Second)
		if err := driver.commitAndPush(pushCtx, rp.Name, branch, rp.BaseBranch, change.CommitMessage); err != nil {
			cancelPush()
			return stages.ActionResult{
				Outcome: "failure",
				Comment: openMRFailureComment(in, rp.Name, "commit/push to "+branch+" failed: "+err.Error()),
			}, fmt.Errorf("commit/push %s: %w", rp.Name, err)
		}
		cancelPush()
		logger.Info().Str("repo", rp.Name).Str("branch", branch).Msg("branch pushed")

		// open PR via VCS API
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
			return stages.ActionResult{
				Outcome: "failure",
				Comment: openMRFailureComment(in, rp.Name, "PR open failed: "+err.Error()),
			}, fmt.Errorf("open mr %s: %w", rp.Name, err)
		}
		logger.Info().Str("repo", rp.Name).Str("url", mr.URL).Msg("PR opened")
		opened = append(opened, mr)
		newRefs = append(newRefs, tasks.MergeRef{
			Repo:         rp.Name,
			URL:          mr.URL,
			IID:          mr.IID,
			Number:       mr.Number,
			State:        "open",
			SourceBranch: branch,
			TargetBranch: rp.BaseBranch,
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

func openMRFailureComment(in stages.ActionInput, repoName, reason string) string {
	return "🤖 code-agent: stage `" + in.Stage.Name + "` failed for " + in.Task.ExternalID + " on repo `" + repoName + "`. " + reason
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

// ---- gitDriver: two implementations (pod via HTTP, host via shell) ----

type gitDriver interface {
	getStatusAndDiff(ctx context.Context, repoName string) (dirty bool, diff string, err error)
	commitAndPush(ctx context.Context, repoName, branch, baseBranch, commitMessage string) error
}

// PodGitDriver is the exported shape of the pod-side git driver.
// Used by pkg/actions/review to commit+push feedback edits on the feature
// branch without rebuilding the whole action.
type PodGitDriver interface {
	GetStatusAndDiff(ctx context.Context, repoName string) (dirty bool, diff string, err error)
	CommitAndPush(ctx context.Context, repoName, branch, baseBranch, commitMessage string) error
}

// NewPodDriver constructs a PodGitDriver talking to the cmd/runner admin
// HTTP at adminURL. Thin wrapper over the internal podDriver.
func NewPodDriver(adminURL string, logger zerolog.Logger) PodGitDriver {
	return &exportedPodDriver{inner: &podDriver{adminURL: adminURL, logger: logger}}
}

type exportedPodDriver struct{ inner *podDriver }

func (e *exportedPodDriver) GetStatusAndDiff(ctx context.Context, repo string) (bool, string, error) {
	return e.inner.getStatusAndDiff(ctx, repo)
}
func (e *exportedPodDriver) CommitAndPush(ctx context.Context, repo, branch, base, msg string) error {
	return e.inner.commitAndPush(ctx, repo, branch, base, msg)
}

// podDriver talks to cmd/runner's admin HTTP.
type podDriver struct {
	adminURL string
	logger   zerolog.Logger
}

func (p *podDriver) getStatusAndDiff(ctx context.Context, repoName string) (bool, string, error) {
	// /repos/{name}/status -> {dirty, changes[]}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.adminURL+"/repos/"+repoName+"/status", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, "", fmt.Errorf("admin status: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return false, "", fmt.Errorf("admin status http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var s struct {
		Dirty   bool     `json:"dirty"`
		Changes []string `json:"changes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return false, "", fmt.Errorf("admin status decode: %w", err)
	}
	if !s.Dirty {
		return false, "", nil
	}
	// Fetch diff
	req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.adminURL+"/repos/"+repoName+"/diff", nil)
	r2, err := http.DefaultClient.Do(req2)
	if err != nil {
		return true, "", fmt.Errorf("admin diff: %w", err)
	}
	defer r2.Body.Close()
	if r2.StatusCode >= 300 {
		body, _ := io.ReadAll(r2.Body)
		return true, "", fmt.Errorf("admin diff http %d: %s", r2.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(r2.Body)
	if err != nil {
		return true, "", err
	}
	return true, string(body), nil
}

func (p *podDriver) commitAndPush(ctx context.Context, repoName, branch, baseBranch, msg string) error {
	body, _ := json.Marshal(map[string]string{
		"branch":         branch,
		"base_branch":    baseBranch,
		"commit_message": msg,
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.adminURL+"/repos/"+repoName+"/commit-push", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("admin commit-push: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("admin commit-push http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// hostDriver shells git directly on the orchestrator's filesystem.
type hostDriver struct {
	workspace string
	logger    zerolog.Logger
}

func (h *hostDriver) repoDir(name string) string { return filepath.Join(h.workspace, name) }

func (h *hostDriver) getStatusAndDiff(ctx context.Context, repoName string) (bool, string, error) {
	dir := h.repoDir(repoName)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return false, "", nil
	}
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "status", "--porcelain").Output()
	if err != nil {
		return false, "", err
	}
	if strings.TrimSpace(string(out)) == "" {
		return false, "", nil
	}
	if err := shellGit(ctx, dir, "add", "-A"); err != nil {
		return true, "", err
	}
	diff, err := exec.CommandContext(ctx, "git", "-C", dir, "diff", "--cached", "--no-color").Output()
	if err != nil {
		return true, "", err
	}
	return true, string(diff), nil
}

func (h *hostDriver) commitAndPush(ctx context.Context, repoName, branch, baseBranch, msg string) error {
	dir := h.repoDir(repoName)
	steps := [][]string{
		{"checkout", "-B", branch, baseBranch},
		{"add", "-A"},
		{"commit", "-m", msg},
		{"push", "-u", "origin", branch},
	}
	for _, args := range steps {
		if err := shellGit(ctx, dir, args...); err != nil {
			return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
	}
	return nil
}

func shellGit(ctx context.Context, dir string, args ...string) error {
	full := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ---- helpers ----

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
