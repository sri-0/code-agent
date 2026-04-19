// cmd/runner is the PID-1 process inside the worker pod.
//
// Responsibilities:
//  1. In eager mode (ephemeral / persistent), parse CODE_AGENT_REPOS and
//     clone each repo into /workspace/<name> with shell `git`. In shared
//     mode (CODE_AGENT_RUNTIME_MODE=shared) skip upfront cloning — the
//     orchestrator drives per-session clones via the admin HTTP below.
//  2. Write /workspace/opencode.json (from CODE_AGENT_OPENCODE_CONFIG, or
//     a minimal default pointing at /workspace).
//  3. Start a small admin HTTP on CODE_AGENT_ADMIN_PORT (default 4100):
//       POST /sessions/{id}/setup {repos:[{name,url,base_branch}]}
//       DELETE /sessions/{id}
//       GET  /healthz
//     The orchestrator uses these to carve per-session subdirs in shared
//     mode.
//  4. Spawn `opencode serve` as a child process, forward stdout/stderr, and
//     propagate SIGTERM/SIGINT. Exit with opencode's exit code.
//
// Repo credentials: if CODE_AGENT_GIT_TOKEN_<UPPER_NAME> is set we inject
// it as HTTP basic auth (user "x-access-token") by rewriting the URL.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const (
	workspaceDir   = "/workspace"
	opencodeConfig = "/workspace/opencode.json"
	defaultPort    = "4096"
	defaultAdmin   = "4100"
)

func main() {
	mode := os.Getenv("CODE_AGENT_RUNTIME_MODE") // ephemeral | persistent | shared
	logf("runner starting: mode=%s task=%s board=%s ext=%s",
		mode,
		os.Getenv("CODE_AGENT_TASK_ID"),
		os.Getenv("CODE_AGENT_BOARD_ID"),
		os.Getenv("CODE_AGENT_EXTERNAL_ID"),
	)

	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		fatalf("mkdir %s: %v", workspaceDir, err)
	}

	// Give the pod's git a default identity so commits succeed. This
	// writes /home/worker/.gitconfig (no --system needed). Override via
	// CODE_AGENT_GIT_USER_NAME/EMAIL env if you want something other
	// than the defaults.
	gitName := envDefault("CODE_AGENT_GIT_USER_NAME", "code-agent")
	gitEmail := envDefault("CODE_AGENT_GIT_USER_EMAIL", "code-agent@local")
	if err := exec.Command("git", "config", "--global", "user.name", gitName).Run(); err != nil {
		logf("warn: git config user.name: %v", err)
	}
	if err := exec.Command("git", "config", "--global", "user.email", gitEmail).Run(); err != nil {
		logf("warn: git config user.email: %v", err)
	}

	eager := mode != "shared"
	repos, err := parseRepos(os.Getenv("CODE_AGENT_REPOS"))
	if err != nil {
		fatalf("parse CODE_AGENT_REPOS: %v", err)
	}
	if eager {
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
	}
	if err := writeOpenCodeConfig(repos, eager); err != nil {
		fatalf("write opencode config: %v", err)
	}

	adminPort := envDefault("CODE_AGENT_ADMIN_PORT", defaultAdmin)
	adminSrv := startAdminHTTP(adminPort)

	ocPort := envDefault("OPENCODE_PORT", defaultPort)
	bin, err := exec.LookPath("opencode")
	if err != nil {
		fatalf("opencode binary not found on PATH: %v", err)
	}
	cmd := exec.Command(bin, "serve", "--port", ocPort, "--hostname", "0.0.0.0")
	cmd.Env = append(os.Environ(), "OPENCODE_CONFIG="+opencodeConfig)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		fatalf("start opencode: %v", err)
	}
	logf("opencode pid=%d port=%s", cmd.Process.Pid, ocPort)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var exitCode int
	select {
	case sig := <-sigCh:
		logf("received %s; forwarding to opencode", sig)
		_ = cmd.Process.Signal(sig)
		select {
		case err := <-waitCh:
			exitCode = exitCodeOf(err)
		case <-time.After(15 * time.Second):
			logf("opencode did not exit in 15s; killing")
			_ = cmd.Process.Kill()
			<-waitCh
			exitCode = 137
		}
	case err := <-waitCh:
		exitCode = exitCodeOf(err)
		logf("opencode exited: code=%d err=%v", exitCode, err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = adminSrv.Shutdown(shutdownCtx)
	os.Exit(exitCode)
}

// ---- admin HTTP ----

type setupReq struct {
	Repos []repo `json:"repos"`
}

func startAdminHTTP(port string) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("/sessions/", handleSessions)
	mux.HandleFunc("/repos/", handleRepos)

	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		fatalf("admin listen %s: %v", srv.Addr, err)
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logf("admin http: %v", err)
		}
	}()
	logf("admin http listening on %s", srv.Addr)
	return srv
}

// /sessions/{id}/setup     POST   — clone repos into /workspace/{id}/<name>
// /sessions/{id}           DELETE — rm -rf /workspace/{id}
func handleSessions(w http.ResponseWriter, r *http.Request) {
	// strip prefix
	p := strings.TrimPrefix(r.URL.Path, "/sessions/")
	parts := strings.SplitN(p, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "session id required", 400)
		return
	}
	sid := parts[0]
	if !validSessionID(sid) {
		http.Error(w, "invalid session id", 400)
		return
	}
	sub := ""
	if len(parts) == 2 {
		sub = parts[1]
	}

	switch {
	case r.Method == http.MethodPost && sub == "setup":
		handleSetup(w, r, sid)
	case r.Method == http.MethodDelete && sub == "":
		handleDelete(w, r, sid)
	default:
		http.Error(w, "not found", 404)
	}
}

var sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func validSessionID(s string) bool { return sessionIDRe.MatchString(s) }

func handleSetup(w http.ResponseWriter, r *http.Request, sid string) {
	var req setupReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad body: "+err.Error(), 400)
		return
	}
	base := filepath.Join(workspaceDir, sid)
	if err := os.MkdirAll(base, 0o755); err != nil {
		http.Error(w, "mkdir: "+err.Error(), 500)
		return
	}
	for _, rp := range req.Repos {
		dest := filepath.Join(base, rp.Name)
		if _, err := os.Stat(filepath.Join(dest, ".git")); err == nil {
			continue
		}
		if err := cloneRepo(rp, dest); err != nil {
			http.Error(w, "clone "+rp.Name+": "+err.Error(), 500)
			return
		}
	}
	w.WriteHeader(204)
}

func handleDelete(w http.ResponseWriter, _ *http.Request, sid string) {
	base := filepath.Join(workspaceDir, sid)
	if err := os.RemoveAll(base); err != nil {
		http.Error(w, "rm: "+err.Error(), 500)
		return
	}
	w.WriteHeader(204)
}

// ---- repo git operations ----
//
// GET  /repos/{name}/status        -> {dirty: bool, changes: ["M path", ...]}
// GET  /repos/{name}/diff          -> raw git diff after `git add -A`
// POST /repos/{name}/commit-push   -> body: {branch, commit_message}; runs
//                                     checkout -B branch, add -A, commit,
//                                     push -u origin branch
//
// All paths operate on /workspace/<name>. 404 if the dir isn't a git
// checkout. Output is intentionally minimal — the orchestrator decides
// branch names and commit messages.

var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func handleRepos(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/repos/")
	parts := strings.SplitN(p, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "repo name required", 400)
		return
	}
	name := parts[0]
	if !repoNameRe.MatchString(name) {
		http.Error(w, "invalid repo name", 400)
		return
	}
	sub := ""
	if len(parts) == 2 {
		sub = parts[1]
	}

	dir := filepath.Join(workspaceDir, name)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		http.Error(w, "repo not found", 404)
		return
	}

	switch {
	case r.Method == http.MethodGet && sub == "status":
		handleRepoStatus(w, r, dir)
	case r.Method == http.MethodGet && sub == "diff":
		handleRepoDiff(w, r, dir)
	case r.Method == http.MethodPost && sub == "commit-push":
		handleRepoCommitPush(w, r, dir)
	default:
		http.Error(w, "not found", 404)
	}
}

func handleRepoStatus(w http.ResponseWriter, _ *http.Request, dir string) {
	out, err := runGitOut(dir, "status", "--porcelain")
	if err != nil {
		http.Error(w, "git status: "+err.Error(), 500)
		return
	}
	changes := []string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			changes = append(changes, line)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"dirty":   len(changes) > 0,
		"changes": changes,
	})
}

func handleRepoDiff(w http.ResponseWriter, _ *http.Request, dir string) {
	// Stage untracked + modified so `git diff --cached` shows everything.
	if err := runGit(dir, "add", "-A"); err != nil {
		http.Error(w, "git add: "+err.Error(), 500)
		return
	}
	out, err := runGitOut(dir, "diff", "--cached", "--no-color")
	if err != nil {
		http.Error(w, "git diff: "+err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(out)
}

type commitPushReq struct {
	Branch        string `json:"branch"`
	CommitMessage string `json:"commit_message"`
	BaseBranch    string `json:"base_branch"` // optional; if set, `git checkout -B branch base_branch`
}

func handleRepoCommitPush(w http.ResponseWriter, r *http.Request, dir string) {
	var req commitPushReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad body: "+err.Error(), 400)
		return
	}
	if req.Branch == "" || req.CommitMessage == "" {
		http.Error(w, "branch and commit_message required", 400)
		return
	}
	steps := [][]string{}
	if req.BaseBranch != "" {
		steps = append(steps, []string{"checkout", "-B", req.Branch, req.BaseBranch})
	} else {
		steps = append(steps, []string{"checkout", "-B", req.Branch})
	}
	steps = append(steps,
		[]string{"add", "-A"},
		[]string{"commit", "-m", req.CommitMessage},
		[]string{"push", "-u", "origin", req.Branch},
	)
	for _, args := range steps {
		if err := runGit(dir, args...); err != nil {
			http.Error(w, "git "+strings.Join(args, " ")+": "+err.Error(), 500)
			return
		}
	}
	w.WriteHeader(204)
}

func runGit(dir string, args ...string) error {
	full := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func runGitOut(dir string, args ...string) ([]byte, error) {
	full := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", full...)
	return cmd.Output()
}

// ---- setup helpers ----

type repo struct {
	Name       string `json:"name"`
	URL        string `json:"url"`
	BaseBranch string `json:"base_branch"`
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

func writeOpenCodeConfig(repos []repo, eager bool) error {
	if raw := os.Getenv("CODE_AGENT_OPENCODE_CONFIG"); raw != "" {
		if !json.Valid([]byte(raw)) {
			return fmt.Errorf("CODE_AGENT_OPENCODE_CONFIG is not valid JSON")
		}
		return os.WriteFile(opencodeConfig, []byte(raw), 0o644)
	}
	allowed := []string{workspaceDir}
	if eager {
		for _, r := range repos {
			allowed = append(allowed, filepath.Join(workspaceDir, r.Name))
		}
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

// ---- misc ----

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return 1
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[runner] "+format+"\n", args...)
}

func fatalf(format string, args ...any) {
	logf(format, args...)
	os.Exit(1)
}
