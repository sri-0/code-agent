// Package all imports each concrete vcs provider for its init()
// registration.
package all

import (
	_ "code-agent/pkg/vcs/github"
	_ "code-agent/pkg/vcs/gitlab"
)
