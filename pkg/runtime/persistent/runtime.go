// Package persistent implements runtime.Runtime with one long-running
// Deployment per task. A task's worker is kept alive across stage transitions
// so a human can SSH into its opencode UI (via Ingress) to review work.
//
// Created lazily on EnsureWorker; torn down in Cleanup (called by the stage
// engine when a task reaches a terminal stage). An optional idle goroutine
// can GC workers whose tasks have been silent too long.
package persistent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/rs/zerolog"

	"code-agent/internal/config"
	"code-agent/pkg/k8s"
	"code-agent/pkg/opencode"
	"code-agent/pkg/runtime"
	"code-agent/pkg/tasks"
	"code-agent/pkg/transcript"
	"code-agent/pkg/vcs"
)

type Runtime struct {
	name   string
	cfg    config.Runtime
	logger zerolog.Logger
	tx     transcript.Store
	k      *k8s.Client

	openCode config.OpenCodeConfig
	git      config.GitIdentity

	// GC config — see Deps.
	taskStore        tasks.Store
	boards           *config.BoardsConfig
	ttlAfterMRClosed time.Duration
	ttlMax           time.Duration
	gcInterval       time.Duration

	mu       sync.Mutex
	clients  map[string]*opencode.Client
	sessions map[string]string
	forwards map[string]*k8s.PortForward // task id -> tunnel (out-of-cluster only)
	refs     map[string]*tasks.WorkerRef // task id -> cached full ref (includes AdminURL, port-forward URLs)
	// podBirth is when each pod was first created. Used by the GC to
	// enforce the absolute-max lifetime regardless of MR state.
	podBirth map[string]time.Time
	password string
	// gcOnce + gcDone let us start exactly one GC goroutine across the
	// orchestrator's lifetime.
	gcOnce sync.Once
}

func (r *Runtime) Mode() string { return "persistent" }

