// Package review holds the action runner that handles PR review comments.
// Invoked by pkg/review.Poller (one call per new comment), it:
//
//  1. Classifies the comment via the LLM (question / change_request /
//     approval / noise).
//  2. Approval/noise: no-op (poller has already advanced the cursor).
//  3. Question: creates an opencode session on the feature branch in the
//     existing persistent pod and runs the agent in read-only (explore)
//     mode. Posts the assistant's text as a PR reply.
//  4. Change-request: same as (3) but in build mode. After the agent turn,
//     any new commits on the feature branch are pushed by cmd/runner's
//     admin HTTP; a summary comment goes on the PR.
package review

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"code-agent/internal/config"
	"code-agent/pkg/actions"
	"code-agent/pkg/llm"
	"code-agent/pkg/opencode"
	oreview "code-agent/pkg/review"
	"code-agent/pkg/runtime"
	"code-agent/pkg/tasks"
	"code-agent/pkg/vcs"
)

// Handler satisfies oreview.Handler.
type Handler struct {
	Runtimes map[string]runtime.Runtime // runtime name -> instance
	LLM      *llm.Client                // may be nil; no-op if so
	Logger   zerolog.Logger
}

// Handle is the entry point invoked by the poller. Returning nil always
// advances the cursor past this comment (even when we chose to ignore it),
// so poll noise never retries.
func (h *Handler) Handle(ctx context.Context, e oreview.Event) error {
	log := h.Logger.With().
		Str("task", e.Task.ID).
		Str("repo", e.MR.Repo).
		Str("comment_id", e.Comment.ID).
		Str("author", e.Comment.Author).
		Logger()

	if strings.TrimSpace(e.Comment.Body) == "" {
		return nil
	}
	if h.LLM == nil {
		log.Warn().Msg("LLM unavailable; cannot classify PR comment")
		return nil
	}

	classifyCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	cls, err := h.LLM.ClassifyReviewComment(classifyCtx, e.Task.Title, e.Comment.Author, e.Comment.Body)
	cancel()
	if err != nil {
		log.Warn().Err(err).Msg("classify failed; treating as question")
	}
	log.Info().
		Str("kind", string(cls.Kind)).
		Float64("confidence", cls.Confidence).
		Str("reason", cls.Reason).
		Msg("comment classified")

	switch cls.Kind {
	case llm.ReviewKindApproval, llm.ReviewKindNoise:
		return nil
	case llm.ReviewKindQuestion:
		return h.runQuestion(ctx, log, e)
	case llm.ReviewKindChangeRequest:
		return h.runChangeRequest(ctx, log, e)
	}
	return nil
}

func (h *Handler) runQuestion(ctx context.Context, log zerolog.Logger, e oreview.Event) error {
	text, err := h.runAgent(ctx, log, e, "explore", questionPrompt(e))
	if err != nil {
		return err
	}
	return h.replyOnPR(ctx, log, e, text)
}

func (h *Handler) runChangeRequest(ctx context.Context, log zerolog.Logger, e oreview.Event) error {
	text, err := h.runAgent(ctx, log, e, "build", changeRequestPrompt(e))
	if err != nil {
		return err
	}
	// Have cmd/runner push any new commits on the feature branch. We
	// reuse the open_mr driver abstraction: pod admin HTTP commit-push
	// on the existing branch (no new PR — pushing updates the open one).
	if err := h.pushFeatureBranch(ctx, log, e); err != nil {
		// Not fatal for the reply — the agent may have only explained
		// something rather than editing. Log and continue.
		log.Warn().Err(err).Msg("push on feature branch failed")
	}
	return h.replyOnPR(ctx, log, e, text)
}

func (h *Handler) runAgent(ctx context.Context, log zerolog.Logger, e oreview.Event, agent, systemPrompt string) (string, error) {
	rtName := e.Board.RuntimeRef
	rt, ok := h.Runtimes[rtName]
	if !ok {
		return "", fmt.Errorf("runtime %q not registered", rtName)
	}
	client, _, err := rt.EnsureWorker(ctx, e.Task, e.Board)
	if err != nil {
		return "", fmt.Errorf("ensure worker: %w", err)
	}

	// Scope every opencode call to the specific repo's directory via
	// WithDirectory — opencode's InstanceMiddleware binds this request's
	// project/worktree to /workspace/<repo>, so the session, agent turn,
	// diff view, and review UI all see exactly this repo.
	scoped := client.WithDirectory("/workspace/" + e.MR.Repo)
	session, err := scoped.CreateSession(ctx, fmt.Sprintf("review %s %s", e.Task.ExternalID, e.MR.Repo))
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}

	userMsg := composeReviewUserMessage(e)
	submitStart := time.Now().UnixMilli()
	if err := scoped.PostMessageAsync(ctx, session.ID, opencode.PostMessageRequest{
		Agent:  agent,
		System: systemPrompt,
		Parts:  []opencode.MessagePart{{Type: "text", Text: userMsg}},
	}); err != nil {
		return "", fmt.Errorf("post prompt: %w", err)
	}

	turnCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	turn, err := scoped.WaitForTurn(turnCtx, session.ID, submitStart)
	if err != nil {
		return "", fmt.Errorf("wait turn: %w", err)
	}
	if turn.Error != nil {
		return "", fmt.Errorf("agent error: %s", turn.Error.Data.Message)
	}
	if strings.TrimSpace(turn.Text) == "" {
		return "(no response)", nil
	}
	return turn.Text, nil
}

