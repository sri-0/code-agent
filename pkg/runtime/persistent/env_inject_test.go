package persistent

import (
	"testing"

	"code-agent/internal/config"
)

func TestBuildRepoSecretMounts(t *testing.T) {
	b := config.Board{
		Repos: []config.BoardRepo{
			{
				Name:       "risky-coreapi",
				EnvRef:     "secret://code-agent-env-risky-coreapi",
				ExtraFiles: []string{"auth_service_public.pem"},
			},
			{
				Name:   "risky-live-app",
				EnvRef: "secret://code-agent-env-risky-live-app",
			},
			{
				Name: "no-secrets",
			},
		},
	}
	env := map[string]string{}
	mounts := buildRepoSecretMounts(b, env)
	if len(mounts) != 2 {
		t.Fatalf("got %d mounts, want 2", len(mounts))
	}
	if mounts[0].SecretName != "code-agent-env-risky-coreapi" {
		t.Errorf("secret name = %q", mounts[0].SecretName)
	}
	if mounts[0].MountPath != "/secrets/risky-coreapi" {
		t.Errorf("mount path = %q", mounts[0].MountPath)
	}
	if env["CODE_AGENT_REPO_ENV_RISKY_COREAPI"] != "/secrets/risky-coreapi/.env" {
		t.Errorf("env path = %q", env["CODE_AGENT_REPO_ENV_RISKY_COREAPI"])
	}
	if env["CODE_AGENT_REPO_EXTRAS_RISKY_COREAPI"] != "auth_service_public.pem" {
		t.Errorf("extras = %q", env["CODE_AGENT_REPO_EXTRAS_RISKY_COREAPI"])
	}
	if _, dashHyphen := env["CODE_AGENT_REPO_ENV_RISKY_LIVE_APP"]; !dashHyphen {
		t.Errorf("dash→underscore conversion failed")
	}
}

func TestParseSecretRef(t *testing.T) {
	tests := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"secret://my-secret", "my-secret", true},
		{"secret://", "", false},
		{"file:///etc/.env", "", false},
		{"", "", false},
		{"my-secret", "", false},
	}
	for _, tc := range tests {
		got, ok := parseSecretRef(tc.in)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("parseSecretRef(%q) = (%q, %v), want (%q, %v)",
				tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}
