package server

import (
	"net/http"

	"code-agent/internal/handler"
	"code-agent/internal/orchestrator"
	"code-agent/pkg/logging"
	"code-agent/pkg/tasks"

	"github.com/gorilla/mux"
	"github.com/rs/zerolog"
)

type Deps struct {
	Version string
	Tasks   tasks.Store
	Orch    *orchestrator.Orchestrator
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

	if d.Orch != nil {
		r.Handle("/webhooks/{board}", &handler.WebhookHandler{
			Orch:   d.Orch,
			Logger: d.Logger,
		}).Methods(http.MethodPost)
	}

	return r
}
