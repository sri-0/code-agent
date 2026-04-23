// cmd/runner is the PID-1 process inside the worker pod.
//
// Responsibilities:
//  1. In eager mode (ephemeral / persistent), parse CODE_AGENT_REPOS and
//     clone each repo into /workspace/<name> with shell `git`. In shared
//     mode (CODE_AGENT_RUNTIME_MODE=shared) skip upfront cloning — the
//     orchestrator drives per-session clones via the admin HTTP below.
//  2. Write /home/worker/.config/opencode/opencode.json (from
//     CODE_AGENT_OPENCODE_CONFIG, or a minimal default pointing at
//     /workspace). Kept outside /workspace so the agent doesn't see it.
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
	"archive/tar"
	"compress/gzip"
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
	workspaceDir = "/workspace"
	// opencode config lives under the worker's HOME, outside /workspace, so
	// the agent doesn't see it via its file tools. Matches opencode's
	// standard global config path — set via OPENCODE_CONFIG env below.
	opencodeHome   = "/home/worker/.config/opencode"
	opencodeConfig = opencodeHome + "/opencode.json"
	skillsDir      = opencodeHome + "/skills"
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
	extID := os.Getenv("CODE_AGENT_EXTERNAL_ID")
	branchPrefix := envDefault("CODE_AGENT_BRANCH_PREFIX", "ai/")
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
			// After cloning the base branch, see whether an ai/<ticket>
			// branch already exists on the remote from a prior agent run.
			// If so, check it out so the agent sees prior work on disk
			// (and its new commits extend that branch rather than forking
			// a duplicate from base).
			if extID != "" && branchPrefix != "" {
				resumeAgentBranch(dest, r.Name, branchPrefix+extID)
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

	// Seed per-repo projects. Opencode's InstanceMiddleware auto-registers
	// a project the first time a request arrives with ?directory=... set
	// to a git-initialised worktree. We call GET /project/current once
	// per cloned repo so they all appear in the web UI project picker
	// without the user having to navigate. Fires in the background so
	// startup isn't blocked on opencode readiness.
	if eager {
		go seedRepoProjects(repos, ocPort)
	}

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
	mux.HandleFunc("/skills", handleSkills)

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

// ---- skills ----
//
// POST /skills — body is a tar archive (optionally gzip-compressed per
// Content-Encoding: gzip). Entries are extracted into
// /home/worker/.config/opencode/skills/, preserving the top-level
// bundle-dir structure (each bundle lives under <name>/SKILL.md + files).
//
// Idempotent overwrite per file — the orchestrator re-uploads on every
// stage, so later writes replace earlier ones. Path traversal (absolute
// paths, .. segments) is rejected.
func handleSkills(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", 405)
		return
	}
	defer r.Body.Close()
	var reader io.Reader = r.Body
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "gzip: "+err.Error(), 400)
			return
		}
		defer gz.Close()
		reader = gz
	}
	if err := extractSkillsTar(reader, skillsDir); err != nil {
		http.Error(w, "extract: "+err.Error(), 500)
		return
	}
	w.WriteHeader(204)
}

func extractSkillsTar(r io.Reader, dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean(h.Name)
		if clean == "." || clean == "" {
			continue
		}
		if strings.HasPrefix(clean, "..") || strings.HasPrefix(clean, string(filepath.Separator)) {
			return fmt.Errorf("unsafe path in tar: %s", h.Name)
		}
		target := filepath.Join(dest, clean)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		default:
			// Silently skip symlinks, devices, etc. Skill bundles should be
			// plain files + dirs only.
		}
	}
	return nil
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

// resumeAgentBranch checks whether the given branch exists on origin and,
// if it does, fetches + checks it out so prior agent work is on disk.
// All failures are warnings (not fatal) — the repo is usable from the
// base branch regardless, and we'd rather degrade than crash the pod.
func resumeAgentBranch(dest, repoName, branch string) {
	out, err := exec.Command("git", "-C", dest, "ls-remote", "--heads", "origin", branch).Output()
	if err != nil {
		logf("warn: ls-remote %s in %s: %v", branch, repoName, err)
		return
	}
	if strings.TrimSpace(string(out)) == "" {
		return // no prior agent branch
	}
	if err := runGit(dest, "fetch", "origin", branch); err != nil {
		logf("warn: fetch %s in %s: %v", branch, repoName, err)
		return
	}
	if err := runGit(dest, "checkout", "-B", branch, "origin/"+branch); err != nil {
		logf("warn: checkout %s in %s: %v", branch, repoName, err)
		return
	}
	logf("resumed prior agent branch %s in %s", branch, repoName)
}

// seedRepoProjects pings opencode /project/current?directory=<repo> for
// each cloned repo so the web UI's project picker shows them all. Retries
// a handful of times because opencode's HTTP server may not be ready
// immediately after spawn.
func seedRepoProjects(repos []repo, port string) {
	if len(repos) == 0 {
		return
	}
	base := "http://127.0.0.1:" + port
	client := &http.Client{Timeout: 5 * time.Second}
	// Wait for opencode to be reachable.
	for i := 0; i < 30; i++ {
		resp, err := client.Get(base + "/app")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode < 500 {
				break
			}
		}
		time.Sleep(time.Second)
	}
	for _, r := range repos {
		dir := filepath.Join(workspaceDir, r.Name)
		u := base + "/project/current?directory=" + url.QueryEscape(dir)
		resp, err := client.Get(u)
		if err != nil {
			logf("seed project %s: %v", r.Name, err)
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 300 {
			logf("seed project %s: HTTP %d", r.Name, resp.StatusCode)
			continue
		}
		logf("seeded opencode project: %s (dir=%s)", r.Name, dir)
	}
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

func writeOpenCodeConfig(_ []repo, _ bool) error {
	if err := os.MkdirAll(opencodeHome, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", opencodeHome, err)
	}
	if raw := os.Getenv("CODE_AGENT_OPENCODE_CONFIG"); raw != "" {
		if !json.Valid([]byte(raw)) {
			return fmt.Errorf("CODE_AGENT_OPENCODE_CONFIG is not valid JSON")
		}
		return os.WriteFile(opencodeConfig, []byte(raw), 0o644)
	}
	// No override → ship a minimal config with just the schema marker.
	// Filesystem scoping lives under `permission` in current opencode; the
	// old `folders` stanza was removed and now errors the server. Callers
	// that need provider keys or permission tuning should inject via
	// CODE_AGENT_OPENCODE_CONFIG.
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
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
