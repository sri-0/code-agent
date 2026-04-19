package handler

import (
	"encoding/json"
	"net/http"

	"code-agent/pkg/tasks"

	"github.com/rs/zerolog"
)

type HealthHandler struct {
	Version string
	Tasks   tasks.Store
	Logger  zerolog.Logger
}

type HealthResponse struct {
	Healthy bool   `json:"healthy"`
	Version string `json:"version"`
	Tasks   bool   `json:"tasks_store"`
}

func (h *HealthHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	resp := HealthResponse{
		Healthy: true,
		Version: h.Version,
		Tasks:   h.Tasks != nil,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
