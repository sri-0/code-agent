// Package tasks defines the task data model used by the orchestrator and
// provides a storage interface with swappable backends (currently: valkey).
package tasks

import "time"

// Task is the canonical record of a board ticket the orchestrator is
// tracking. It is persisted between stage transitions so restarts are safe.
type Task struct {
	ID          string `json:"id"`          // internal uuid
	BoardID     string `json:"board_id"`    // ref into boards.yaml
	ExternalID  string `json:"external_id"` // e.g. "ALPHA-123"
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	URL         string `json:"url,omitempty"` // link back to the ticket

	Stage        string    `json:"stage"` // current stage name
	StageStarted time.Time `json:"stage_started"`
	Retries      int       `json:"retries"`

	RuntimeMode string    `json:"runtime_mode"` // local | ephemeral | persistent | shared
	WorkerRef   WorkerRef `json:"worker_ref"`
	SessionID   string    `json:"session_id,omitempty"` // opencode session

	Repos []RepoState `json:"repos"`

	MergeRequests []MergeRef `json:"merge_requests,omitempty"`

	LastEventKey string    `json:"last_event_key"` // dedupe marker (updated_at or event id)
	LastSync     time.Time `json:"last_sync"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// WorkerRef identifies where a worker is running. Fields are mode-dependent:
// - local: URL + Password
// - ephemeral: Namespace + JobName + Pod + URL (after ready)
// - persistent: Namespace + DeploymentName + Service + Ingress + URL
// - shared: Namespace + DeploymentName + Service + Ingress + URL (shared)
type WorkerRef struct {
	Mode      string `json:"mode"`
	Namespace string `json:"namespace,omitempty"`
	Kind      string `json:"kind,omitempty"` // Job | Deployment
	Name      string `json:"name,omitempty"`
	Pod       string `json:"pod,omitempty"`
	Service   string `json:"service,omitempty"`
	Ingress   string `json:"ingress,omitempty"`
	URL       string `json:"url,omitempty"`      // opencode server URL
	UIURL     string `json:"ui_url,omitempty"`   // user-facing URL
	Password  string `json:"password,omitempty"` // OPENCODE_SERVER_PASSWORD (not logged)
}

type RepoState struct {
	Name       string `json:"name"` // boards.yaml repo name
	URL        string `json:"url"`
	BaseBranch string `json:"base_branch"`
	Branch     string `json:"branch"` // branch_prefix + external_id
	HasChanges bool   `json:"has_changes"`
	Pushed     bool   `json:"pushed"`
}

type MergeRef struct {
	Repo   string `json:"repo"`
	URL    string `json:"url"`
	IID    int    `json:"iid,omitempty"`    // gitlab
	Number int    `json:"number,omitempty"` // github
	State  string `json:"state"`            // open | merged | closed
}
