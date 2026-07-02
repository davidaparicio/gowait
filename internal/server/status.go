package server

import (
	"encoding/json"
	"math"
	"net/http"
	"time"

	"github.com/davidaparicio/gowait/internal/queue"
)

type statusResponse struct {
	Status      string `json:"status"`
	Position    int    `json:"position"`
	QueueLength int    `json:"queue_length"`
	ActiveUsers int    `json:"active_users"`
	ETASeconds  int    `json:"eta_seconds"`
	PollSeconds int    `json:"poll_seconds"`
	ServerTime  string `json:"server_time"`
}

// handleStatus serves GET /gowait/status for the waiting page's poller.
// Lookup inside StatusOf doubles as the queued user's heartbeat.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	id, err := s.ticketID(w, r)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	res, err := s.ctrl.StatusOf(r.Context(), id)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeStatusJSON(w, http.StatusOK, res)
}

func (s *Server) writeStatusJSON(w http.ResponseWriter, code int, res queue.Result) {
	status := "queued"
	if res.Decision == queue.DecisionProxy {
		status = "active"
	}
	eta := int(math.Ceil(res.ETA.Seconds()))
	// Never advertise less than one poll interval: the user can't get in
	// faster than their next poll anyway.
	if min := int(s.cfg.PollInterval.Seconds()); status == "queued" && eta < min {
		eta = min
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(statusResponse{
		Status:      status,
		Position:    res.Position,
		QueueLength: res.QueueLength,
		ActiveUsers: res.ActiveCount,
		ETASeconds:  eta,
		PollSeconds: int(s.cfg.PollInterval.Seconds()),
		ServerTime:  s.now().UTC().Format(time.RFC3339),
	})
}
