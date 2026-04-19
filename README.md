# code-agent

Go orchestrator that drives OpenCode workers through configurable stages from
Jira / ClickUp boards. Runs ephemeral k8s Jobs, per-task or shared Deployments,
or against a locally running `opencode serve` for development.

## Runtime modes

| Mode         | k8s resource                    | Use case                                          |
|--------------|---------------------------------|---------------------------------------------------|
| `local`      | none                            | dev; orchestrator talks to local opencode         |
| `ephemeral`  | `Job`                           | one-shot per task x stage                         |
| `persistent` | `Deployment + Svc + Ingress`    | per-task long-running container with web UI       |
| `shared`     | `Deployment + Svc + Ingress`    | per-board long-running container, many sessions   |

## Quick start (local dev)

```bash
# 1. Start valkey
docker compose up -d

# 2. Start your local opencode server
opencode serve --port 4096

# 3. Copy env and fill tokens
cp .env.example .env

# 4. Run orchestrator
make dev-local
```

## Layout

- `cmd/` — entry points (`server`, `cli`, `runner`)
- `internal/` — this service's wiring (bootstrap, config, orchestrator, handlers, server)
- `pkg/` — reusable libraries (providers, vcs, git, opencode, k8s, stages, runtime, tasks, transcript)
- `config/default/` — default YAML config (`boards.yaml`, `stages.yaml`, `runtime.yaml`, `opencode.yaml`)
- `deploy/k8s/` — orchestrator deployment manifests
