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

# --- runtime: node (debian/glibc) + git + opencode + our runner ---
# Alpine's musl libc breaks opencode's postinstall native binary — opencode
# downloads a glibc-built shim that spawnSyncs a native binary. Stay on
# debian-slim for compatibility.
FROM node:22-slim

# System toolchains. git is always needed. python3 + pip3 let opencode
# detect Python projects (pyright auto-installs via npm). Go is installed
# from the official tarball below (apt ships 1.19, too old for modern
# gopls / projects).
RUN apt-get update && apt-get install -y --no-install-recommends \
      git openssh-client ca-certificates curl \
      python3 python3-pip \
    && rm -rf /var/lib/apt/lists/*

# Install Go from the official tarball so opencode's gopls LSP can run and
# agents can build Go repos. Read the image arch at build time rather than
# trust BuildKit's TARGETARCH (which isn't always set and silently falls
# back to the ARG default, producing an arch-mismatched binary).
ARG GO_VERSION=1.23.4
RUN GO_ARCH=$(dpkg --print-architecture) \
 && curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${GO_ARCH}.tar.gz" \
      | tar -C /usr/local -xz \
 && /usr/local/go/bin/go version
ENV PATH="/usr/local/go/bin:${PATH}"

# Pin opencode to a known version; override with --build-arg OPENCODE_VERSION=...
ARG OPENCODE_VERSION=latest
RUN npm install -g opencode-ai@${OPENCODE_VERSION} \
    && opencode --version

COPY --from=build /out/runner /usr/local/bin/runner

WORKDIR /workspace
EXPOSE 4096
# Run as a non-root user with a real HOME so opencode can persist auth files.
# node:22-slim already ships a 'node' user at UID 1000; pick 1001.
# Pre-create the opencode config dir with skills/ subdir — cmd/runner writes
# opencode.json here and receives skill bundles at /skills/... via the
# admin HTTP. Kept outside /workspace so the agent doesn't see config files.
RUN useradd -m -u 1001 -d /home/worker -s /bin/bash worker \
    && mkdir -p /workspace /home/worker/.config/opencode/skills \
    && chown -R worker:worker /workspace /home/worker
USER worker
ENV HOME=/home/worker

ENTRYPOINT ["/usr/local/bin/runner"]
