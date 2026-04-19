package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"code-agent/pkg/opencode"
	"code-agent/pkg/stages"
	"code-agent/pkg/tasks"
	"code-agent/pkg/transcript"
)

// AgentRunner is a stages.ActionRunner implementing the `run_agent` action
// against any Runtime. It:
//
//  1. asks the Runtime for a worker client and a session
//  2. posts the stage's system prompt + the task description as a user message
//  3. streams events into the transcript store
//  4. blocks until a terminal event arrives or the context times out
//
// The runner is built once per board and looks up the right Runtime from a
// map provided at construction.
type AgentRunner struct {
	runtimes map[string]Runtime // runtime config name -> instance
	tx       transcript.Store

	// streamWG tracks transcript writers so server shutdown can drain them.
	streamWG sync.WaitGroup
}

// NewAgentRunner constructs the action runner.
func NewAgentRunner(runtimes map[string]Runtime, tx transcript.Store) *AgentRunner {
	return &AgentRunner{runtimes: runtimes, tx: tx}
}

// Wait blocks until any in-flight streams finish.
func (r *AgentRunner) Wait() { r.streamWG.Wait() }

func (r *AgentRunner) pickRuntime(boardRuntimeRef, stageOverride string) (Runtime, error) {
	name := boardRuntimeRef
	if stageOverride != "" {
		name = stageOverride
	}
	rt, ok := r.runtimes[name]
	if !ok {
		return nil, fmt.Errorf("runtime %q not registered", name)
	}
	return rt, nil
}

