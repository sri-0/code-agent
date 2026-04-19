// Package all imports each concrete runtime package for its init()
// registration. Phase 4 ships only `local`; later phases add the rest.
package all

import (
	_ "code-agent/pkg/runtime/local"
)
