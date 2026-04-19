.PHONY: server dev-local dev-k8s cli build test tidy docker docker-worker fmt vet minikube-up minikube-down minikube-bootstrap

# Production server entry
server:
	go run ./cmd/server

# Local dev: orchestrator on host, talks to a locally running `opencode serve` on $OPENCODE_URL
dev-local:
	RUNTIME_MODE=local go run ./cmd/server

# Dev against minikube
dev-k8s:
	RUNTIME_MODE=ephemeral go run ./cmd/server

# Operator CLI
cli:
	go run ./cmd/cli

# Build all binaries
build:
	mkdir -p bin
	go build -o bin/server ./cmd/server
	go build -o bin/cli    ./cmd/cli
	go build -o bin/runner ./cmd/runner

test:
	go test ./...

tidy:
	go mod tidy

fmt:
	gofmt -w -s .

vet:
	go vet ./...

# Docker image for the orchestrator
docker:
	docker build -t code-agent:dev .

# Docker image for the opencode worker pod (packs cmd/runner + opencode CLI)
docker-worker:
	docker build -f docker/opencode/Dockerfile -t code-agent-worker:dev .

# --- Minikube helpers ---
# Start minikube only — no cluster resources created.
minikube-up:
	minikube start --driver=docker --cpus=4 --memory=6g

minikube-down:
	minikube stop

# One-time cluster bootstrap: namespace + RBAC + worker image. Run once
# after minikube-up (or whenever the worker image changes).
minikube-bootstrap: docker-worker
	kubectl apply -f deploy/k8s/rbac.yaml
	minikube image load code-agent-worker:dev
	@echo ""
	@echo "--- minikube bootstrap complete ---"
	@kubectl -n code-agent get sa,role,rolebinding
	@minikube ssh -- docker images code-agent-worker:dev 2>/dev/null | tail -2
