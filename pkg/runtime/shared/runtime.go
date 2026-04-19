// Package shared implements runtime.Runtime with one Deployment per *board*
// that multiplexes many opencode sessions.
//
// Differences from persistent:
//   - Deployment is named per board, not per task (created lazily)
//   - Each task's repos are cloned into /workspace/<session_id>/<repo> by
//     calling the worker's admin HTTP (POST /sessions/{id}/setup). The pod
//     runs in CODE_AGENT_RUNTIME_MODE=shared so cmd/runner skips upfront
//     cloning.
//   - Cleanup removes only the per-session subdir, not the Deployment.
//
// The board Deployment lives forever — operators tear it down via the CLI.
package shared

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
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
)

type Runtime struct {
	name   string
	cfg    config.Runtime
	logger zerolog.Logger
	tx     transcript.Store
	k      *k8s.Client

	mu         sync.Mutex
	boardClnts map[string]*opencode.Client // board id -> client (shared)
	sessions   map[string]string           // task id -> session id
	password   string
}

func (r *Runtime) Mode() string { return "shared" }

func (r *Runtime) EnsureWorker(ctx context.Context, t *tasks.Task, b config.Board) (*opencode.Client, *tasks.WorkerRef, error) {
	r.mu.Lock()
	if c, ok := r.boardClnts[b.ID]; ok {
		ref := r.refFor(b)
		r.mu.Unlock()
		return c, ref, nil
	}
	r.mu.Unlock()

	depName := boardDepName(b)
	labels := map[string]string{
		"app":        "code-agent-worker",
		"board-id":   b.ID,
		"managed-by": "code-agent",
		"runtime":    "shared",
	}
	host, err := renderHost(r.cfg.Ingress, map[string]string{"BoardID": b.ID})
	if err != nil {
		return nil, nil, err
	}

	exists, err := r.k.DeploymentExists(ctx, r.cfg.Namespace, depName)
	if err != nil {
		return nil, nil, fmt.Errorf("check deployment: %w", err)
	}
	if !exists {
		env := map[string]string{
			"OPENCODE_SERVER_PASSWORD": r.password,
			"CODE_AGENT_BOARD_ID":      b.ID,
			"CODE_AGENT_RUNTIME_MODE":  "shared",
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
		r.logger.Info().Str("dep", depName).Str("ns", r.cfg.Namespace).Str("board", b.ID).Msg("shared deployment created")
	}

	if _, err := r.k.WaitForDeploymentReady(ctx, r.cfg.Namespace, depName, 5*time.Minute); err != nil {
		return nil, nil, fmt.Errorf("wait ready: %w", err)
	}

	url := fmt.Sprintf("http://%s.%s.svc.cluster.local:4096", depName, r.cfg.Namespace)
	client := opencode.New(opencode.Options{BaseURL: url, Password: r.password}, r.logger)
	if err := client.WaitForReady(ctx, 60*time.Second); err != nil {
		return nil, nil, fmt.Errorf("opencode ready: %w", err)
	}

	r.mu.Lock()
	r.boardClnts[b.ID] = client
	r.mu.Unlock()

	ref := &tasks.WorkerRef{
		Mode:      "shared",
		Namespace: r.cfg.Namespace,
		Kind:      "Deployment",
		Name:      depName,
		Service:   depName,
		URL:       url,
		UIURL:     uiURL(host),
		Password:  r.password,
		Ingress:   ingressNameIfEnabled(host, depName),
	}
	return client, ref, nil
}

func (r *Runtime) refFor(b config.Board) *tasks.WorkerRef {
	dn := boardDepName(b)
	host, _ := renderHost(r.cfg.Ingress, map[string]string{"BoardID": b.ID})
	return &tasks.WorkerRef{
		Mode:      "shared",
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

	var sid string
	if t.SessionID != "" {
		sid = t.SessionID
	} else {
		s, err := client.CreateSession(ctx, t.ExternalID+" "+t.Title)
		if err != nil {
			return "", err
		}
		sid = s.ID
	}

	// clone repos into /workspace/<sid>/<repo> via admin HTTP
	if len(t.Repos) > 0 {
		if err := r.callAdminSetup(ctx, t, sid); err != nil {
			return "", fmt.Errorf("setup session repos: %w", err)
		}
	}

	r.mu.Lock()
	r.sessions[t.ID] = sid
	r.mu.Unlock()
	if r.tx != nil {
		_ = r.tx.Init(ctx, transcript.Meta{SessionID: sid, TaskID: t.ID, BoardID: t.BoardID})
	}
	return sid, nil
}

func (r *Runtime) Cleanup(ctx context.Context, t *tasks.Task) error {
	r.mu.Lock()
	sid := r.sessions[t.ID]
	delete(r.sessions, t.ID)
	r.mu.Unlock()
	if sid == "" {
		return nil
	}
	// best-effort: find board id via task board id -> deployment name.
	depName := "ocshared-" + sanitize(t.BoardID)
	admin := fmt.Sprintf("http://%s.%s.svc.cluster.local:4100/sessions/%s", depName, r.cfg.Namespace, sid)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, admin, nil)
	if err != nil {
		return nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
	return nil
}

// ---- helpers ----

func (r *Runtime) callAdminSetup(ctx context.Context, t *tasks.Task, sid string) error {
	depName := "ocshared-" + sanitize(t.BoardID)
	admin := fmt.Sprintf("http://%s.%s.svc.cluster.local:4100/sessions/%s/setup", depName, r.cfg.Namespace, sid)

	type reqRepo struct {
		Name       string `json:"name"`
		URL        string `json:"url"`
		BaseBranch string `json:"base_branch"`
	}
	body := struct {
		Repos []reqRepo `json:"repos"`
	}{}
	for _, rp := range t.Repos {
		body.Repos = append(body.Repos, reqRepo{Name: rp.Name, URL: rp.URL, BaseBranch: rp.BaseBranch})
	}
	b, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, admin, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("admin setup: status %d", resp.StatusCode)
	}
	return nil
}

func boardDepName(b config.Board) string { return "ocshared-" + sanitize(b.ID) }

func sanitize(s string) string {
	// k8s names: lowercase letters, digits, '-'. Cheap coercion; callers
	// should use sensible board ids already.
	var out []rune
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+32)
		default:
			out = append(out, '-')
		}
	}
	// trim repeated dashes
	r := strings.ReplaceAll(string(out), "--", "-")
	r = strings.Trim(r, "-")
	if len(r) > 50 {
		r = r[:50]
	}
	return r
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
	runtime.Register("shared", func(name string, cfg config.Runtime, deps runtime.Deps) (runtime.Runtime, error) {
		kc, err := k8s.New(cfg.Namespace)
		if err != nil {
			return nil, fmt.Errorf("k8s client: %w", err)
		}
		return &Runtime{
			name:       name,
			cfg:        cfg,
			logger:     deps.Logger.With().Str("runtime", "shared").Str("name", name).Logger(),
			tx:         deps.Transcript,
			k:          kc,
			boardClnts: map[string]*opencode.Client{},
			sessions:   map[string]string{},
			password:   randPassword(),
		}, nil
	})
}