func (r *Runtime) EnsureWorker(ctx context.Context, t *tasks.Task, b config.Board) (*opencode.Client, *tasks.WorkerRef, error) {
	depName := depNameFor(t)
	r.mu.Lock()
	if c, ok := r.clients[t.ID]; ok {
		ref := r.refs[t.ID]
		r.mu.Unlock()
		if ref != nil {
			return c, ref, nil
		}
		// fallback shouldn't normally happen; rebuild without port-forward info
		return c, r.refFor(t, b), nil
	}
	r.mu.Unlock()

	exists, err := r.k.DeploymentExists(ctx, r.cfg.Namespace, depName)
	if err != nil {
		return nil, nil, fmt.Errorf("check deployment: %w", err)
	}
	labels := map[string]string{
		"app":        "code-agent-worker",
		"task-id":    t.ID,
		"board-id":   b.ID,
		"managed-by": "code-agent",
		"runtime":    "persistent",
	}

	host, err := renderHost(r.cfg.Ingress, map[string]string{
		"TaskID":  t.ID,
		"BoardID": b.ID,
		"ExtID":   t.ExternalID,
	})
	if err != nil {
		return nil, nil, err
	}

	if !exists {
		env := baseEnv(r.password, t, b, "persistent")
		// Git identity for commits the runner makes on behalf of agents.
		if r.git.UserName != "" {
			env["CODE_AGENT_GIT_USER_NAME"] = r.git.UserName
		}
		if r.git.UserEmail != "" {
			env["CODE_AGENT_GIT_USER_EMAIL"] = r.git.UserEmail
		}
		// Inject opencode.json (as compact JSON in env) if the orchestrator
		// was given one. cmd/runner writes this to /workspace/opencode.json
		// before launching opencode serve. The config includes provider
		// API keys inline (matches the user's local opencode.json shape).
		if r.openCode != nil {
			if j, err := r.openCode.JSON(); err != nil {
				return nil, nil, fmt.Errorf("marshal opencode config: %w", err)
			} else if j != nil {
				env["CODE_AGENT_OPENCODE_CONFIG"] = string(j)
			}
		}
		spec := k8s.DeploymentSpec{
			Name:      depName,
			Namespace: r.cfg.Namespace,
			Image:     r.cfg.Image,
			Labels:    labels,
			Env:       env,
			Port:      4096,
			AdminPort: 4100,
		}
		if r.cfg.Resources != nil {
			spec.CPURequest = r.cfg.Resources.Requests.CPU
			spec.MemRequest = r.cfg.Resources.Requests.Memory
			spec.CPULimit = r.cfg.Resources.Limits.CPU
			spec.MemLimit = r.cfg.Resources.Limits.Memory
		}
		if host != "" {
			spec.IngressHost = host
			if r.cfg.Ingress != nil {
				spec.IngressTLSSecret = r.cfg.Ingress.TLSSecret
				spec.IngressClassName = r.cfg.Ingress.ClassName
			}
		}
		if err := r.k.CreateDeployment(ctx, spec); err != nil {
			return nil, nil, fmt.Errorf("create deployment: %w", err)
		}
		r.logger.Info().Str("dep", depName).Str("ns", r.cfg.Namespace).Msg("persistent deployment created")
	}

	pod, err := r.k.WaitForDeploymentReady(ctx, r.cfg.Namespace, depName, 5*time.Minute)
	if err != nil {
		return nil, nil, fmt.Errorf("wait ready: %w", err)
	}

	// URL selection:
	//   - In-cluster: use the Service DNS (fastest, always routable).
	//   - Out-of-cluster (dev laptop): SPDY port-forward to the pod via
	//     the apiserver. We forward both 4096 (opencode) and 4100 (admin)
	//     so the orchestrator can drive git ops via cmd/runner admin HTTP.
	var (
		openURL  string
		adminURL string
		fwd      *k8s.PortForward
	)
	if r.k.InCluster {
		openURL = fmt.Sprintf("http://%s.%s.svc.cluster.local:4096", depName, r.cfg.Namespace)
		adminURL = fmt.Sprintf("http://%s.%s.svc.cluster.local:4100", depName, r.cfg.Namespace)
	} else {
		pf, err := r.k.ForwardPodPorts(ctx, r.cfg.Namespace, pod, []int{4096, 4100})
		if err != nil {
			return nil, nil, fmt.Errorf("port-forward: %w", err)
		}
		fwd = pf
		openURL = fmt.Sprintf("http://localhost:%d", pf.LocalPorts[4096])
		adminURL = fmt.Sprintf("http://localhost:%d", pf.LocalPorts[4100])
		r.logger.Info().
			Str("pod", pod).
			Int("opencode_port", pf.LocalPorts[4096]).
			Int("admin_port", pf.LocalPorts[4100]).
			Msg("port-forward established")
	}
	client := opencode.New(opencode.Options{BaseURL: openURL, Password: r.password}, r.logger)
	if err := client.WaitForReady(ctx, 60*time.Second); err != nil {
		if fwd != nil {
			fwd.Close()
		}
		return nil, nil, fmt.Errorf("opencode ready: %w", err)
	}

	r.mu.Lock()
	r.clients[t.ID] = client
	if fwd != nil {
		r.forwards[t.ID] = fwd
	}
	if _, ok := r.podBirth[t.ID]; !ok {
		r.podBirth[t.ID] = time.Now()
	}
	// Cache the ref too — subsequent EnsureWorker calls return the same
	// URL/AdminURL so downstream actions (open_mr) reach the pod via the
	// port-forward, not an unreachable svc.cluster.local DNS name.
	ref := &tasks.WorkerRef{
		Mode:      "persistent",
		Namespace: r.cfg.Namespace,
		Kind:      "Deployment",
		Name:      depName,
		Pod:       pod,
		Service:   depName,
		URL:       openURL,
		AdminURL:  adminURL,
		UIURL:     uiURL(host),
		Password:  r.password,
		Ingress:   ingressNameIfEnabled(host, depName),
	}
	r.refs[t.ID] = ref
	r.mu.Unlock()
	return client, ref, nil
}

