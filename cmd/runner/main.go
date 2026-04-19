// cmd/runner is the PID-1 process inside the worker pod.
//
// Responsibilities:
//  1. Parse CODE_AGENT_REPOS (comma-separated name=url@branch tuples) and
//     clone each repo into /workspace/<name> with shell `git`.
//  2. Write /workspace/opencode.json from CODE_AGENT_OPENCODE_CONFIG (raw
//     JSON) if supplied, otherwise a minimal default pointing at /workspace.
//  3. Exec `opencode serve` so it inherits PID 1 — that way `kubectl logs`
//     and lifecycle signals go straight to opencode.
//
// Repo credentials: if the env contains CODE_AGENT_GIT_TOKEN_<NAME>, we use
// it as the HTTP basic auth password (with "x-access-token" as username) by
// rewriting the URL. This keeps the tokens out of process args.
//
// The runner never phones home to the orchestrator; it just sets up the
// filesystem and hands off to opencode. The orchestrator talks to opencode
// over HTTP once its readiness probe succeeds.
package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	workspaceDir   = "/workspace"
	opencodeConfig = "/workspace/opencode.json"
	defaultPort    = "4096"
)

func main() {
	logf("runner starting: task=%s board=%s ext=%s",
		os.Getenv("CODE_AGENT_TASK_ID"),
		os.Getenv("CODE_AGENT_BOARD_ID"),
		os.Getenv("CODE_AGENT_EXTERNAL_ID"),
	)

	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		fatalf("mkdir %s: %v", workspaceDir, err)
	}

	repos, err := parseRepos(os.Getenv("CODE_AGENT_REPOS"))
	if err != nil {
		fatalf("parse CODE_AGENT_REPOS: %v", err)
	}
	for _, r := range repos {
		dest := filepath.Join(workspaceDir, r.Name)
		if _, err := os.Stat(filepath.Join(dest, ".git")); err == nil {
			logf("repo %s already cloned; skipping", r.Name)
			continue
		}
		if err := cloneRepo(r, dest); err != nil {
			fatalf("clone %s: %v", r.Name, err)
		}
	}

	if err := writeOpenCodeConfig(repos); err != nil {
		fatalf("write opencode config: %v", err)
	}

	port := os.Getenv("OPENCODE_PORT")
	if port == "" {
		port = defaultPort
	}
	bin, err := exec.LookPath("opencode")
	if err != nil {
		fatalf("opencode binary not found on PATH: %v", err)
	}
	args := []string{
		"opencode", "serve",
		"--port", port,
		"--hostname", "0.0.0.0",
	}
	logf("exec %s %s", bin, strings.Join(args[1:], " "))
	env := append(os.Environ(), "OPENCODE_CONFIG="+opencodeConfig)
	// execve: replace the runner so opencode is PID 1.
	if err := syscall.Exec(bin, args, env); err != nil {
		fatalf("exec opencode: %v", err)
	}
}

// repo describes one cloned repo.
type repo struct {
	Name       string
	URL        string
	BaseBranch string
}

func parseRepos(s string) ([]repo, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []repo
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, rest, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("repo %q missing '=': expected name=url@branch", part)
		}
		at := strings.LastIndex(rest, "@")
		var repoURL, branch string
		if at < 0 {
			repoURL = rest
		} else {
			repoURL = rest[:at]
			branch = rest[at+1:]
		}
		out = append(out, repo{Name: name, URL: repoURL, BaseBranch: branch})
	}
	return out, nil
}

func cloneRepo(r repo, dest string) error {
	cloneURL := r.URL
	// token injection: if CODE_AGENT_GIT_TOKEN_<UPPER_NAME> is set, use it.
	tokEnv := "CODE_AGENT_GIT_TOKEN_" + strings.ToUpper(strings.ReplaceAll(r.Name, "-", "_"))
	if tok := os.Getenv(tokEnv); tok != "" {
		if rewritten, ok := urlWithToken(cloneURL, tok); ok {
			cloneURL = rewritten
		}
	}

	args := []string{"clone", "--depth", "50"}
	if r.BaseBranch != "" {
		args = append(args, "-b", r.BaseBranch)
	}
	args = append(args, cloneURL, dest)

	start := time.Now()
	cmd := exec.Command("git", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone: %w", err)
	}
	logf("cloned %s@%s in %s", r.Name, r.BaseBranch, time.Since(start).Truncate(time.Millisecond))
	return nil
}

// urlWithToken rewrites http(s) clone URLs with an access token. For SSH URLs
// we give up and return ok=false (the caller should configure SSH in the image
// instead).
func urlWithToken(raw, token string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	u.User = url.UserPassword("x-access-token", token)
	return u.String(), true
}

func writeOpenCodeConfig(repos []repo) error {
	if raw := os.Getenv("CODE_AGENT_OPENCODE_CONFIG"); raw != "" {
		if !json.Valid([]byte(raw)) {
			return fmt.Errorf("CODE_AGENT_OPENCODE_CONFIG is not valid JSON")
		}
		return os.WriteFile(opencodeConfig, []byte(raw), 0o644)
	}
	allowed := []string{workspaceDir}
	for _, r := range repos {
		allowed = append(allowed, filepath.Join(workspaceDir, r.Name))
	}
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"folders": map[string]any{
			"allowed": allowed,
			"denied":  []string{workspaceDir + "/.git/hooks"},
		},
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(opencodeConfig, b, 0o644)
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[runner] "+format+"\n", args...)
}

func fatalf(format string, args ...any) {
	logf(format, args...)
	os.Exit(1)
}