func (h *Handler) pushFeatureBranch(ctx context.Context, log zerolog.Logger, e oreview.Event) error {
	adminURL := e.Task.WorkerRef.AdminURL
	if adminURL == "" {
		return fmt.Errorf("no admin URL on task")
	}
	branch := e.MR.SourceBranch
	if branch == "" {
		branch = "ai/" + e.Task.ExternalID
	}
	author := e.Comment.Author
	if author == "" {
		author = "reviewer"
	}
	commit := fmt.Sprintf("Address review feedback from @%s on %s", author, e.Task.ExternalID)
	driver := actions.NewPodDriver(adminURL, log)
	dirty, diff, err := driver.GetStatusAndDiff(ctx, e.MR.Repo)
	if err != nil {
		return err
	}
	if !dirty || strings.TrimSpace(diff) == "" {
		log.Info().Msg("no new changes to commit")
		return nil
	}
	// Empty base_branch — we don't want `checkout -B branch BASE` to wipe
	// the feature branch's existing commits; just update in place.
	return driver.CommitAndPush(ctx, e.MR.Repo, branch, "", commit)
}

func (h *Handler) replyOnPR(ctx context.Context, log zerolog.Logger, e oreview.Event, body string) error {
	br, ok := findBoardRepo(e.Board, e.MR.Repo)
	if !ok {
		return fmt.Errorf("repo %q not in board config", e.MR.Repo)
	}
	client, err := vcs.Build(br)
	if err != nil {
		return err
	}
	ref := vcs.MergeRequest{Repo: e.MR.Repo, IID: e.MR.IID, Number: e.MR.Number, URL: e.MR.URL}
	replyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return client.ReplyToComment(replyCtx, ref, e.Comment, body)
}

// ---- prompts ----

func questionPrompt(e oreview.Event) string {
	return `You are responding to a question on PR #` + mrNumber(e.MR) + ` for repo ` + e.MR.Repo + `.

The feature branch is checked out at /workspace/` + e.MR.Repo + `. The reviewer's comment is in the next message.

Rules:
- DO NOT modify any files. Read, grep, search — never edit.
- Answer concisely and factually based on the code in the feature branch.
- If the question is ambiguous, explain both interpretations briefly.
- If you cannot determine the answer from the code, say so.
- End with a single-line marker: "ACTION: explained".
`
}

func changeRequestPrompt(e oreview.Event) string {
	return `You are addressing review feedback on PR #` + mrNumber(e.MR) + ` for repo ` + e.MR.Repo + `.

The feature branch is checked out at /workspace/` + e.MR.Repo + `. The reviewer's comment is in the next message.

Decide:
1. Does this comment REQUIRE a code change, or is it a question/preference the
   reviewer is open to discussion on? If it's a question, just answer without
   changing code.
2. If a code change is needed, make the minimal change, run lightweight checks
   (type-check / test) if relevant, and summarise in 2-3 sentences what you
   changed and why.

End with exactly one line:
  ACTION: explained     — if you only answered, no file edits
  ACTION: changed       — if you edited files
Do NOT run git commands. The orchestrator handles commit/push from your edits.
`
}

func composeReviewUserMessage(e oreview.Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "PR: %s\nReviewer (@%s) wrote:\n", e.MR.URL, e.Comment.Author)
	b.WriteString(e.Comment.Body)
	return b.String()
}

func mrNumber(mr tasks.MergeRef) string {
	if mr.Number > 0 {
		return fmt.Sprintf("%d", mr.Number)
	}
	if mr.IID > 0 {
		return fmt.Sprintf("%d", mr.IID)
	}
	return "?"
}

func findBoardRepo(b config.Board, name string) (config.BoardRepo, bool) {
	for _, r := range b.Repos {
		if r.Name == name {
			return r, true
		}
	}
	return config.BoardRepo{}, false
}