func (r *Runtime) refFor(t *tasks.Task, b config.Board) *tasks.WorkerRef {
	dn := depNameFor(t)
	host, _ := renderHost(r.cfg.Ingress, map[string]string{
		"TaskID":  t.ID,
		"BoardID": b.ID,
		"ExtID":   t.ExternalID,
	})
	return &tasks.WorkerRef{
		Mode:      "persistent",
		Namespace: r.cfg.Namespace,
		Kind:      "Deployment",
		Name:      dn,
		Service:   dn,
		URL:       fmt.Sprintf("http://%s.%s.svc.cluster.local:4096", dn, r.cfg.Namespace),
		UIURL:     uiURL(host),
		Password:  r.password,
		Ingress:   ingressNameIfEnabled(host, dn),
	}
}

func (r *Runtime) EnsureSession(ctx context.Context, client *opencode.Client, t *tasks.Task) (string, error) {
	r.mu.Lock()
	if sid, ok := r.sessions[t.ID]; ok {
		r.mu.Unlock()
		return sid, nil
	}
	r.mu.Unlock()
	if t.SessionID != "" {
		r.mu.Lock()
		r.sessions[t.ID] = t.SessionID
		r.mu.Unlock()
		return t.SessionID, nil
	}
	s, err := client.CreateSession(ctx, t.ExternalID+" "+t.Title)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	r.sessions[t.ID] = s.ID
	r.mu.Unlock()
	if r.tx != nil {
		_ = r.tx.Init(ctx, transcript.Meta{SessionID: s.ID, TaskID: t.ID, BoardID: t.BoardID})
	}
	return s.ID, nil
}

func (r *Runtime) Cleanup(ctx context.Context, t *tasks.Task) error {
	dn := depNameFor(t)
	r.mu.Lock()
	delete(r.clients, t.ID)
	delete(r.sessions, t.ID)
	delete(r.refs, t.ID)
	delete(r.podBirth, t.ID)
	if fwd, ok := r.forwards[t.ID]; ok {
		fwd.Close()
		delete(r.forwards, t.ID)
	}
	r.mu.Unlock()
	return r.k.DeleteDeployment(ctx, r.cfg.Namespace, dn)
}

// StartGC launches the idle-pod garbage collector goroutine. Safe to call
// multiple times — the first wins, subsequent calls are no-ops. The
// goroutine runs until ctx is cancelled.
//
// GC policy:
//  1. For each live pod (in r.refs), load its task from the store.
//  2. Age 1 — absolute max: pod age > ttlMax → Cleanup (catches runaways).
//  3. Age 2 — all MRs closed/merged + (now - latest close) > ttlAfterMRClosed → Cleanup.
//     MR state is refreshed from the VCS provider on each scan, and the
//     task record gets its ClosedAt/MergedAt populated for observability.
//  4. No MRs on the task yet → covered by ttlMax only. Lets plan/implement
//     stages complete without the GC prematurely reaping them.
func (r *Runtime) StartGC(ctx context.Context) {
	r.gcOnce.Do(func() {
		if r.taskStore == nil {
			r.logger.Info().Msg("persistent GC disabled (no task store)")
			return
		}
		interval := r.gcInterval
		if interval <= 0 {
			interval = 10 * time.Minute
		}
		r.logger.Info().
			Dur("interval", interval).
			Dur("ttl_after_mr_closed", r.ttlAfterMRClosed).
			Dur("ttl_max", r.ttlMax).
			Msg("persistent GC starting")
		go func() {
			t := time.NewTicker(interval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					r.gcTick(ctx)
				}
			}
		}()
	})
}

