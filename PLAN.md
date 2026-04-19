# code-agent — Plan & Status

## Goal

Build a Go orchestrator service at `~/code/code-agent` that drives OpenCode AI coding workers through configurable stages based on Jira/ClickUp tickets. The orchestrator:

- Receives webhook + polls Jira/ClickUp boards
- Spawns OpenCode workers in multiple modes:
  - `local` (dev, talks to host opencode)
  - `ephemeral` (k8s Job)
  - `persistent` (k8s Deployment+Ingress per task)
  - `shared` (k8s Deployment+Ingress per board, many sessions)
- Moves tickets through configurable stages (todo → in_progress → test → ready_for_qa → blocked); each stage can have a system prompt and an action (`run_agent`, `open_mr`, `comment_error`, etc.)
- Handles multi-repo changes per ticket (single ticket may modify e.g. api + ui repos)
- Opens MRs/PRs per repo via GitLab + GitHub bot tokens (orchestrator, not the agent)
- Persists chat transcripts even after worker dies

## Instructions / Conventions

- Structure mirrors `~/code/agentic` (same patterns: zerolog, envconfig + YAML, bootstrap.Init, gorilla/mux).
- **ALL external integrations go under `pkg/`**; `internal/` is thin wiring only.
- Use real multi-line YAML in config files, **no** flow-style `{}` or inline maps.
- No edits to `~/code/agentic` — its `go.mod` has `module agentic` (non-fetchable), so we **copy** small pieces (e.g. `pkg/logging`, `pkg/db/valkey`) rather than `go get`.
- Storage: **Valkey/Redis** (may move to OpenSearch later).
- VCS: both GitLab (`xanzy/go-gitlab`) and GitHub (`google/go-github`).
- Git ops: `go-git` orchestrator-side, shell `git` inside worker image.
- Always clone all configured repos per ticket (simple, wasteful but fine for now).
- No Ingress auth for now; oauth2-proxy later.
- Shared-mode workspace isolation via **per-session subdirs** (`/workspace/<session>/<repo>`) so OpenCode `folders.allowed` can scope per session.
- Two local dev modes: `make dev-local` (no k8s) and `make dev-k8s` (minikube).
- MRs: orchestrator opens per-repo MRs, cross-linked; branch = `{branch_prefix}{ticket_id}` identical across repos.
- Build in phases; commit after each so user can sanity-check.

## Discoveries

- `agentic`'s git remote is `git@github.com:sri-0/agentic.git`, but its module name is `module agentic` (not `github.com/sri-0/agentic`), which makes it un-`go get`-able without editing. Decision: copy files instead.
- `agentic` uses valkey-go, zerolog, envconfig, gorilla/mux, yaml.v3, joho/godotenv — same stack adopted here.
- Copied source shape from `agentic/pkg/logging/{setup.go,middleware.go}` and `agentic/pkg/db/valkey/{client.go,kv.go}` with import paths rewritten to `code-agent/...` and `Setup` updated to accept `(level, jsonOutput)` args.
- Final approved plan v3 has 4 runtime modes and phased build order (Phase 1 scaffolding → Phase 8 CLI/metrics).
- `go build ./...` was attempted after `go mod tidy` succeeded; the first couple of attempts had tool "Stream closed" errors and the build result is **not yet confirmed**. Next agent should re-run `go build ./...` first thing.

## Status (2026-04-19)

Phases 1–8 implemented and building clean (`go build ./...`, `go vet ./...`).
Binaries `bin/{server,cli,runner}` all compile. No tests yet (Phase 9
candidate).

- Phase 1 — scaffolding (commit 0e71bcf)
- Phase 2 — providers (commit f4429d9)
- Phase 3 — stages engine + opencode HTTP+SSE + transcript (commit 89e9577)
- Phase 4 — local runtime + run_agent runner (commit ece56d9)
- Phase 5 — ephemeral k8s runtime + worker image + cmd/runner supervisor
  (commit 778fe03)
- Phase 6 — persistent + shared runtimes, admin HTTP on :4100 for shared
  per-session clones (commit 88b1bbe)
- Phase 7 — VCS (gitlab + github), pkg/git ls-remote check, open_mr
  ActionRunner with per-repo cross-linking (commit bcb21aa)
- Phase 8 — operator CLI, JSON API under /api, read-only HTML dashboard,
  Prometheus /metrics (this commit)

## Accomplished — Phase 1 scaffolding (build not yet verified)

