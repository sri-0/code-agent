.PHONY: server dev-local dev-k8s cli build test tidy docker fmt vet minikube-up minikube-down

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

# --- Minikube helpers ---
minikube-up:
	minikube start --driver=docker --cpus=4 --memory=6g
	kubectl apply -f deploy/k8s/rbac.yaml

minikube-down:
	minikube stop