// Run satisfies stages.ActionRunner.
func (r *AgentRunner) Run(ctx context.Context, in stages.ActionInput) (stages.ActionResult, error) {
	rt, err := r.pickRuntime(in.Board.RuntimeRef, in.Stage.RuntimeMode)
	if err != nil {
		return stages.ActionResult{Outcome: "failure"}, err
	}

	client, ref, err := rt.EnsureWorker(ctx, in.Task, in.Board)
	if err != nil {
		return stages.ActionResult{Outcome: "failure"}, fmt.Errorf("ensure worker: %w", err)
	}
	in.Task.WorkerRef = *ref
	in.Task.RuntimeMode = rt.Mode()

	// For local runtime: sync each repo to its configured base_branch
	// before handing off to the agent. We fetch, hard-reset, and check out
	// so every run starts from a clean known baseline. k8s runtimes do
	// their own cloning via cmd/runner, so skip there.
	if rt.Mode() == "local" {
		syncCtx, cancelSync := context.WithTimeout(ctx, 3*time.Minute)
		if err := syncLocalRepos(syncCtx, client, in); err != nil {
			cancelSync()
			return stages.ActionResult{Outcome: "failure"}, fmt.Errorf("sync repos: %w", err)
		}
		cancelSync()
	}

	sessionID, err := rt.EnsureSession(ctx, client, in.Task)
	if err != nil {
		return stages.ActionResult{Outcome: "failure"}, fmt.Errorf("ensure session: %w", err)
	}
	in.Task.SessionID = sessionID

	// start streaming before posting so we don't miss early events
	streamCtx, streamCancel := context.WithCancel(context.Background())
	events, errs, err := client.Stream(streamCtx)
	if err != nil {
		streamCancel()
		return stages.ActionResult{Outcome: "failure"}, fmt.Errorf("stream: %w", err)
	}

	r.streamWG.Add(1)
	terminalCh := make(chan string, 1)
	go r.consumeStream(streamCtx, sessionID, events, errs, terminalCh, in)

	// Compose user message: include the ticket so the model has context.
	userMsg := composeUserMessage(in.Task)
	// Submit via /prompt_async — returns as soon as opencode queues the
	// prompt, completion arrives through SSE. The older /message endpoint
	// held the HTTP connection open for the full model turn (many minutes)
	// which hit our timeouts.
	postCtx, postCancel := context.WithTimeout(ctx, 60*time.Second)
	err = client.PostMessageAsync(postCtx, sessionID, opencode.PostMessageRequest{
		ProviderID: in.Stage.Provider,
		ModelID:    in.Stage.Model,
		Mode:       in.Stage.Mode,
		System:     in.Stage.SystemPrompt,
		Parts:      []opencode.MessagePart{{Type: "text", Text: userMsg}},
	})
	postCancel()
	if err != nil {
		streamCancel()
		return stages.ActionResult{Outcome: "failure"}, fmt.Errorf("post message: %w", err)
	}

	// Wait for the model to finish (terminal event) or for the action ctx
	// to time out.
	select {
	case <-ctx.Done():
		// best-effort abort
		abortCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = client.AbortSession(abortCtx, sessionID)
		cancel()
		streamCancel()
		return stages.ActionResult{Outcome: "failure"}, ctx.Err()
	case outcome := <-terminalCh:
		streamCancel()
		// Look at the last assistant message for the NEEDS_MORE_INFO marker.
		// If the agent emitted it, park the ticket: tag it, post the reason
		// as a comment, and return failure — the boards.yaml exclude_tags
		// filter stops us from re-entering until a human removes the tag.
		fetchCtx, cancelFetch := context.WithTimeout(context.Background(), 15*time.Second)
		lastText, _ := client.LastAssistantText(fetchCtx, sessionID)
		cancelFetch()
		if reason, ok := parseNeedsMoreInfo(lastText); ok {
			in.Logger.Warn().Str("reason", reason).Msg("agent reported needs-more-info; tagging ticket")
			tagCtx, cancelTag := context.WithTimeout(context.Background(), 15*time.Second)
			if err := in.Provider.AddTag(tagCtx, in.Task.ExternalID, "needs-more-info"); err != nil {
				in.Logger.Error().Err(err).Msg("failed to add needs-more-info tag")
			}
			cancelTag()
			return stages.ActionResult{
				Outcome: "failure",
				Comment: "needs-more-info: " + reason,
				Mutate: func(t *tasks.Task) {
					t.SessionID = sessionID
					t.WorkerRef = *ref
					t.RuntimeMode = rt.Mode()
				},
			}, nil
		}
		// If the agent emitted a PLAN_START/PLAN_END block, write it to
		// the ticket description so the next stage (and any human
		// reviewer) has the plan as the source of truth.
		if plan, ok := parsePlan(lastText); ok {
			newDesc := formatPlanDescription(in.Task, plan)
			in.Logger.Info().Int("bytes", len(newDesc)).Msg("updating ticket description with plan")
			descCtx, cancelDesc := context.WithTimeout(context.Background(), 15*time.Second)
			if err := in.Provider.UpdateDescription(descCtx, in.Task.ExternalID, newDesc); err != nil {
				in.Logger.Error().Err(err).Msg("failed to update ticket description")
			}
			cancelDesc()
		}
		return stages.ActionResult{
			Outcome: outcome,
			Mutate: func(t *tasks.Task) {
				t.SessionID = sessionID
				t.WorkerRef = *ref
				t.RuntimeMode = rt.Mode()
			},
		}, nil
	}
}

// parsePlan extracts the markdown between PLAN_START / PLAN_END sentinel
// lines. Returns the inner text and true on success. Both sentinels must
// appear on their own lines.
func parsePlan(text string) (string, bool) {
	startIdx := strings.Index(text, "PLAN_START")
	if startIdx < 0 {
		return "", false
	}
	// advance past the PLAN_START line
	nl := strings.IndexByte(text[startIdx:], '\n')
	if nl < 0 {
		return "", false
	}
	bodyStart := startIdx + nl + 1
	rest := text[bodyStart:]
	endIdx := strings.Index(rest, "PLAN_END")
	if endIdx < 0 {
		return "", false
	}
	return strings.TrimSpace(rest[:endIdx]), true
}

// formatPlanDescription builds the new ticket description: an "Agent plan"
// header, the plan body, and the original description preserved below a
// divider so ticket authors don't lose what they wrote.
func formatPlanDescription(t *tasks.Task, plan string) string {
	var b strings.Builder
	b.WriteString("🤖 **Agent plan** — generated ")
	b.WriteString(time.Now().UTC().Format("2006-01-02 15:04 UTC"))
	b.WriteString(" for ")
	b.WriteString(t.ExternalID)
	b.WriteString("\n\n")
	b.WriteString(plan)
	b.WriteString("\n\n---\n\n")
	b.WriteString("### Original ticket description\n\n")
	if strings.TrimSpace(t.Description) == "" {
		b.WriteString("_(empty)_")
	} else {
		b.WriteString(t.Description)
	}
	return b.String()
}

