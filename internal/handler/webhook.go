package handler

import (
	"net/http"

	"github.com/gorilla/mux"
	"github.com/rs/zerolog"

	"code-agent/internal/orchestrator"
	"code-agent/pkg/providers"
)

// WebhookHandler routes incoming POSTs to the right BoardProvider.
//
// URL: POST /webhooks/{board}
type WebhookHandler struct {
	Orch   *orchestrator.Orchestrator
	Logger zerolog.Logger
}

func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	boardID := vars["board"]
	p, ok := h.Orch.Provider(boardID)
	if !ok {
		http.Error(w, "unknown board", http.StatusNotFound)
		return
	}

	// Provider writes to a channel; we route through dispatcher.EmitWebhook
	// so dedupe is applied uniformly.
	out := make(chan providers.Event, 4)
	done := make(chan struct{})
	go func() {
		for e := range out {
			h.Orch.Dispatcher().EmitWebhook(e)
		}
		close(done)
	}()
	p.HandleWebhook(w, r, out)
	close(out)
	<-done
}
