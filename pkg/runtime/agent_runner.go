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

	"code-agent/internal/config"
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
	skills   *config.SkillsIndex // host-side skill bundle index (may be nil)

	// streamWG tracks transcript writers so server shutdown can drain them.
	streamWG sync.WaitGroup
}

// NewAgentRunner constructs the action runner. skills may be nil when
// the orchestrator has no CODE_AGENT_SKILLS_DIR configured.
func NewAgentRunner(runtimes map[string]Runtime, tx transcript.Store, skills *config.SkillsIndex) *AgentRunner {
	return &AgentRunner{runtimes: runtimes, tx: tx, skills: skills}
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

	// Ship skill bundles (union of board.skills + stage.skills) to the
	// worker before the first prompt of this stage. Non-fatal on error —
	// the agent can still run, just without the skill hints.
	if err := r.syncSkills(ctx, in, ref); err != nil {
		in.Logger.Warn().Err(err).Msg("skill sync failed; continuing without skills")
	}

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

	// Start SSE streaming for the transcript in the background. We no
	// longer rely on SSE events for completion detection — the sync
	// PostMessage below blocks until the full turn (including all tool
	// calls and subagent work) is done. SSE is just for observability.
	streamCtx, streamCancel := context.WithCancel(context.Background())
	events, errs, err := client.Stream(streamCtx)
	if err != nil {
		streamCancel()
		return stages.ActionResult{Outcome: "failure"}, fmt.Errorf("stream: %w", err)
	}
	r.streamWG.Add(1)
	go r.consumeStream(streamCtx, sessionID, events, errs, nil, in)

	// Compose user message and submit synchronously. PostMessage blocks
	// until opencode is finished with this prompt. Bounded by the action
	// ctx (stage.Timeout, ~45m). Abort on ctx cancellation.
	userMsg := composeUserMessage(in.Task)
	err = client.PostMessage(ctx, sessionID, opencode.PostMessageRequest{
		ProviderID: in.Stage.Provider,
		ModelID:    in.Stage.Model,
		Agent:      in.Stage.Agent,
		System:     in.Stage.SystemPrompt,
		Parts:      []opencode.MessagePart{{Type: "text", Text: userMsg}},
	})
	streamCancel()
	if err != nil {
		if ctx.Err() != nil {
			abortCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = client.AbortSession(abortCtx, sessionID)
			cancel()
			return stages.ActionResult{Outcome: "failure"}, ctx.Err()
		}
		return stages.ActionResult{Outcome: "failure"}, fmt.Errorf("post message: %w", err)
	}

	// PostMessage returned cleanly → the turn is fully done. Inspect the
	// last assistant message for sentinel markers (NEEDS_MORE_INFO /
	// PLAN_START..PLAN_END).
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
	if in.Stage.WritesPlan && strings.TrimSpace(lastText) != "" {
		newDesc := formatPlanDescription(in.Task, lastText)
		in.Logger.Info().Int("bytes", len(newDesc)).Msg("updating ticket description with plan")
		descCtx, cancelDesc := context.WithTimeout(context.Background(), 15*time.Second)
		if err := in.Provider.UpdateDescription(descCtx, in.Task.ExternalID, newDesc); err != nil {
			in.Logger.Error().Err(err).Msg("failed to update ticket description")
		}
		cancelDesc()
	}
	return stages.ActionResult{
		Outcome: "success",
		Mutate: func(t *tasks.Task) {
			t.SessionID = sessionID
			t.WorkerRef = *ref
			t.RuntimeMode = rt.Mode()
		},
	}, nil
}

// syncSkills resolves the union of board-level and stage-level skill
// names against the host skills index and uploads matching bundles to
// the worker's admin HTTP. No-op when no skills are requested or when
// the runtime has no admin URL (e.g. local mode — dev manages their own
// ~/.config/opencode/skills).
func (r *AgentRunner) syncSkills(ctx context.Context, in stages.ActionInput, ref *tasks.WorkerRef) error {
	names := unionSkills(in.Board.Skills, in.Stage.Skills)
	if len(names) == 0 {
		return nil
	}
	dirs, err := r.skills.Resolve(names)
	if err != nil {
		return err
	}
	if ref.AdminURL == "" {
		in.Logger.Debug().Strs("skills", names).Msg("no admin URL; skipping skill upload (local mode)")
		return nil
	}
	uploadCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := UploadSkills(uploadCtx, ref.AdminURL, dirs); err != nil {
		return err
	}
	in.Logger.Info().Strs("skills", names).Int("bundles", len(dirs)).Msg("skills synced to worker")
	return nil
}

func unionSkills(a, b []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, list := range [][]string{a, b} {
		for _, s := range list {
			if _, ok := seen[s]; ok {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
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

// consumeStream drains SSE events into the transcript store. No longer
// used for completion detection — sync PostMessage blocks until the turn
// is actually done. terminalCh is accepted but may be nil.
func (r *AgentRunner) consumeStream(ctx context.Context, sessionID string, events <-chan opencode.Event, errs <-chan error, terminalCh chan<- string, in stages.ActionInput) {
	defer r.streamWG.Done()

	logger := in.Logger.With().Str("session", sessionID).Logger()
	logger.Debug().Msg("transcript stream started")

	terminalSent := false
	sendTerminal := func(outcome string) {
		if terminalSent || terminalCh == nil {
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
			// wipe untracked files/dirs left over from any prior run so
			// each stage starts from a truly clean worktree. -f = force,
			// -d = include directories, -x = include ignored files too.
			{"clean", "-fdx"},
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
