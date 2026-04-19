// Package all imports each concrete runtime package for its init()
// registration. Concrete runtimes are built lazily by runtime.Build, so
// importing them here is enough to make every mode addressable from
// config.
package all

import (
	_ "code-agent/pkg/runtime/ephemeral"
	_ "code-agent/pkg/runtime/local"
)
