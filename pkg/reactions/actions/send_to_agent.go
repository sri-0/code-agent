package actions

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	"code-agent/pkg/reactions"
	"code-agent/pkg/tasks"
)

// TurnRunner is the contract reaction → agent invocations need. It's a
// thin interface so reactions doesn't take a runtime dependency. The
// orchestrator wires an implementation backed by the existing review
// handler (same code path: ensure-worker → opencode session → prompt
// async → wait turn → optional commit-push).
type TurnRunner interface {
	RunTurn(ctx context.Context, in TurnInput) error
}

// TurnInput is everything the runner needs to fire one prompt at the
// agent in the context of a task.
type TurnInput struct {
	Task         *tasks.Task
	BoardID      string
	Mode         string // "explore" | "build" — read-only vs edit allowed
	SystemPrompt string
	UserMessage  string
	// PushOnEdit, when true, asks the runner to commit-push any new
	// commits on the feature branch after the turn (used for ci-failed
	// + changes-requested fixes that produce edits).
	PushOnEdit bool
}

// SendToAgent reacts by composing a prompt and running it through the
// agent. The actual opencode plumbing is delegated to TurnRunner so
// this package stays runtime-agnostic.
type SendToAgent struct {
	Runner TurnRunner
	Logger zerolog.Logger
}

func (s *SendToAgent) Name() string { return "send-to-agent" }

func (s *SendToAgent) Run(ctx context.Context, e reactions.Event) error {
	if s.Runner == nil {
		return fmt.Errorf("send-to-agent: no runner configured")
	}
	in := composePrompt(e)
	return s.Runner.RunTurn(ctx, in)
}

// composePrompt builds a structured user message + system prompt for
// each reaction kind. Mirrors the review handler's prompt shape so the
// agent's behaviour is consistent across both entry points.
func composePrompt(e reactions.Event) TurnInput {
	in := TurnInput{Task: e.Task, BoardID: e.Task.BoardID, Mode: "build", PushOnEdit: true}
	switch e.Key {
	case reactions.EventCIFailed:
		in.SystemPrompt = ciFailedSystemPrompt
		in.UserMessage = fmt.Sprintf(
			"CI is failing on PR %s for task %s.\n\nFailing runs:\n%s\n\n"+
				"Inspect the failures, then fix the underlying issues and commit.",
			e.Next.PR.URL, e.Task.ExternalID, formatFailingRuns(e),
		)
	case reactions.EventChangesRequested:
		in.SystemPrompt = changesRequestedSystemPrompt
		in.UserMessage = fmt.Sprintf(
			"A reviewer requested changes on PR %s for task %s.\n\n"+
				"Read the latest review comments via your VCS tools and address each "+
				"actionable point. If a comment is a question rather than a request, "+
				"answer it without editing.",
			e.Next.PR.URL, e.Task.ExternalID,
		)
	default:
		in.Mode = "explore"
		in.SystemPrompt = "You are a code review assistant."
		in.UserMessage = fmt.Sprintf("Reaction %s fired for task %s.", e.Key, e.Task.ExternalID)
	}
	return in
}

func formatFailingRuns(e reactions.Event) string {
	// The reactions engine doesn't carry CI run details directly — the
	// lifecycle poller has them but only writes the rolled-up reason.
	// For the prompt, point the agent at the PR check tab. Adding the
	// failing run names + URLs requires plumbing CISummary through the
	// Event payload; deferred to a follow-up.
	return "(see PR check runs)"
}

const ciFailedSystemPrompt = `You are addressing a CI failure on an open PR.

The feature branch is checked out at /workspace/<repo>. Use the agent's
shell tools to:
  1. Read the failing CI run logs (gh CLI, GitLab CI tools, or PR url tab).
  2. Identify the root cause.
  3. Make the minimal fix and run the failing tests/linters locally.
  4. Commit. The orchestrator handles push.

End with exactly one line:
  ACTION: changed   — if you edited files
  ACTION: explained — if you only investigated and the failure is environmental
`

const changesRequestedSystemPrompt = `You are addressing reviewer change-requests on an open PR.

The feature branch is checked out at /workspace/<repo>. Use the agent's
shell tools to:
  1. List unresolved review comments via your VCS tools.
  2. Decide for each: actionable change vs question.
  3. Make the minimal edits for actionable items, run lightweight checks.
  4. Commit. The orchestrator handles push.

End with exactly one line:
  ACTION: changed   — if you edited files
  ACTION: explained — if everything was Q&A only
`
