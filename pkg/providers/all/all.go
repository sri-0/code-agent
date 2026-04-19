// Package all imports every concrete provider package for its side-effect
// init() registration. The orchestrator imports this once so it doesn't have
// to know about each provider explicitly.
package all

import (
	_ "code-agent/pkg/providers/clickup"
	_ "code-agent/pkg/providers/jira"
)
