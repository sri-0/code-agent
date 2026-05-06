# code-agent architecture

`code-agent` is a Go orchestrator that drives [opencode](https://github.com/sst/opencode)
worker pods through a configurable state machine, anchored against board
tickets (Jira / ClickUp). Each ticket can flow plan → implement → MR open →
review-feedback → done, with the orchestrator owning git, MR creation,
ticket transitions, comments, and pod lifecycle. Workers run on
Kubernetes (minikube for local dev), the orchestrator runs on the host.

## Components

```
                          ┌─────────────────────────┐
   ClickUp / Jira ─────►  │  pkg/providers/{clickup,│
   (poll + webhook)       │   jira}                 │
                          └────────────┬────────────┘
                                       │ Event
                                       ▼
                          ┌─────────────────────────┐
                          │  pkg/providers          │
                          │   .Dispatcher (dedupe)  │
                          └────────────┬────────────┘
                                       │
                                       ▼
                          ┌─────────────────────────┐         ┌────────────────────┐
                          │  internal/orchestrator  │◄────────│  pkg/review.Poller │
                          │   .consume + engine     │ Event   │  (PR comments)     │
                          └────────────┬────────────┘         └────────────────────┘
                                       │ ActionInput
                                       ▼
                          ┌─────────────────────────┐
                          │  pkg/stages.Engine      │
                          │   per-ticket FSM        │
                          └────────────┬────────────┘
                                       │
                ┌──────────────┬───────┼───────┬───────────────────┐
                ▼              ▼       ▼       ▼                   ▼
        ┌─────────────┐  ┌─────────┐ ┌────┐  ┌──────────────┐  ┌──────────────┐
        │ run_agent   │  │ open_mr │ │noop│  │ comment_*    │  │ review_      │
        │ (agent_     │  │         │ │    │  │              │  │ feedback     │
        │  runner)    │  │         │ │    │  │              │  │              │
        └──────┬──────┘  └────┬────┘ └────┘  └──────────────┘  └──────┬───────┘
               │              │                                         │
               ▼              ▼                                         │
        ┌─────────────┐  ┌──────────────┐                               │
        │ pkg/runtime │  │ pkg/vcs      │◄──────────────────────────────┘
        │  .Runtime   │  │  (github,    │
        │ {persistent,│  │   gitlab)    │
        │  local,     │  └──────┬───────┘
        │  ephemeral} │         │
        └──────┬──────┘         │
               │ port-forward    │
               ▼                 ▼
        ┌─────────────┐    GitHub / GitLab
        │  Pod (k8s)  │
        │ ┌─────────┐ │
        │ │cmd/     │ │
        │ │ runner  │ │  ── admin HTTP :4100  ── git ops, skill upload
        │ └────┬────┘ │
        │      │      │
        │ ┌────▼────┐ │
        │ │opencode │ │  ── HTTP :4096        ── sessions, prompts, diffs
        │ │ serve   │ │
        │ └─────────┘ │
        └─────────────┘
```

## Code layout

| Path | Responsibility |
|---|---|
| `cmd/server` | Orchestrator entry point. |
| `cmd/runner` | PID-1 inside the worker pod: clones repos, exposes admin HTTP (git ops, skills, sessions), spawns `opencode serve`, seeds per-repo opencode projects. |
| `cmd/cli` | Operator CLI (cancel, inspect tasks). |
| `internal/orchestrator` | Wires providers, runtimes, stage engine, review poller. Owns the consume loop. |
| `internal/config` | YAML + envconfig loading: `boards.yaml`, `stages.yaml`, `runtime.yaml`, `opencode.yaml`, plus `SkillsIndex`. |
| `internal/handler` | HTTP handlers + dashboard templates (`/`, `/tasks/{id}`, `/api/tasks`). |
| `internal/server` | gorilla router + middleware. |
| `pkg/providers` | Board provider interface; `clickup/`, `jira/`, dispatcher (dedupe + polling fan-in). |
| `pkg/runtime` | Runtime interface (`local`, `ephemeral`, `persistent`, `shared`). `agent_runner.go` is the `run_agent` action. `skills.go` packs + uploads skill bundles. |
| `pkg/runtime/persistent` | Long-lived per-task k8s Deployment. Owns pod lifecycle + GC (TTL-based reaping) + port-forward. |
| `pkg/stages` | Configurable stage engine. Per-task in-flight locks, completed-stage tracking, success/failure routing. |
| `pkg/actions` | Stage-action runners: `open_mr` (commit/push/PR open + LLM-summarised messages), `review/` (PR feedback handler). |
| `pkg/review` | Poller that watches open MRs for new review comments and dispatches them to the review handler. |
| `pkg/vcs` | MR/PR API abstraction. Concrete `github/` (go-github) and `gitlab/` (go-gitlab) clients. Both implement `OpenMR`, `GetMR`, `Comment`, `ListComments`, `ReplyToComment`, `BotIdentity`. |
| `pkg/opencode` | OpenCode HTTP client. Supports per-request `x-opencode-directory` for multi-project servers. `WaitForTurn` polls assistant message `time.completed` instead of relying on SSE terminal events. |
| `pkg/llm` | OpenAI-compatible chat client (default OpenRouter). Used for commit/PR summaries (`SummarizeChange`) and PR comment classification (`ClassifyReviewComment`). |
| `pkg/k8s` | client-go wrapper: deployments, services, ingresses (templated), SPDY port-forward. |
| `pkg/tasks` | Task model + `Store` interface (valkey impl in `valkey/`). |
| `pkg/transcript` | Per-session SSE event log (valkey-backed). |
| `pkg/db/valkey` | Shared valkey client (env-driven). |
| `pkg/httpclient` | Tiny JSON HTTP wrapper with retries + per-call header injection. |
| `pkg/metrics` | Prometheus metric registration. |
| `docker/opencode/Dockerfile` | Worker image: node:22-slim + git + go + python3 + opencode + cmd/runner. |
| `deploy/k8s/rbac.yaml` | Namespace + ServiceAccount + Role + RoleBinding for the worker. Applied once via `make minikube-bootstrap`. |
| `config/default/` | Example config (committed). |
| `config/local/` | User's actual config (gitignored). |

## Configuration

Five YAML files (and environment overrides) drive behaviour:

### `boards.yaml`

One entry per board. Provider, auth env, filters (label / tag include +
exclude), triggers (poll interval / webhook), `runtime_ref` (which
runtime entry to use), `stages_ref` (which stage set), `repos[]` (with
per-repo VCS provider + auth env), `branch_prefix`, `mr_strategy`, and
optional `skills:` (bundle names).

### `stages.yaml`

Stage sets keyed by name. Each stage has:
- `provider_status` — board status this stage observes (or absent for
  internal stages reached via `success_next`).
- `action` — `noop`, `run_agent`, `open_mr`, `comment_error`,
  `wait_for_review`, `run_shell`.
- `agent` — opencode agent for `run_agent` (`plan`, `build`, `explore`).
- `provider`, `model` — opencode model selection.
- `system_prompt` — stage-specific system prompt.
- `skills:` — bundle names unioned with board-level skills.
- `success_next`, `failure_next`, `next` — stage routing.
- `terminal`, `human_review` — gating.
- `writes_plan` — write the agent's final message back to the ticket
  description.
- `timeout`, `max_retries`, `mr_template`.

### `runtime.yaml`

Top-level `git:` identity (`user_name`, `user_email`) injected into pods
for commits. Runtime entries keyed by name, each declaring `mode`
(`local` | `ephemeral` | `persistent` | `shared`), `namespace`, `image`,
`idle_timeout`, `resources`, `ingress` (host template + TLS secret).

### `opencode.yaml`

Free-form 1:1 with [opencode's config schema](https://opencode.ai/config.json).
Shipped to the pod as `CODE_AGENT_OPENCODE_CONFIG` and written to
`/home/worker/.config/opencode/opencode.json` by `cmd/runner`. Provider
API keys live inline (no separate env forwarding).

### Environment

| Variable | Default | Purpose |
|---|---|---|
| `PORT` | `8080` | Orchestrator HTTP port. |
| `CONFIG_DIR` | `config/default` | Where YAML files are loaded from. |
| `RUNTIME_MODE` | unset | Override every board's runtime selection. |
| `REDIS_HOST` / `REDIS_PORT` / `REDIS_USER` / `REDIS_PASS` / `REDIS_DB` | — | Valkey connection. |
| `OPENCODE_URL` / `OPENCODE_SERVER_PASSWORD` | — | For `local` runtime. |
| `CODE_AGENT_SKILLS_DIR` | unset | Host dir holding skill bundles (`<name>/SKILL.md`). |
| `CODE_AGENT_LLM_API_KEY` (or `AI_API_KEY`) | — | LLM key for commit/PR summaries + comment classifier. |
| `CODE_AGENT_LLM_BASE_URL` | `https://openrouter.ai/api/v1` | LLM endpoint (OpenAI-compatible). |
| `CODE_AGENT_LLM_MODEL` | `anthropic/claude-haiku-4.5` | LLM model. |
| `CODE_AGENT_POD_TTL_AFTER_MR_CLOSED` | `24h` | Persistent-runtime grace period after all MRs closed/merged. |
| `CODE_AGENT_POD_TTL_MAX` | `168h` | Absolute pod lifetime ceiling. |
| `CODE_AGENT_POD_GC_INTERVAL` | `10m` | GC + review-poller cadence. |
| Per-board: `CLICKUP_TOKEN`, `JIRA_TOKEN_*`, `GITHUB_BOT_TOKEN`, `GITLAB_BOT_TOKEN`, etc. | — | Resolved by `auth.env` references. |

## Pod lifecycle (persistent runtime)

1. **Spawn** — orchestrator's `EnsureWorker` checks for existing
   Deployment; if absent, builds env (task id, board id, repo CSV with
   per-repo `CODE_AGENT_GIT_TOKEN_<NAME>`, branch prefix, opencode
   config JSON, git identity) and creates the Deployment + Service +
   optional Ingress via `pkg/k8s`.
2. **Wait ready** — polls Deployment until 1+ pod is `Running` +
   `Ready`. Out-of-cluster (laptop), establishes SPDY port-forward to
   pod ports `4096` (opencode) and `4100` (admin), assigning ephemeral
   local ports.
3. **Cache the ref** — task's `WorkerRef` (URL, AdminURL, pod name) is
   saved to valkey immediately so the dashboard surfaces the live URL
   during long agent turns. The ref is also kept in an in-process map so
   subsequent stages reuse the same pod + port-forward.
4. **`cmd/runner` startup**:
   - Apply `git config --global user.name/email` from env.
   - Clone every `CODE_AGENT_REPOS` entry into `/workspace/<name>` with
     a token-rewritten URL.
   - For each repo, if `origin/ai/<ticket>` exists, check it out so
     prior agent work is on disk.
   - Write opencode config to `/home/worker/.config/opencode/opencode.json`.
   - Spawn `opencode serve --port 4096 --hostname 0.0.0.0`.
   - Start admin HTTP on :4100 with endpoints:
     - `POST /sessions/{id}/setup` — clone repos into a session subdir.
     - `DELETE /sessions/{id}` — wipe a session subdir.
     - `GET /repos/{name}/status` — git status JSON.
     - `GET /repos/{name}/diff` — staged diff.
     - `POST /repos/{name}/commit-push` — checkout+add+commit+push.
     - `POST /skills` — extract a gzip tar into `~/.config/opencode/skills/`.
     - `GET /healthz`.
   - Background: ping `GET /project/current?directory=/workspace/<repo>`
     for every repo so opencode auto-registers each as a discrete project
     and they all appear in the web UI sidebar.
5. **Skill sync** — before each `run_agent` stage, the orchestrator
   resolves the union of `board.skills + stage.skills` against the host
   `SkillsIndex`, tars matching bundles, and POSTs to
   `<admin_url>/skills`.
6. **Agent turn** — orchestrator creates an opencode session
   (optionally scoped to `/workspace/<repo>` via `x-opencode-directory`
   header), submits via `POST /session/{id}/prompt_async`, then polls
   `GET /session/{id}/message` until the tail assistant message has
   `time.completed != 0`. Provider errors recorded on the message
   (`error.data.message`) become a stage failure with a comment posted
   to the ticket. An empty turn (zero text + zero tool calls) is also
   treated as a failure with a diagnostic comment.
7. **GC** — every `CODE_AGENT_POD_GC_INTERVAL`, the persistent runtime
   scans live pods:
   - **Rule 1 (max age)** — pod older than `CODE_AGENT_POD_TTL_MAX` →
     `Cleanup`.
   - **Rule 2 (post-MR grace)** — refresh every MR's state via VCS;
     when all are `closed`/`merged` and the latest close is older than
     `CODE_AGENT_POD_TTL_AFTER_MR_CLOSED` → `Cleanup`.
8. **Cleanup** — drop in-memory state, close port-forward, delete the
   Deployment.

## Stage flow (default `prism-main` board)

```
Open ─────────────────► plan       (run_agent, agent=plan, writes_plan)
                          │ success
                          ▼
in progress ────────► implement   (run_agent, agent=build)
                          │ success
                          ▼
                       open_mr     (commit + push + PR per repo)
                          │ success
                          ▼
testing  ─────────────►  done      (noop, terminal)
```

`open_mr` records the resulting `MergeRef` (URL, number/IID, source +
target branches) on the task. After `done`, the review poller starts
watching every MR.

## Review-feedback loop

Every `CODE_AGENT_POD_GC_INTERVAL`, `pkg/review.Poller`:

1. `tasks.Store.ListByBoard` for each board.
2. For each task with `MergeRequests`, build a `vcs.Client` and call
   `ListComments(since=mr.LastCommentCursor)`.
3. Skip comments containing the embedded HTML marker
   `<!-- code-agent:reply -->` (this avoids self-loops when the bot uses
   a human's PAT — author-based skipping breaks because the PAT's login
   matches the human's own comments).
4. Hand each new comment to `pkg/actions/review.Handler.Handle`:
   - **Classify** via `llm.ClassifyReviewComment` →
     `{question | change_request | approval | noise}` with confidence.
     `change_request` below 0.7 confidence is downgraded to `question`
     for safety.
   - **`approval` / `noise`** → no-op, advance cursor.
   - **`question`** → ensure pod, refresh `WorkerRef`, create opencode
     session scoped to `/workspace/<repo>` in `explore` agent (read-only
     prompt), wait for turn, post the assistant's text as a quoted PR
     reply with the marker appended.
   - **`change_request`** → same as question but with `build` agent
     (allowed to edit). After the turn, call cmd/runner's
     `commit-push` admin endpoint on the existing feature branch
     (`branchExistsLocal` in cmd/runner avoids resetting the branch when
     it's already checked out, preventing non-fast-forward pushes). Then
     post the agent's summary as a PR reply.
5. `MergeRef.LastCommentCursor` is advanced past every processed
   comment and persisted to valkey.

## Resilience

Every failure path posts a comment to the originating ticket so
operators see *why* a stage stopped:

- **Provider error** (credit limit, rate limit, auth, 5xx) — surfaced
  from opencode's `message.error.data.message`.
- **Empty turn** (no text, no tool calls) — caught explicitly to avoid
  silently advancing to `open_mr` with nothing to push.
- **Stage timeout** — stage ctx deadline → opencode session aborted +
  comment.
- **`open_mr` push/commit/PR-open errors** — each failure includes the
  repo name + underlying git/API error.
- **Self-deadlock** — per-task per-stage in-flight locks prevent the
  same poll event re-entering an in-progress stage.
- **Re-entry after completion** — `Task.CompletedStages[]` tracks every
  stage that's already run; `engine.HandleEvent` skips re-entry so a
  ticket update from the bot's own ticket-description-write or
  tag-add doesn't restart a finished stage.

## Dashboard

`http://localhost:8080`:

- `/` — task list (status + stage + last activity).
- `/tasks/{id}` — task detail: stage, MRs, repos with deep-links to the
  pod's per-repo opencode project view (`<URL>/?directory=/workspace/<repo>`),
  transcript events.
- `/api/tasks`, `/api/tasks/{id}`, `POST /api/tasks/{id}/cancel` — JSON.
- `/healthz`, `/metrics`.

## Local development

```
make minikube-up            # start minikube (docker driver)
make minikube-bootstrap     # build worker image, kubectl apply rbac, image-load
docker compose up -d valkey # local valkey
make build                  # bin/{server,runner,cli}
./bin/server                # orchestrator on :8080
```

The orchestrator picks up `.env` via godotenv. `config/local/` is
gitignored — `config/default/` carries example values mirrored with the
local set.

To rebuild the worker image after changes:

```
docker build --no-cache -f docker/opencode/Dockerfile -t code-agent-worker:dev .
minikube image rm code-agent-worker:dev   # only when no live pod uses it
minikube image load code-agent-worker:dev
```

## Skills

Skill bundles are Anthropic-style `<name>/SKILL.md (+ supporting files)`
directories under `CODE_AGENT_SKILLS_DIR`. The orchestrator indexes them
on boot and uploads the per-stage union to the pod before each
`run_agent` turn (admin HTTP `/skills`, gzip tar). cmd/runner extracts
into `/home/worker/.config/opencode/skills/`, where opencode auto-loads
them. Reference bundles by directory name from `boards.yaml` or
`stages.yaml`:

```yaml
skills:
  - frontend-design
  - tailwind-design-system
  - ui-ux-pro-max
```
