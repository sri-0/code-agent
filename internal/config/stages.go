package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type StagesConfig struct {
	StageSets map[string][]Stage `yaml:"stage_sets"`
}

type Stage struct {
	Name           string        `yaml:"name"`
	ProviderStatus string        `yaml:"provider_status"`
	Action         string        `yaml:"action"`                 // noop | run_agent | open_mr | comment | comment_error | wait_for_review | run_shell
	RuntimeMode    string        `yaml:"runtime_mode,omitempty"` // override runtime for this stage
	SystemPrompt   string        `yaml:"system_prompt,omitempty"`
	// Opencode model selection for run_agent. Both optional: if empty,
	// opencode's default provider/model is used.
	Provider string `yaml:"provider,omitempty"` // e.g. "openrouter", "anthropic"
	Model    string `yaml:"model,omitempty"`    // e.g. "anthropic/claude-opus-4.7"
	// Agent is the opencode agent to invoke for this stage — e.g. "plan"
	// or "build". Maps to opencode's /session/{id}/prompt_async body
	// field `agent`. See https://opencode.ai/docs/server/#messages.
	Agent string `yaml:"agent,omitempty"`
	Next       string        `yaml:"next,omitempty"`     // for action=noop
	SuccessNext string       `yaml:"success_next,omitempty"`
	FailureNext string       `yaml:"failure_next,omitempty"`
	Terminal   bool          `yaml:"terminal,omitempty"`
	// HumanReview, when true, tells the engine to skip the automatic
	// transition to success_next on completion. The stage behaves as
	// terminal — a human must move the ticket to the next status manually.
	// Used to gate the plan -> implement transition so a human can approve
	// the plan first.
	HumanReview bool          `yaml:"human_review,omitempty"`
	// WritesPlan, when true, causes run_agent to write the stage's final
	// assistant message (verbatim) to the ticket description after the
	// stage completes. Used on the plan stage so the ticket description
	// becomes the source of truth for subsequent stages.
	WritesPlan bool           `yaml:"writes_plan,omitempty"`
	Timeout     time.Duration `yaml:"timeout,omitempty"`
	MaxRetries  int           `yaml:"max_retries,omitempty"`
	MRTemplate  string        `yaml:"mr_template,omitempty"` // for action=open_mr
}

func LoadStages(path string) (*StagesConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg StagesConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	return &cfg, nil
}

// Set returns a stage set by name.
func (c *StagesConfig) Set(name string) ([]Stage, bool) {
	s, ok := c.StageSets[name]
	return s, ok
}

// StageByName returns a stage within a set.
func (c *StagesConfig) StageByName(setName, stageName string) (Stage, bool) {
	set, ok := c.Set(setName)
	if !ok {
		return Stage{}, false
	}
	for _, s := range set {
		if s.Name == stageName {
			return s, true
		}
	}
	return Stage{}, false
}

// StageByProviderStatus finds the stage a given provider status maps to.
func (c *StagesConfig) StageByProviderStatus(setName, providerStatus string) (Stage, bool) {
	set, ok := c.Set(setName)
	if !ok {
		return Stage{}, false
	}
	for _, s := range set {
		if s.ProviderStatus == providerStatus {
			return s, true
		}
	}
	return Stage{}, false
}
