# Build history

A condensed log of how `code-agent` evolved from empty repo to working
end-to-end pipeline. Useful for understanding *why* certain choices
exist.

## Phase 1 — scaffolding

Config loader (envconfig + YAML), valkey-backed task + transcript
stores, gorilla HTTP server with health/metrics, dashboard templates.

## Phase 2 — board providers

`pkg/providers` interface, ClickUp + Jira clients, dispatcher with
dedupe + polling fan-in, webhook handlers.

## Phase 3 — stage engine

`pkg/stages.Engine` consumes provider events, maps `provider_status →
stage`, dispatches the configured `action`. ActionRunner registry with
`Run(ctx, ActionInput)` returning `ActionResult{Outcome, Comment, Mutate}`.
Comment is posted to the ticket; Mutate persists state.

## Phase 4 — local runtime + run_agent

`pkg/runtime` interface. `local` runtime talks to a `opencode serve`
running on the host. `AgentRunner` posts a prompt, blocks on
completion, parses sentinels (`NEEDS_MORE_INFO`), optionally writes
final assistant text to the ticket description.

## Phase 5 — k8s persistent runtime

`pkg/k8s` (client-go wrapper). `pkg/runtime/persistent` creates a
Deployment + Service per task, waits for pod ready, port-forwards
opencode + admin via SPDY when out-of-cluster. `cmd/runner` is the
PID-1 inside each pod: clones repos, writes opencode config, exposes
admin HTTP for git ops + session setup. Worker image is `node:22-slim`
(opencode's prebuilt binary requires glibc).

## Phase 6 — VCS abstraction + open_mr

`pkg/vcs` interface; `github` (go-github/v67) + `gitlab` (go-gitlab)
clients. `pkg/actions/openmr.go` — `gitDriver` interface with two impls:
`podDriver` (admin HTTP) for in-cluster work, `hostDriver` (shell git)
for local runtime. Picks based on whether `task.WorkerRef.AdminURL` is
set. Records resulting MRs on the task.

## Phase 7 — LLM-summarised commits + PR bodies

`pkg/llm.SummarizeChange` calls an OpenAI-compatible chat endpoint
(default OpenRouter, claude-haiku-4.5) to produce a structured
`{commit_message, pr_title, pr_body}` from a diff. Falls back to a
template if no API key.

## Phase 8 — async + resilience refit

The biggest refactor. `POST /session/{id}/message` had been used as
sync, but in current opencode (1.14.x) it's fire-and-forget — turns
returned in 1s while the model was still running, leading to silent
failures (push with no commits, "no changes to PR"). Switched to
`POST /session/{id}/prompt_async` + polling
`GET /session/{id}/message` for the tail assistant message's
`time.completed`. Provider errors now surface from
`message.error.data.message` (credit limits, rate limits, auth, 5xx).
Empty turns (no text + no tool calls) flagged as failures.

Every failure path now posts a diagnostic comment to the originating
ticket so operators see *why*.

## Phase 9 — skills + LSP toolchains

Worker image gains Go 1.23 (arch-aware via `dpkg --print-architecture`)
+ python3 + pip. Opencode autodetects gopls / pyright / typescript-LSP
once toolchains are present.

`pkg/runtime.UploadSkills` packs Anthropic-style skill bundles
(`<name>/SKILL.md` + files) from `CODE_AGENT_SKILLS_DIR` into a gzip
tar and POSTs to `<admin_url>/skills`. cmd/runner extracts into
`/home/worker/.config/opencode/skills/` (opencode's documented global
skill dir). `boards.yaml` and `stages.yaml` reference bundles by name;
the union is uploaded before each `run_agent` turn.

Opencode config also moved out of `/workspace` (now at
`/home/worker/.config/opencode/opencode.json`) so the agent's tools
don't see it. Old `folders` stanza dropped (deprecated in current
opencode → 500s on startup).

## Phase 10 — branch resume

cmd/runner, after cloning each repo, checks `git ls-remote --heads
origin ai/<ticket>` and if the branch exists, fetches + checks it out
so prior agent work is on disk as HEAD. Lets a re-run continue from
where the previous run stopped instead of duplicating work.

cmd/runner's commit-push handler grew the `branchExistsLocal` check —
when the feature branch is already checked out (resumed), use plain
`git checkout <branch>` instead of `-B <branch> <base>`, which would
reset the branch to base and produce a non-fast-forward push.

## Phase 11 — pod TTL + GC

`runtime.GCCapable` interface. Persistent runtime spins a background
goroutine every `CODE_AGENT_POD_GC_INTERVAL`:
- Refresh each MR's state via the VCS client.
- Reap pods past `CODE_AGENT_POD_TTL_MAX` (catches forgotten pods).
- Reap pods where all MRs are closed/merged + grace period
  (`CODE_AGENT_POD_TTL_AFTER_MR_CLOSED`) has elapsed.

## Phase 12 — multi-project seeding

`POST /project/{id}` doesn't exist on the opencode server, but
`InstanceMiddleware` auto-registers a project on first request that
carries `?directory=<path>` (or `x-opencode-directory` header) when the
path is a git worktree. cmd/runner pings `/project/current?directory=`
once per cloned repo at startup; the opencode UI's sidebar then shows
each repo as a distinct project with its own session list, diff/review
tab, and file tree.

`pkg/opencode.Client.WithDirectory(dir)` returns a shallow clone that
carries the header on every call. The dashboard surfaces deep-links
(`<URL>/?directory=/workspace/<repo>`) per repo on `/tasks/{id}`.

## Phase 13 — PR review feedback loop

`pkg/review.Poller` ticks every `CODE_AGENT_POD_GC_INTERVAL`, lists
comments on each open MR via `vcs.Client.ListComments(since)`, dedupes
the bot's own replies via the embedded
`<!-- code-agent:reply -->` marker (HTML comment, invisible in PR
view — needed because the PAT typically belongs to a human user, so
author-based dedup loops on real comments).

`pkg/actions/review.Handler` classifies each comment via
`llm.ClassifyReviewComment` (claude-haiku) into
`{question, change_request, approval, noise}` with confidence:
- `approval` / `noise` → cursor advance, no-op.
- `question` → opencode session scoped at `/workspace/<repo>` running
  the `explore` agent (no edits allowed). Reply posted to the PR.
- `change_request` (confidence ≥ 0.7) → `build` agent. After the turn,
  `cmd/runner /repos/<name>/commit-push` extends the existing feature
  branch (no `-B` reset). Reply posted summarising what changed.

`vcs.Client` grew `ListComments`, `ReplyToComment`, `BotIdentity` for
both GitHub (issue comments) and GitLab (MR notes, system notes
filtered out). `MergeRef.LastCommentCursor` persists per-MR.
