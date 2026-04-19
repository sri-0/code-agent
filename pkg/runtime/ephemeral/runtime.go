// Package ephemeral implements runtime.Runtime as a per-task k8s Job.
//
// One Job (and headless Service) per task. The orchestrator talks to the
// worker via the cluster-internal DNS name. The Job lives until the task
// reaches a terminal stage; Cleanup deletes it.
package ephemeral

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
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
	clients  map[string]*opencode.Client // task id -> client
	sessions map[string]string
	password string
}

func (r *Runtime) Mode() string { return "ephemeral" }

// EnsureWorker creates a Job for the task if one isn't already running, then
// waits for the Pod to be Ready and returns a connected opencode client.
func (r *Runtime) EnsureWorker(ctx context.Context, t *tasks.Task, b config.Board) (*opencode.Client, *tasks.WorkerRef, error) {
	r.mu.Lock()
	if c, ok := r.clients[t.ID]; ok {
		ref := r.refFor(t)
		r.mu.Unlock()
		return c, ref, nil
	}
	r.mu.Unlock()

	jobName := jobNameFor(t)
	labels := map[string]string{
		"app":        "code-agent-worker",
		"task-id":    t.ID,
		"board-id":   b.ID,
		"managed-by": "code-agent",
	}
	url := fmt.Sprintf("http://%s.%s.svc.cluster.local:4096", jobName, r.cfg.Namespace)

	env := map[string]string{
		"OPENCODE_SERVER_PASSWORD": r.password,
		"CODE_AGENT_TASK_ID":       t.ID,
		"CODE_AGENT_BOARD_ID":      b.ID,
		"CODE_AGENT_EXTERNAL_ID":   t.ExternalID,
	}
	// pass repos as comma-separated list of name=url@base_branch
	if len(t.Repos) > 0 {
		var s string
		for i, repo := range t.Repos {
			if i > 0 {
				s += ","
			}
			s += repo.Name + "=" + repo.URL + "@" + repo.BaseBranch
		}
		env["CODE_AGENT_REPOS"] = s
	}

	spec := k8s.JobSpec{
		Name:      jobName,
		Namespace: r.cfg.Namespace,
		Image:     r.cfg.Image,
		Labels:    labels,
		Env:       env,
		Port:      4096,
	}
	if r.cfg.Resources != nil {
		spec.CPURequest = r.cfg.Resources.Requests.CPU
		spec.MemRequest = r.cfg.Resources.Requests.Memory
		spec.CPULimit = r.cfg.Resources.Limits.CPU
		spec.MemLimit = r.cfg.Resources.Limits.Memory
	}

	if err := r.k.CreateJob(ctx, spec); err != nil {
		return nil, nil, fmt.Errorf("create job: %w", err)
	}
	r.logger.Info().Str("job", jobName).Str("ns", r.cfg.Namespace).Msg("ephemeral job created")

	pod, err := r.k.WaitForJobPodReady(ctx, r.cfg.Namespace, jobName, 5*time.Minute)
	if err != nil {
		return nil, nil, fmt.Errorf("wait pod ready: %w", err)
	}

	client := opencode.New(opencode.Options{BaseURL: url, Password: r.password}, r.logger)
	if err := client.WaitForReady(ctx, 60*time.Second); err != nil {
		return nil, nil, fmt.Errorf("opencode ready: %w", err)
	}

	r.mu.Lock()
	r.clients[t.ID] = client
	ref := &tasks.WorkerRef{
		Mode:      "ephemeral",
		Namespace: r.cfg.Namespace,
		Kind:      "Job",
		Name:      jobName,
		Pod:       pod,
		Service:   jobName,
		URL:       url,
		Password:  r.password,
	}
	r.mu.Unlock()
	return client, ref, nil
}

func (r *Runtime) refFor(t *tasks.Task) *tasks.WorkerRef {
	jn := jobNameFor(t)
	return &tasks.WorkerRef{
		Mode:      "ephemeral",
		Namespace: r.cfg.Namespace,
		Kind:      "Job",
		Name:      jn,
		Service:   jn,
		URL:       fmt.Sprintf("http://%s.%s.svc.cluster.local:4096", jn, r.cfg.Namespace),
		Password:  r.password,
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
	jn := jobNameFor(t)
	r.mu.Lock()
	delete(r.clients, t.ID)
	delete(r.sessions, t.ID)
	r.mu.Unlock()
	return r.k.DeleteJob(ctx, r.cfg.Namespace, jn)
}

func jobNameFor(t *tasks.Task) string {
	// k8s names: 63 chars max, lowercase. Truncate task id.
	id := t.ID
	if len(id) > 16 {
		id = id[:16]
	}
	return "ocworker-" + id
}

func init() {
	runtime.Register("ephemeral", func(name string, cfg config.Runtime, deps runtime.Deps) (runtime.Runtime, error) {
		kc, err := k8s.New(cfg.Namespace)
		if err != nil {
			return nil, fmt.Errorf("k8s client: %w", err)
		}
		return &Runtime{
			name:     name,
			cfg:      cfg,
			logger:   deps.Logger.With().Str("runtime", "ephemeral").Str("name", name).Logger(),
			tx:       deps.Transcript,
			k:        kc,
			clients:  map[string]*opencode.Client{},
			sessions: map[string]string{},
			password: randPassword(),
		}, nil
	})
}

func randPassword() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
