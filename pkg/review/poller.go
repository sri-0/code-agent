// Package review polls open MRs/PRs for new review comments and turns them
// into stage events that drive the review_feedback action.
//
// Lifecycle:
//  1. On each tick, iterate all tasks with MergeRequests in state=open.
//  2. For each MR, build a vcs.Client, fetch comments since
//     mr.LastCommentCursor, filter out the bot's own.
//  3. For each new comment, call the handler (which runs the
//     review_feedback action synchronously).
//  4. Advance mr.LastCommentCursor past processed comments.
//
// Cursor persistence is via tasks.Store (MergeRef.LastCommentCursor).
package review

import (
	"context"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"code-agent/internal/config"
	"code-agent/pkg/tasks"
	"code-agent/pkg/vcs"
)

// Event is emitted by the poller per new comment.
type Event struct {
	Task    *tasks.Task
	MR      tasks.MergeRef
	Board   config.Board
	Comment vcs.ReviewComment
}

// BotReplyMarker is embedded (as an HTML comment, invisible in rendered
// PR/MR views) in every comment the orchestrator posts to a PR. Both the
// poller and the open_mr action use this to identify their own replies
// when skipping self-authored comments — author-based detection is
// unreliable when the bot uses a human's PAT (its login is the human's
// login, indistinguishable from the human's own comments).
const BotReplyMarker = "<!-- code-agent:reply -->"

// Handler is called synchronously per event. Returning an error does NOT
// advance the cursor (the poller will retry on next tick); return nil for
// processed-but-failed events you don't want re-attempted.
type Handler func(ctx context.Context, e Event) error

// Poller wakes periodically and checks every task's open MRs.
type Poller struct {
	tasks    tasks.Store
	boards   *config.BoardsConfig
	interval time.Duration
	logger   zerolog.Logger
	handler  Handler
}

func New(store tasks.Store, boards *config.BoardsConfig, interval time.Duration, logger zerolog.Logger, handler Handler) *Poller {
	if interval <= 0 {
		interval = 2 * time.Minute
	}
	return &Poller{
		tasks:    store,
		boards:   boards,
		interval: interval,
		logger:   logger.With().Str("component", "review_poller").Logger(),
		handler:  handler,
	}
}

// Start runs until ctx is cancelled.
func (p *Poller) Start(ctx context.Context) {
	if p.handler == nil {
		p.logger.Warn().Msg("no handler; poller disabled")
		return
	}
	p.logger.Info().Dur("interval", p.interval).Msg("review poller started")
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.tick(ctx)
		}
	}
}

func (p *Poller) tick(ctx context.Context) {
	if p.tasks == nil || p.boards == nil {
		return
	}
	for _, b := range p.boards.Boards {
		tasks, err := p.tasks.ListByBoard(ctx, b.ID)
		if err != nil {
			p.logger.Warn().Err(err).Str("board", b.ID).Msg("list tasks failed")
			continue
		}
		for _, t := range tasks {
			if len(t.MergeRequests) == 0 {
				continue
			}
			p.processTask(ctx, b, t)
		}
	}
}

func (p *Poller) processTask(ctx context.Context, board config.Board, t *tasks.Task) {
	changed := false
	for i, mr := range t.MergeRequests {
		if mr.State != "open" && mr.State != "" {
			continue
		}
		br, ok := findBoardRepo(board, mr.Repo)
		if !ok {
			continue
		}
		client, err := vcs.Build(br)
		if err != nil {
			p.logger.Debug().Err(err).Str("repo", mr.Repo).Msg("vcs.Build failed")
			continue
		}
		ref := vcs.MergeRequest{Repo: mr.Repo, IID: mr.IID, Number: mr.Number, URL: mr.URL}

		listCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		comments, err := client.ListComments(listCtx, ref, mr.LastCommentCursor)
		cancel()
		if err != nil {
			p.logger.Debug().Err(err).Str("repo", mr.Repo).Msg("list comments failed")
			continue
		}
		if len(comments) == 0 {
			continue
		}
		latest := mr.LastCommentCursor
		for _, c := range comments {
			if !c.CreatedAt.After(mr.LastCommentCursor) {
				continue
			}
			// Skip comments we wrote ourselves — identified by an embedded
			// HTML-comment marker, NOT by author login (author-based
			// skipping breaks when the bot uses a human's PAT, since its
			// login matches the human's own comments).
			if strings.Contains(c.Body, BotReplyMarker) {
				if c.CreatedAt.After(latest) {
					latest = c.CreatedAt
				}
				continue
			}
			evt := Event{
				Task:    t,
				MR:      mr,
				Board:   board,
				Comment: c,
			}
			handleCtx, cancelH := context.WithTimeout(ctx, 10*time.Minute)
			err := p.handler(handleCtx, evt)
			cancelH()
			if err != nil {
				p.logger.Error().
					Err(err).
					Str("task", t.ID).
					Str("repo", mr.Repo).
					Str("comment_id", c.ID).
					Msg("review handler failed; will retry next tick")
				// Don't advance cursor past this comment — we retry it.
				break
			}
			if c.CreatedAt.After(latest) {
				latest = c.CreatedAt
			}
		}
		if latest.After(mr.LastCommentCursor) {
			t.MergeRequests[i].LastCommentCursor = latest
			changed = true
		}
	}
	if changed {
		if err := p.tasks.Update(ctx, t); err != nil {
			p.logger.Warn().Err(err).Str("task", t.ID).Msg("persist cursor failed")
		}
	}
}

func findBoardRepo(b config.Board, name string) (config.BoardRepo, bool) {
	for _, r := range b.Repos {
		if r.Name == name {
			return r, true
		}
	}
	return config.BoardRepo{}, false
}