func (r *Runtime) gcTick(ctx context.Context) {
	r.mu.Lock()
	taskIDs := make([]string, 0, len(r.refs))
	births := make(map[string]time.Time, len(r.podBirth))
	for id := range r.refs {
		taskIDs = append(taskIDs, id)
		births[id] = r.podBirth[id]
	}
	r.mu.Unlock()

	if len(taskIDs) == 0 {
		return
	}
	now := time.Now()
	for _, tid := range taskIDs {
		t, err := r.taskStore.Get(ctx, tid)
		if err != nil || t == nil {
			// Task record gone but pod still around — reap it anyway.
			r.logger.Warn().Str("task", tid).Err(err).Msg("GC: task record missing; cleaning pod")
			r.forceCleanup(ctx, tid)
			continue
		}
		// Rule 1: absolute max lifetime.
		if r.ttlMax > 0 {
			birth, ok := births[tid]
			if ok && now.Sub(birth) > r.ttlMax {
				r.logger.Info().Str("task", tid).Dur("age", now.Sub(birth)).Msg("GC: pod exceeded ttl_max; cleaning up")
				_ = r.Cleanup(ctx, t)
				continue
			}
		}
		// Rule 2: MR-closed grace.
		if len(t.MergeRequests) > 0 && r.ttlAfterMRClosed > 0 {
			latestClose, allClosed := r.refreshMRStates(ctx, t)
			if allClosed && !latestClose.IsZero() && now.Sub(latestClose) > r.ttlAfterMRClosed {
				r.logger.Info().
					Str("task", tid).
					Time("latest_close", latestClose).
					Msg("GC: all MRs closed past grace period; cleaning up")
				_ = r.Cleanup(ctx, t)
				continue
			}
		}
	}
}

// refreshMRStates queries each MR on the task via VCS, updates the task
// record in-place, and returns (latestCloseTime, allClosedOrMerged).
// Any refresh error leaves that MR's state untouched and the overall
// allClosed flag becomes false.
func (r *Runtime) refreshMRStates(ctx context.Context, t *tasks.Task) (time.Time, bool) {
	if r.boards == nil {
		return time.Time{}, false
	}
	board, ok := r.boards.ByID(t.BoardID)
	if !ok {
		return time.Time{}, false
	}
	dirty := false
	allClosed := true
	var latest time.Time
	for i, mr := range t.MergeRequests {
		br, ok := findBoardRepo(board, mr.Repo)
		if !ok {
			allClosed = false
			continue
		}
		client, err := vcs.Build(br)
		if err != nil {
			r.logger.Debug().Err(err).Str("repo", mr.Repo).Msg("GC: vcs.Build failed; skipping MR")
			allClosed = false
			continue
		}
		refreshCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		fresh, err := client.GetMR(refreshCtx, vcs.MergeRequest{Repo: mr.Repo, IID: mr.IID, Number: mr.Number})
		cancel()
		if err != nil {
			r.logger.Debug().Err(err).Str("repo", mr.Repo).Msg("GC: GetMR failed; skipping")
			allClosed = false
			continue
		}
		if fresh.State != mr.State {
			dirty = true
		}
		t.MergeRequests[i].State = fresh.State
		if fresh.ClosedAt != nil {
			t.MergeRequests[i].ClosedAt = fresh.ClosedAt
			if fresh.ClosedAt.After(latest) {
				latest = *fresh.ClosedAt
			}
		}
		if fresh.MergedAt != nil {
			t.MergeRequests[i].MergedAt = fresh.MergedAt
		}
		if fresh.State != "closed" && fresh.State != "merged" {
			allClosed = false
		}
	}
	if dirty {
		_ = r.taskStore.Update(ctx, t)
	}
	return latest, allClosed
}

func (r *Runtime) forceCleanup(ctx context.Context, taskID string) {
	r.mu.Lock()
	ref := r.refs[taskID]
	delete(r.clients, taskID)
	delete(r.sessions, taskID)
	delete(r.refs, taskID)
	delete(r.podBirth, taskID)
	if fwd, ok := r.forwards[taskID]; ok {
		fwd.Close()
		delete(r.forwards, taskID)
	}
	r.mu.Unlock()
	if ref != nil && ref.Name != "" {
		_ = r.k.DeleteDeployment(ctx, r.cfg.Namespace, ref.Name)
	}
}

func findBoardRepo(b config.Board, name string) (config.BoardRepo, bool) {
	for _, r := range b.Repos {
		if r.Name == name {
			return r, true
		}
	}
	return config.BoardRepo{}, false
}

// ---- helpers ----

func depNameFor(t *tasks.Task) string {
	id := t.ID
	if len(id) > 16 {
		id = id[:16]
	}
	return "ocworker-" + id
}

