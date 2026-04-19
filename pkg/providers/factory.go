package providers

import (
	"fmt"

	"github.com/rs/zerolog"

	"code-agent/internal/config"
)

// Factory builds a BoardProvider for the given board config.
//
// We use a function variable rather than direct imports so the providers
// package doesn't depend on each concrete provider package (avoids import
// cycles). Concrete packages register themselves via Register in init().
type Factory func(b config.Board, logger zerolog.Logger, disp *Dispatcher) (BoardProvider, error)

var registry = map[string]Factory{}

// Register a provider factory under a name (e.g. "jira", "clickup").
// Safe to call from package init().
func Register(name string, f Factory) {
	registry[name] = f
}

// Build constructs a provider for the board, dispatching on b.Provider.
func Build(b config.Board, logger zerolog.Logger, disp *Dispatcher) (BoardProvider, error) {
	f, ok := registry[b.Provider]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q for board %s", b.Provider, b.ID)
	}
	return f(b, logger, disp)
}
