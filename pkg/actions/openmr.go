// Package actions hosts stages.ActionRunner implementations that don't fit
// neatly in pkg/runtime (which is for runtime-adjacent actions like
// run_agent). Each action is standalone and registered in the orchestrator.
//
// Current: OpenMRRunner — iterates the task's repos, verifies the remote
// branch exists, opens one MR/PR per repo via the appropriate vcs.Client,
// then posts a cross-link comment on each side if more than one MR was
// created.
package actions

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/rs/zerolog"

	"code-agent/internal/config"
	"code-agent/pkg/git"
	"code-agent/pkg/stages"
	"code-agent/pkg/tasks"
	"code-agent/pkg/vcs"
)

// OpenMRRunner opens merge/pull requests for a task.
//
// VCS clients are cached by repo name per-runner (keyed on board+repo
// name; collisions across boards are fine since repo configs are the same).
type OpenMRRunner struct {
	mu      sync.Mutex
	clients map[string]vcs.Client // repo_name -> client
}

func NewOpenMRRunner() *OpenMRRunner {
	return &OpenMRRunner{clients: map[string]vcs.Client{}}
}

// Run satisfies stages.ActionRunner.
func (r *OpenMRRunner) Run(ctx context.Context, in stages.ActionInput) (stages.ActionResult, error) {
	logger := in.Logger.With().Str("action", "open_mr").Logger()

	if len(in.Task.Repos) == 0 {
		return stages.ActionResult{Outcome: "success", Comment: "open_mr: no repos configured"}, nil
	}

	tmpl, err := parseMRTemplate(in.Stage.MRTemplate)
	if err != nil {
		return stages.ActionResult{Outcome: "failure"}, fmt.Errorf("template: %w", err)
	}

	var opened []vcs.MergeRequest
	var newRefs []tasks.MergeRef

	for i, rp := range in.Task.Repos {
		if alreadyOpen(in.Task.MergeRequests, rp.Name) {
			logger.Info().Str("repo", rp.Name).Msg("MR already exists; skipping")
			continue
		}
		boardRepo, ok := findBoardRepo(in.Board, rp.Name)
		if !ok {
			logger.Warn().Str("repo", rp.Name).Msg("repo not in board config; skipping")
			continue
		}
		vcli, err := r.clientFor(boardRepo)
		if err != nil {
			return stages.ActionResult{Outcome: "failure"}, fmt.Errorf("vcs client for %s: %w", rp.Name, err)
		}

		// verify branch exists on origin before trying to open
		tok := os.Getenv(boardRepo.VCS.Auth.Env)
		exists, err := git.RemoteBranchExists(ctx, rp.URL, rp.Branch, tok)
		if err != nil {
			return stages.ActionResult{Outcome: "failure"}, fmt.Errorf("ls-remote %s: %w", rp.Name, err)
		}
		if !exists {
			msg := fmt.Sprintf("branch %s not found on %s — agent did not push", rp.Branch, rp.Name)
			logger.Warn().Msg(msg)
			return stages.ActionResult{Outcome: "failure", Comment: msg}, nil
		}

		title, body, err := renderMRText(tmpl, in, rp, i)
		if err != nil {
			return stages.ActionResult{Outcome: "failure"}, err
		}

		mr, err := vcli.OpenMR(ctx, vcs.OpenMRRequest{
			RepoName:     rp.Name,
			SourceBranch: rp.Branch,
			TargetBranch: rp.BaseBranch,
			Title:        title,
			Description:  body,
		})
		if err != nil {
			return stages.ActionResult{Outcome: "failure"}, fmt.Errorf("open mr %s: %w", rp.Name, err)
		}
		logger.Info().Str("repo", rp.Name).Str("url", mr.URL).Msg("MR opened")
		opened = append(opened, mr)
		newRefs = append(newRefs, tasks.MergeRef{
			Repo:   rp.Name,
			URL:    mr.URL,
			IID:    mr.IID,
			Number: mr.Number,
			State:  "open",
		})
	}

	if len(opened) == 0 {
		return stages.ActionResult{Outcome: "success", Comment: "open_mr: nothing to open"}, nil
	}

	// cross-link if more than one
	if len(opened) > 1 {
		r.crosslink(ctx, logger, opened)
	}

	// ticket comment summarising
	var comment bytes.Buffer
	comment.WriteString("Opened merge/pull requests:\n")
	for _, mr := range opened {
		comment.WriteString("- ")
		comment.WriteString(mr.Repo)
		comment.WriteString(": ")
		comment.WriteString(mr.URL)
		comment.WriteString("\n")
	}

	return stages.ActionResult{
		Outcome: "success",
		Comment: comment.String(),
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
		// look up the client by repo name
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

// parseMRTemplate handles an optional Go text/template for the MR body.
// "" => a sensible default.
func parseMRTemplate(raw string) (*template.Template, error) {
	if strings.TrimSpace(raw) == "" {
		raw = defaultMRTemplate
	}
	return template.New("mr").Parse(raw)
}

const defaultMRTemplate = `{{.Task.Title}}

Ticket: {{.Task.ExternalID}}{{if .Task.URL}} ({{.Task.URL}}){{end}}
Repo: {{.Repo.Name}} — branch {{.Repo.Branch}} → {{.Repo.BaseBranch}}

{{if .Task.Description}}{{.Task.Description}}{{end}}
`

type mrTmplData struct {
	Task *tasks.Task
	Repo tasks.RepoState
}

func renderMRText(tmpl *template.Template, in stages.ActionInput, rp tasks.RepoState, _ int) (title, body string, err error) {
	title = in.Task.Title
	if title == "" {
		title = in.Task.ExternalID
	}
	if in.Task.ExternalID != "" && !strings.Contains(title, in.Task.ExternalID) {
		title = in.Task.ExternalID + " " + title
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, mrTmplData{Task: in.Task, Repo: rp}); err != nil {
		return "", "", fmt.Errorf("exec mr template: %w", err)
	}
	return title, buf.String(), nil
}

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
