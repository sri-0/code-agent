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

	mu       sync.Mutex
	clients  map[string]*opencode.Client
	sessions map[string]string
	password string
}

func (r *Runtime) Mode() string { return "persistent" }

func (r *Runtime) EnsureWorker(ctx context.Context, t *tasks.Task, b config.Board) (*opencode.Client, *tasks.WorkerRef, error) {
	depName := depNameFor(t)
	r.mu.Lock()
	if c, ok := r.clients[t.ID]; ok {
		ref := r.refFor(t, b)
		r.mu.Unlock()
		return c, ref, nil
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

	url := fmt.Sprintf("http://%s.%s.svc.cluster.local:4096", depName, r.cfg.Namespace)
	client := opencode.New(opencode.Options{BaseURL: url, Password: r.password}, r.logger)
	if err := client.WaitForReady(ctx, 60*time.Second); err != nil {
		return nil, nil, fmt.Errorf("opencode ready: %w", err)
	}

	r.mu.Lock()
	r.clients[t.ID] = client
	r.mu.Unlock()

	ref := &tasks.WorkerRef{
		Mode:      "persistent",
		Namespace: r.cfg.Namespace,
		Kind:      "Deployment",
		Name:      depName,
		Pod:       pod,
		Service:   depName,
		URL:       url,
		UIURL:     uiURL(host),
		Password:  r.password,
		Ingress:   ingressNameIfEnabled(host, depName),
	}
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
	r.mu.Unlock()
	return r.k.DeleteDeployment(ctx, r.cfg.Namespace, dn)
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
		"OPENCODE_SERVER_PASSWORD": password,
		"CODE_AGENT_TASK_ID":       t.ID,
		"CODE_AGENT_BOARD_ID":      b.ID,
		"CODE_AGENT_EXTERNAL_ID":   t.ExternalID,
		"CODE_AGENT_RUNTIME_MODE":  mode,
	}
	if len(t.Repos) > 0 {
		env["CODE_AGENT_REPOS"] = reposCSV(t.Repos)
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
			name:     name,
			cfg:      cfg,
			logger:   deps.Logger.With().Str("runtime", "persistent").Str("name", name).Logger(),
			tx:       deps.Transcript,
			k:        kc,
			clients:  map[string]*opencode.Client{},
			sessions: map[string]string{},
			password: randPassword(),
		}, nil
	})
}
