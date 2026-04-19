# syntax=docker/dockerfile:1

FROM golang:1.24-alpine AS build
WORKDIR /src
RUN apk add --no-cache git ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/server ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/server /usr/local/bin/server
COPY config /etc/code-agent/config
ENV CONFIG_DIR=/etc/code-agent/config/default
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/server"]
