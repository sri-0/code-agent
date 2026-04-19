package handler

import (
	_ "embed"
	"html/template"
	"net/http"
	"sort"

	"github.com/gorilla/mux"
	"github.com/rs/zerolog"

	"code-agent/internal/config"
	"code-agent/pkg/tasks"
	"code-agent/pkg/transcript"
)

//go:embed templates/dashboard.html
var dashboardTmpl string

//go:embed templates/task.html
var taskTmpl string

// DashboardHandler serves a small read-only HTML view of the orchestrator
// state: list of tasks, per-task detail including worker ref + latest
// transcript events.
type DashboardHandler struct {
	Cfg        *config.Config
	Tasks      tasks.Store
	Transcript transcript.Store
	Logger     zerolog.Logger

	index *template.Template
	task  *template.Template
}

func NewDashboardHandler(cfg *config.Config, ts tasks.Store, tx transcript.Store, logger zerolog.Logger) *DashboardHandler {
	h := &DashboardHandler{Cfg: cfg, Tasks: ts, Transcript: tx, Logger: logger}
	h.index = template.Must(template.New("dashboard").Parse(dashboardTmpl))
	h.task = template.Must(template.New("task").Parse(taskTmpl))
	return h
}

func (h *DashboardHandler) Mount(r *mux.Router) {
	r.HandleFunc("/", h.index0).Methods(http.MethodGet)
	r.HandleFunc("/tasks/{id}", h.taskPage).Methods(http.MethodGet)
}

type dashboardData struct {
	Boards []config.Board
	Tasks  []*tasks.Task
}

func (h *DashboardHandler) index0(w http.ResponseWriter, r *http.Request) {
	var all []*tasks.Task
	if h.Tasks != nil && h.Cfg.Boards != nil {
		for _, b := range h.Cfg.Boards.Boards {
			list, err := h.Tasks.ListByBoard(r.Context(), b.ID)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			all = append(all, list...)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].UpdatedAt.After(all[j].UpdatedAt) })

	boards := []config.Board{}
	if h.Cfg.Boards != nil {
		boards = h.Cfg.Boards.Boards
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = h.index.Execute(w, dashboardData{Boards: boards, Tasks: all})
}

type taskData struct {
	Task   *tasks.Task
	Meta   transcript.Meta
	Events []string
}

func (h *DashboardHandler) taskPage(w http.ResponseWriter, r *http.Request) {
	if h.Tasks == nil {
		http.Error(w, "task store not configured", 503)
		return
	}
	id := mux.Vars(r)["id"]
	t, err := h.Tasks.Get(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	data := taskData{Task: t}
	if h.Transcript != nil && t.SessionID != "" {
		data.Meta, _ = h.Transcript.GetMeta(r.Context(), t.SessionID)
		data.Events, _ = h.Transcript.Read(r.Context(), t.SessionID, 200)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = h.task.Execute(w, data)
}
