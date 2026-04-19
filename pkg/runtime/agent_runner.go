package runtime

import (
	"context"
	"errors"
	"fmt"
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
	postCtx, postCancel := context.WithTimeout(ctx, 30*time.Second)
	err = client.PostMessage(postCtx, sessionID, opencode.PostMessageRequest{
		System: in.Stage.SystemPrompt,
		Parts:  []opencode.MessagePart{{Type: "text", Text: userMsg}},
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

func composeUserMessage(t *tasks.Task) string {
	var sb strings.Builder
	sb.WriteString("Ticket: ")
	sb.WriteString(t.ExternalID)
	if t.Title != "" {
		sb.WriteString("  ")
		sb.WriteString(t.Title)
	}
	sb.WriteString("\n\n")
	if t.URL != "" {
		sb.WriteString("Link: ")
		sb.WriteString(t.URL)
		sb.WriteString("\n\n")
	}
	if t.Description != "" {
		sb.WriteString(t.Description)
	}
	return strings.TrimSpace(sb.String())
}
