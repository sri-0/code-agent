package server

import (
	"net/http"

	"code-agent/internal/handler"
	"code-agent/pkg/logging"
	"code-agent/pkg/tasks"

	"github.com/gorilla/mux"
	"github.com/rs/zerolog"
)

type Deps struct {
	Version string
	Tasks   tasks.Store
	Logger  zerolog.Logger
}

// NewRouter builds the top-level HTTP router.
func NewRouter(d Deps) http.Handler {
	r := mux.NewRouter()
	r.Use(logging.CreateLoggingMiddleware(d.Logger))

	r.Handle("/health", &handler.HealthHandler{
		Version: d.Version,
		Tasks:   d.Tasks,
		Logger:  d.Logger,
	}).Methods(http.MethodGet)

	return r
}
