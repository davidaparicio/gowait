package server

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
)

// registerAdmin mounts the admin API. Routes 404 while no admin key is
// configured, hiding the surface entirely.
func (s *Server) registerAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET "+ownPrefix+"admin/stats", s.adminGuard(s.handleAdminStats))
	mux.HandleFunc("GET "+ownPrefix+"admin/capacity", s.adminGuard(s.handleAdminCapacityGet))
	mux.HandleFunc("PUT "+ownPrefix+"admin/capacity", s.adminGuard(s.handleAdminCapacityPut))
	mux.HandleFunc("POST "+ownPrefix+"admin/flush", s.adminGuard(s.handleAdminFlush))
	// Catch the rest of the subtree: wrong methods get 405, unknown paths
	// 404 — and nothing under admin/ ever falls through to the gatekeeper.
	mux.HandleFunc(ownPrefix+"admin/", s.adminGuard(s.handleAdminFallback))
}

func (s *Server) handleAdminFallback(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case ownPrefix + "admin/stats", ownPrefix + "admin/capacity", ownPrefix + "admin/flush":
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	default:
		http.NotFound(w, r)
	}
}

// adminGuard enforces admin auth: a valid admin cookie (browser flow) or the
// admin key in the X-Gowait-Admin header (script flow). Cookie auth is safe
// against cross-site PUT/POST because the cookie is SameSite=Lax.
func (s *Server) adminGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminKey == "" {
			http.NotFound(w, r)
			return
		}
		if !s.adminAuthorized(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (s *Server) adminAuthorized(r *http.Request) bool {
	if k := r.Header.Get(adminHeader); k != "" {
		return subtle.ConstantTimeCompare([]byte(k), []byte(s.cfg.AdminKey)) == 1
	}
	return s.isAdmin(r)
}

func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.ctrl.Stats(r.Context())
	if err != nil {
		http.Error(w, "stats unavailable", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"queue_length":        stats.QueueLength,
		"active_users":        stats.ActiveCount,
		"capacity":            s.ctrl.Capacity(),
		"avg_session_seconds": stats.AvgSessionSecs,
	})
}

func (s *Server) handleAdminCapacityGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"capacity": s.ctrl.Capacity()})
}

func (s *Server) handleAdminCapacityPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Capacity int `json:"capacity"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body, want {\"capacity\": N}", http.StatusBadRequest)
		return
	}
	if err := s.ctrl.SetCapacity(r.Context(), body.Capacity); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"capacity": s.ctrl.Capacity()})
}

func (s *Server) handleAdminFlush(w http.ResponseWriter, r *http.Request) {
	n, err := s.ctrl.Flush(r.Context())
	if err != nil {
		http.Error(w, "flush failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"flushed": n})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
