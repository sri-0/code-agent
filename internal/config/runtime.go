package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type RuntimesConfig struct {
	Runtimes map[string]Runtime `yaml:"runtimes"`
	// Git identity for commits made inside worker pods. Injected into
	// the pod as CODE_AGENT_GIT_USER_NAME / CODE_AGENT_GIT_USER_EMAIL,
	// which cmd/runner applies via `git config --global` at startup.
	Git GitIdentity `yaml:"git,omitempty"`
}

type GitIdentity struct {
	UserName  string `yaml:"user_name,omitempty"`
	UserEmail string `yaml:"user_email,omitempty"`
}

type Runtime struct {
	Mode      string     `yaml:"mode"` // local | ephemeral | persistent | shared
	Namespace string     `yaml:"namespace,omitempty"`
	Image     string     `yaml:"image,omitempty"`
	Resources *Resources `yaml:"resources,omitempty"`

	IdleTimeout time.Duration `yaml:"idle_timeout,omitempty"`
	Ingress     *Ingress      `yaml:"ingress,omitempty"`

	// local mode only
	OpenCodeURL      string `yaml:"opencode_url,omitempty"`
	OpenCodePassword EnvRef `yaml:"opencode_password,omitempty"`
	Workdir          string `yaml:"workdir,omitempty"`
}

type Resources struct {
	Requests ResourceList `yaml:"requests,omitempty"`
	Limits   ResourceList `yaml:"limits,omitempty"`
}

type ResourceList struct {
	CPU    string `yaml:"cpu,omitempty"`
	Memory string `yaml:"memory,omitempty"`
}

type Ingress struct {
	Enabled      bool   `yaml:"enabled"`
	HostTemplate string `yaml:"host_template,omitempty"`
	TLSSecret    string `yaml:"tls_secret,omitempty"`
	ClassName    string `yaml:"class_name,omitempty"`
}

func LoadRuntimes(path string) (*RuntimesConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg RuntimesConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	return &cfg, nil
}

func (c *RuntimesConfig) ByRef(ref string) (Runtime, bool) {
	r, ok := c.Runtimes[ref]
	return r, ok
}
