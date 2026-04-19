package server

import (
	"net/http"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"

	"code-agent/internal/config"
	"code-agent/internal/handler"
	"code-agent/internal/orchestrator"
	"code-agent/pkg/logging"
	"code-agent/pkg/tasks"
	"code-agent/pkg/transcript"
)

type Deps struct {
	Version    string
	Cfg        *config.Config
	Tasks      tasks.Store
	Transcript transcript.Store
	Orch       *orchestrator.Orchestrator
	Logger     zerolog.Logger
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

	// Prometheus metrics — no logging middleware noise.
	r.Handle("/metrics", promhttp.Handler()).Methods(http.MethodGet)

	if d.Orch != nil {
		r.Handle("/webhooks/{board}", &handler.WebhookHandler{
			Orch:   d.Orch,
			Logger: d.Logger,
		}).Methods(http.MethodPost)
	}

	// Operator API + dashboard. Mounted whenever we have config; handlers
	// degrade gracefully when Tasks/Transcript are nil (e.g. valkey down in
	// local dev) so /api/boards and the dashboard skeleton still work.
	if d.Cfg != nil {
		api := r.PathPrefix("/api").Subrouter()
		(&handler.APIHandler{
			Cfg:        d.Cfg,
			Tasks:      d.Tasks,
			Transcript: d.Transcript,
			Orch:       d.Orch,
			Logger:     d.Logger,
		}).Mount(api)

		dash := handler.NewDashboardHandler(d.Cfg, d.Tasks, d.Transcript, d.Logger)
		dash.Mount(r)
	}

	return r
}
