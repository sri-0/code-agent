# syntax=docker/dockerfile:1
#
# Worker image for code-agent: bundles the opencode CLI + our cmd/runner
# as PID 1. The orchestrator creates Jobs/Deployments from this image and
# talks to the running opencode server at port 4096.

# --- builder: compile cmd/runner ---
FROM golang:1.24-alpine AS build
WORKDIR /src
RUN apk add --no-cache git ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/runner ./cmd/runner

# --- runtime: node + git + opencode + our runner ---
FROM node:22-alpine
RUN apk add --no-cache git openssh-client ca-certificates bash curl
# Pin opencode to a known version; override with --build-arg OPENCODE_VERSION=...
ARG OPENCODE_VERSION=latest
RUN npm install -g opencode-ai@${OPENCODE_VERSION} \
    && opencode --version

COPY --from=build /out/runner /usr/local/bin/runner

WORKDIR /workspace
EXPOSE 4096
# Run as a non-root user with a real HOME so opencode can persist auth files.
RUN adduser -D -u 1000 -h /home/worker worker \
    && mkdir -p /workspace \
    && chown -R worker:worker /workspace /home/worker
USER worker
ENV HOME=/home/worker

ENTRYPOINT ["/usr/local/bin/runner"]
