package handler

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"

	"github.com/gorilla/mux"
	"github.com/rs/zerolog"

	"code-agent/internal/config"
	"code-agent/internal/orchestrator"
	"code-agent/pkg/tasks"
	"code-agent/pkg/transcript"
)

// APIHandler mounts read-oriented JSON endpoints used by the CLI and
// dashboard. Mutating endpoints (cancel a task, etc.) live here too.
type APIHandler struct {
	Cfg        *config.Config
	Tasks      tasks.Store
	Transcript transcript.Store
	Orch       *orchestrator.Orchestrator
	Logger     zerolog.Logger
}

// Mount wires API routes onto the given subrouter.
func (h *APIHandler) Mount(r *mux.Router) {
	r.HandleFunc("/tasks", h.listTasks).Methods(http.MethodGet)
	r.HandleFunc("/tasks/{id}", h.getTask).Methods(http.MethodGet)
	r.HandleFunc("/tasks/{id}/cancel", h.cancelTask).Methods(http.MethodPost)
	r.HandleFunc("/transcript/{session}", h.getTranscript).Methods(http.MethodGet)
	r.HandleFunc("/boards", h.listBoards).Methods(http.MethodGet)
}

func (h *APIHandler) listTasks(w http.ResponseWriter, r *http.Request) {
	if h.Tasks == nil {
		writeJSON(w, 200, map[string]any{"count": 0, "tasks": []any{}})
		return
	}
	boardFilter := r.URL.Query().Get("board")
	stageFilter := r.URL.Query().Get("stage")
	ctx := r.Context()

	var out []*tasks.Task
	if stageFilter != "" {
		list, err := h.Tasks.ListByStage(ctx, stageFilter)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		for _, t := range list {
			if boardFilter != "" && t.BoardID != boardFilter {
				continue
			}
			out = append(out, t)
		}
	} else if boardFilter != "" {
		list, err := h.Tasks.ListByBoard(ctx, boardFilter)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		out = list
	} else {
		// aggregate across all configured boards
		if h.Cfg.Boards != nil {
			for _, b := range h.Cfg.Boards.Boards {
				list, err := h.Tasks.ListByBoard(ctx, b.ID)
				if err != nil {
					writeErr(w, 500, err)
					return
				}
				out = append(out, list...)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	writeJSON(w, 200, map[string]any{"count": len(out), "tasks": out})
}

func (h *APIHandler) getTask(w http.ResponseWriter, r *http.Request) {
	if h.Tasks == nil {
		writeErr(w, 503, errStr("task store not configured"))
		return
	}
	id := mux.Vars(r)["id"]
	t, err := h.Tasks.Get(r.Context(), id)
	if err != nil {
		if err == tasks.ErrNotFound {
			writeErr(w, 404, err)
			return
		}
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, t)
}

func (h *APIHandler) cancelTask(w http.ResponseWriter, r *http.Request) {
	if h.Tasks == nil {
		writeErr(w, 503, errStr("task store not configured"))
		return
	}
	id := mux.Vars(r)["id"]
	t, err := h.Tasks.Get(r.Context(), id)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	// Flip the task into a terminal-ish stage; runners observe cancellation
	// via timeout. Full cleanup (worker teardown) lives on the runtime.
	t.Stage = "cancelled"
	if err := h.Tasks.Update(r.Context(), t); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "task": t})
}

func (h *APIHandler) getTranscript(w http.ResponseWriter, r *http.Request) {
	sid := mux.Vars(r)["session"]
	if h.Transcript == nil {
		writeErr(w, 503, errStr("transcript store not configured"))
		return
	}
	limit := 500
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			limit = n
		}
	}
	rows, err := h.Transcript.Read(r.Context(), sid, limit)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	meta, _ := h.Transcript.GetMeta(r.Context(), sid)
	writeJSON(w, 200, map[string]any{
		"session": sid,
		"meta":    meta,
		"events":  rows,
	})
}

func (h *APIHandler) listBoards(w http.ResponseWriter, _ *http.Request) {
	type boardView struct {
		ID         string `json:"id"`
		Provider   string `json:"provider"`
		StagesRef  string `json:"stages_ref"`
		RuntimeRef string `json:"runtime_ref"`
	}
	var out []boardView
	if h.Cfg.Boards != nil {
		for _, b := range h.Cfg.Boards.Boards {
			out = append(out, boardView{
				ID:         b.ID,
				Provider:   b.Provider,
				StagesRef:  b.StagesRef,
				RuntimeRef: b.RuntimeRef,
			})
		}
	}
	writeJSON(w, 200, map[string]any{"boards": out})
}

// ---- helpers ----

type errResp struct {
	Error string `json:"error"`
}

type stringError string

func (s stringError) Error() string { return string(s) }
func errStr(s string) error         { return stringError(s) }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, errResp{Error: err.Error()})
}