// parseNeedsMoreInfo returns the reason and true if the assistant's final
// text contains a line beginning with "NEEDS_MORE_INFO:". The marker must
// be on its own line so we don't trip on discussion that mentions the
// string incidentally.
func parseNeedsMoreInfo(text string) (string, bool) {
	for line := range strings.SplitSeq(strings.TrimSpace(text), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "NEEDS_MORE_INFO:"); ok {
			return strings.TrimSpace(rest), true
		}
	}
	return "", false
}

func (r *AgentRunner) consumeStream(ctx context.Context, sessionID string, events <-chan opencode.Event, errs <-chan error, terminalCh chan<- string, in stages.ActionInput) {
	defer r.streamWG.Done()

	logger := in.Logger.With().Str("session", sessionID).Logger()
	logger.Debug().Msg("transcript stream started")

	terminalSent := false
	sendTerminal := func(outcome string) {
		if terminalSent {
			return
		}
		terminalSent = true
		select {
		case terminalCh <- outcome:
		default:
		}
	}

	for {
		select {
		case <-ctx.Done():
			sendTerminal("failure")
			return
		case e, ok := <-events:
			if !ok {
				if !terminalSent {
					sendTerminal("failure")
				}
				return
			}
			if r.tx != nil {
				if e.SessionID == "" || e.SessionID == sessionID {
					if err := r.tx.Append(ctx, sessionID, e.RawJSON); err != nil {
						logger.Debug().Err(err).Msg("transcript append failed")
					}
				}
			}
			if e.IsTerminal() {
				outcome := "success"
				if e.Type == "session.error" {
					outcome = "failure"
				}
				sendTerminal(outcome)
			}
		case err, ok := <-errs:
			if !ok {
				continue
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				logger.Warn().Err(err).Msg("sse error")
				sendTerminal("failure")
			}
		}
	}
}

// syncLocalRepos fetches + hard-resets + checks out each repo on its
// base_branch in the opencode worktree. Destructive: any uncommitted work
// in those trees is wiped — by design, each run starts from a clean base.
// Repos whose directory is missing or not a git checkout are skipped with
// a warning (not an error).
func syncLocalRepos(ctx context.Context, client *opencode.Client, in stages.ActionInput) error {
	project, err := client.CurrentProject(ctx)
	if err != nil {
		return fmt.Errorf("get current project: %w", err)
	}
	root := project.Worktree
	if root == "" || root == "/" {
		return fmt.Errorf("opencode has no workspace worktree (got %q); start it inside a git-initialised parent dir", root)
	}
	for _, r := range in.Task.Repos {
		dir := filepath.Join(root, r.Name)
		if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
			in.Logger.Warn().Str("repo", r.Name).Str("dir", dir).Msg("repo dir not a git checkout; skipping sync")
			continue
		}
		branch := r.BaseBranch
		if branch == "" {
			branch = "main"
		}
		steps := [][]string{
			{"fetch", "--quiet", "origin", branch},
			{"checkout", "-B", branch, "origin/" + branch},
			{"reset", "--hard", "origin/" + branch},
		}
		for _, args := range steps {
			full := append([]string{"-C", dir}, args...)
			cmd := exec.CommandContext(ctx, "git", full...)
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("repo %s: git %s: %w (%s)", r.Name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
			}
		}
		in.Logger.Info().Str("repo", r.Name).Str("branch", branch).Msg("repo synced to base_branch")
	}
	return nil
}

func composeUserMessage(t *tasks.Task) string {
	// Deliberately omit t.URL — private ticket URLs trigger the model's
	// WebSearch tool and waste calls on pages it can't reach. The agent
	// should plan from the text body + codebase alone.
	var sb strings.Builder
	sb.WriteString("Ticket: ")
	sb.WriteString(t.ExternalID)
	if t.Title != "" {
		sb.WriteString("  ")
		sb.WriteString(t.Title)
	}
	sb.WriteString("\n\n")
	if t.Description != "" {
		sb.WriteString(t.Description)
	}
	return strings.TrimSpace(sb.String())
}