- Repo directory tree created (`cmd/{server,cli,runner}`, `internal/{bootstrap,config,orchestrator,handler,server}`, `pkg/{logging,httpclient,db/valkey,providers/{jira,clickup},vcs/{gitlab,github},git,opencode,k8s,stages,runtime/{local,ephemeral,persistent,shared},tasks/valkey,transcript}`, `config/default`, `deploy/k8s`, `docker/opencode`, `test`), `git init` done.
- `go.mod` with `module code-agent`, `go 1.24`.
- `go mod tidy` pulled deps: zerolog, valkey-go, envconfig, gorilla/mux, yaml.v3, godotenv.
- Foundation files: `.gitignore`, `.env.example`, `README.md`, `Makefile` (server/dev-local/dev-k8s/cli/build/test/tidy/fmt/vet/docker/minikube-up/down), `docker-compose.yaml` (valkey + redis-insight), `Dockerfile` (distroless-style alpine multi-stage).
- `pkg/logging/{setup.go,middleware.go}` — zerolog setup + HTTP middleware.
- `pkg/db/valkey/{client.go,kv.go}` — valkey client with Set/Get/Del/SAdd/SMembers/SRem.
- `pkg/httpclient/client.go` — retry+backoff+jitter HTTP client with `HTTPError` type.
- `pkg/tasks/{model.go,store.go}` — `Task`, `WorkerRef`, `RepoState`, `MergeRef` types + `Store` interface.
- `pkg/tasks/valkey/store.go` — valkey-backed Store with keying scheme `task:{id}`, `task:ext:{board}:{ext}`, `board:{id}:tasks`, `stage:{name}:tasks`.
- `internal/config/{config.go,boards.go,stages.go,runtime.go,opencode.go}` — envconfig root + YAML loaders.
- `internal/bootstrap/bootstrap.go` — mirrors agentic; loads env+YAML, sets up logger, connects valkey, builds task store, returns Result.
- `internal/handler/health.go` + `internal/server/router.go` — `/health` endpoint with logging middleware.
- `cmd/server/main.go` — HTTP server with graceful shutdown.
- `cmd/cli/main.go`, `cmd/runner/main.go` — placeholder stubs.
- `config/default/{boards.yaml,stages.yaml,runtime.yaml,opencode.yaml}` — example configs with multi-line YAML (no flow style).

## Immediate next step when resuming

1. `cd ~/code/code-agent && go build ./...` to confirm Phase 1 compiles.
2. Fix any errors. Then `go vet ./...`.
3. First git commit: "Phase 1: scaffolding".

## Remaining phases (in order)

- **Phase 2:** `pkg/providers/{jira,clickup}` — BoardProvider interface (ListTasks, Get, Transition, Comment, Webhook handler), both REST clients, webhook signature verification, pollers with dedupe into a single event channel.
- **Phase 3:** `pkg/stages` (state machine engine), `pkg/opencode` (HTTP client + SSE), `pkg/transcript` (SSE ingestor → store).
- **Phase 4:** `pkg/runtime/local` end-to-end (`make dev-local` against host opencode on `:4096`).
- **Phase 5:** `pkg/k8s` client-go helpers, `pkg/runtime/ephemeral` (Job), flesh out `cmd/runner` that runs inside worker pod.
- **Phase 6:** `pkg/runtime/persistent` (Deployment+Svc+Ingress per task), `pkg/runtime/shared` (per board, per-session subdirs).
- **Phase 7:** `pkg/vcs/{gitlab,github}`, `pkg/git` (go-git), stage action `open_mr` with per-repo cross-linked MRs.
- **Phase 8:** `cmd/cli` operator tooling, Prometheus `/metrics`, small read-only dashboard.

## Relevant files / directories

- `~/code/code-agent/` — the new repo (all work)
- `~/code/code-agent/go.mod`, `go.sum`, `.gitignore`, `.env.example`, `README.md`, `Makefile`, `docker-compose.yaml`, `Dockerfile`
- `~/code/code-agent/cmd/server/main.go` — HTTP server entry
- `~/code/code-agent/cmd/{cli,runner}/main.go` — placeholders
- `~/code/code-agent/internal/bootstrap/bootstrap.go`
- `~/code/code-agent/internal/config/{config.go,boards.go,stages.go,runtime.go,opencode.go}`
- `~/code/code-agent/internal/handler/health.go`
- `~/code/code-agent/internal/server/router.go`
- `~/code/code-agent/pkg/logging/{setup.go,middleware.go}` (copied from agentic)
- `~/code/code-agent/pkg/db/valkey/{client.go,kv.go}` (copied from agentic, imports rewritten)
- `~/code/code-agent/pkg/httpclient/client.go`
- `~/code/code-agent/pkg/tasks/{model.go,store.go}`
- `~/code/code-agent/pkg/tasks/valkey/store.go`
- `~/code/code-agent/config/default/{boards.yaml,stages.yaml,runtime.yaml,opencode.yaml}`
- Empty dirs reserved for later phases: `pkg/{providers/{jira,clickup},vcs/{gitlab,github},git,opencode,k8s,stages,runtime/{local,ephemeral,persistent,shared},transcript}`, `internal/{orchestrator}`, `deploy/k8s`, `docker/opencode`, `test`
- **Reference only (do not edit):** `~/code/agentic/` — source repo being mirrored.