func baseEnv(password string, t *tasks.Task, b config.Board, mode string) map[string]string {
	env := map[string]string{
		"CODE_AGENT_TASK_ID":      t.ID,
		"CODE_AGENT_BOARD_ID":     b.ID,
		"CODE_AGENT_EXTERNAL_ID":  t.ExternalID,
		"CODE_AGENT_RUNTIME_MODE": mode,
	}
	// Password-protect only if a non-empty one was passed. For in-cluster
	// use we leave it unset — /app must be reachable unauthenticated for
	// the kubelet readiness probe to succeed. The pod's port is only
	// reachable from inside the cluster or via the orchestrator's
	// port-forward, so the exposure is bounded.
	if password != "" {
		env["OPENCODE_SERVER_PASSWORD"] = password
	}
	if len(t.Repos) > 0 {
		env["CODE_AGENT_REPOS"] = reposCSV(t.Repos)
	}
	// Lets cmd/runner resume an ai/<ticket> branch from a prior run.
	if b.BranchPrefix != "" {
		env["CODE_AGENT_BRANCH_PREFIX"] = b.BranchPrefix
	}
	// Per-repo git auth: cmd/runner looks for CODE_AGENT_GIT_TOKEN_<NAME>
	// (uppercase, dashes -> underscores) and injects the token as HTTP
	// basic auth in the clone URL. The orchestrator reads the token's
	// value from its own env (the one named in boards.yaml vcs.auth.env)
	// and ships it into the pod.
	for _, br := range b.Repos {
		if br.VCS.Auth.Env == "" {
			continue
		}
		tok := os.Getenv(br.VCS.Auth.Env)
		if tok == "" {
			continue
		}
		key := "CODE_AGENT_GIT_TOKEN_" + strings.ToUpper(strings.ReplaceAll(br.Name, "-", "_"))
		env[key] = tok
	}
	return env
}

func reposCSV(repos []tasks.RepoState) string {
	var b bytes.Buffer
	for i, r := range repos {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(r.Name)
		b.WriteByte('=')
		b.WriteString(r.URL)
		b.WriteByte('@')
		b.WriteString(r.BaseBranch)
	}
	return b.String()
}

func renderHost(ing *config.Ingress, vars map[string]string) (string, error) {
	if ing == nil || !ing.Enabled || ing.HostTemplate == "" {
		return "", nil
	}
	tmpl, err := template.New("host").Parse(ing.HostTemplate)
	if err != nil {
		return "", fmt.Errorf("host template: %w", err)
	}
	var b bytes.Buffer
	if err := tmpl.Execute(&b, vars); err != nil {
		return "", fmt.Errorf("render host: %w", err)
	}
	return b.String(), nil
}

func uiURL(host string) string {
	if host == "" {
		return ""
	}
	return "https://" + host
}

func ingressNameIfEnabled(host, name string) string {
	if host == "" {
		return ""
	}
	return name
}

func randPassword() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func init() {
	runtime.Register("persistent", func(name string, cfg config.Runtime, deps runtime.Deps) (runtime.Runtime, error) {
		kc, err := k8s.New(cfg.Namespace)
		if err != nil {
			return nil, fmt.Errorf("k8s client: %w", err)
		}
		return &Runtime{
			name:             name,
			cfg:              cfg,
			logger:           deps.Logger.With().Str("runtime", "persistent").Str("name", name).Logger(),
			tx:               deps.Transcript,
			k:                kc,
			openCode:         deps.OpenCode,
			git:              deps.Git,
			taskStore:        deps.Tasks,
			boards:           deps.Boards,
			ttlAfterMRClosed: deps.TTLAfterMRClosed,
			ttlMax:           deps.TTLMax,
			gcInterval:       deps.GCInterval,
			clients:          map[string]*opencode.Client{},
			sessions:         map[string]string{},
			forwards:         map[string]*k8s.PortForward{},
			refs:             map[string]*tasks.WorkerRef{},
			podBirth:         map[string]time.Time{},
			password:         "", // no opencode auth for in-cluster use; probe-friendly
		}, nil
	})
}
