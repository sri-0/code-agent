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
