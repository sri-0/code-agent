package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type BoardsConfig struct {
	Boards []Board `yaml:"boards"`
}

type Board struct {
	ID           string        `yaml:"id"`
	Provider     string        `yaml:"provider"` // jira | clickup
	URL          string        `yaml:"url"`
	ProjectKey   string        `yaml:"project_key,omitempty"`  // jira
	WorkspaceID  string        `yaml:"workspace_id,omitempty"` // clickup
	SpaceID      string        `yaml:"space_id,omitempty"`     // clickup
	ListIDs      []string      `yaml:"list_ids,omitempty"`     // clickup
	Auth         EnvRef        `yaml:"auth"`
	Filters      BoardFilters  `yaml:"filters"`
	Triggers     BoardTriggers `yaml:"triggers"`
	StagesRef    string        `yaml:"stages_ref"`
	RuntimeRef   string        `yaml:"runtime_ref"`
	Repos        []BoardRepo   `yaml:"repos"`
	BranchPrefix string        `yaml:"branch_prefix"`
	MRStrategy   string        `yaml:"mr_strategy"`
	// Skills lists bundle names to ship into every stage run on this board.
	// Bundle names resolve against the host skills dir (CODE_AGENT_SKILLS_DIR).
	// Per-stage skills are unioned on top of this list.
	Skills []string `yaml:"skills,omitempty"`

	// Reactions are auto-responses to lifecycle transitions on this
	// board's tasks (e.g. ci-failing → send-to-agent). Empty map means
	// rely on the orchestrator's per-stack defaults.
	Reactions map[string]ReactionConfig `yaml:"reactions,omitempty"`
}

// ReactionConfig is one declarative event-handler rule. Mirrors
// agent-orchestrator's `reactions:` block. The map key is the event id
// (e.g. "ci-failed", "changes-requested", "approved-and-green",
// "agent-stuck"); the engine resolves which event(s) trigger which rule.
type ReactionConfig struct {
	// Auto controls whether the action fires automatically. When false
	// the engine still runs the notify path so humans see the event.
	Auto *bool `yaml:"auto,omitempty"`
	// Action is "send-to-agent" | "notify" | "auto-merge".
	Action string `yaml:"action,omitempty"`
	// Retries is the max number of times the action can fire on the
	// same evidence (e.g. CI failed twice in a row → max 2 agent runs).
	Retries int `yaml:"retries,omitempty"`
	// EscalateAfter — duration ("30m") OR attempt count.
	// Parsed by the engine; we keep it as raw string to support both forms.
	EscalateAfter string `yaml:"escalate_after,omitempty"`
	// Threshold — for time-based events (e.g. agent-stuck after 10m).
	Threshold string `yaml:"threshold,omitempty"`
	// Priority — "urgent" | "action" | "warning" | "info". Drives notification routing.
	Priority string `yaml:"priority,omitempty"`
	// Method — for auto-merge, the merge method ("squash" | "merge" | "rebase").
	Method string `yaml:"method,omitempty"`
}

// AutoEnabled returns the effective auto value with the right default
// (true unless explicitly set to false).
func (r ReactionConfig) AutoEnabled() bool {
	if r.Auto == nil {
		return true
	}
	return *r.Auto
}

type EnvRef struct {
	Env string `yaml:"env"`
}

func (e EnvRef) Value() string { return os.Getenv(e.Env) }

type BoardFilters struct {
	Labels        []string `yaml:"labels,omitempty"`
	ExcludeLabels []string `yaml:"exclude_labels,omitempty"`
	Tags          []string `yaml:"tags,omitempty"`         // clickup
	ExcludeTags   []string `yaml:"exclude_tags,omitempty"` // clickup — skip tickets carrying any of these tags
}

type BoardTriggers struct {
	Webhook WebhookTrigger `yaml:"webhook"`
	Poll    PollTrigger    `yaml:"poll"`
}

type WebhookTrigger struct {
	Enabled bool   `yaml:"enabled"`
	Secret  EnvRef `yaml:"secret"`
}

type PollTrigger struct {
	Enabled  bool          `yaml:"enabled"`
	Interval time.Duration `yaml:"interval"`
}

type BoardRepo struct {
	Name       string  `yaml:"name"`
	URL        string  `yaml:"url"`
	BaseBranch string  `yaml:"base_branch"`
	VCS        RepoVCS `yaml:"vcs"`
	// EnvRef points at a per-repo `.env` file. Two schemes:
	//   file:///abs/path/to/.env   — local runtime, host-side path
	//   secret://<secret-name>     — persistent runtime, k8s Secret name
	//                                whose `.env` key contains the file
	// Empty means no env injected (current behaviour preserved).
	EnvRef string `yaml:"env_ref,omitempty"`
	// ExtraFiles names additional non-env secret files to project into
	// the workspace alongside `.env` (e.g. "auth_service_public.pem").
	// Resolved against the same Secret as EnvRef when EnvRef uses
	// secret://, or relative to the host file's directory for file://.
	ExtraFiles []string `yaml:"extra_files,omitempty"`
}

type RepoVCS struct {
	Provider string `yaml:"provider"` // gitlab | github
	Host     string `yaml:"host,omitempty"`
	Auth     EnvRef `yaml:"auth"`
}

func LoadBoards(path string) (*BoardsConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg BoardsConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	return &cfg, nil
}

func (c *BoardsConfig) ByID(id string) (Board, bool) {
	for _, b := range c.Boards {
		if b.ID == id {
			return b, true
		}
	}
	return Board{}, false
}
