package server

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strconv"

	"github.com/davidaparicio/gowait/internal/queue"
)

// gatekeeper decides, for every non-reserved request: proxy it (active or
// admin), or serve the waiting room.
func (s *Server) gatekeeper(w http.ResponseWriter, r *http.Request) {
	// Admin key presented → set the admin cookie. The query-param flow
	// redirects to the same URL with the key stripped, so it doesn't linger
	// in the address bar or logs; the header flow proxies straight through.
	if s.cfg.AdminKey != "" {
		if key, fromQuery := adminKeyFrom(r); key != "" {
			if subtle.ConstantTimeCompare([]byte(key), []byte(s.cfg.AdminKey)) != 1 {
				http.Error(w, "invalid admin key", http.StatusForbidden)
				return
			}
			expiry := s.now().Add(adminSessionDuration).Unix()
			s.setCookie(w, adminCookie, fmt.Sprintf("admin:%d", expiry))
			if fromQuery {
				u := *r.URL
				q := u.Query()
				q.Del(adminParam)
				u.RawQuery = q.Encode()
				http.Redirect(w, r, u.String(), http.StatusFound)
				return
			}
			s.backend.ServeHTTP(w, r)
			return
		}
		if s.isAdmin(r) {
			s.backend.ServeHTTP(w, r)
			return
		}
	}

	id, err := s.ticketID(w, r)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	res, err := s.ctrl.Check(r.Context(), id)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if res.Decision == queue.DecisionProxy {
		s.backend.ServeHTTP(w, r)
		return
	}

	// Queued: humans get the waiting page in place (refresh- and
	// deep-link-safe), API clients get honest 503 semantics.
	w.Header().Set("Cache-Control", "no-store")
	if acceptsHTML(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(s.waitHTML)
		return
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(s.cfg.PollInterval.Seconds())))
	s.writeStatusJSON(w, http.StatusServiceUnavailable, res)
}

func adminKeyFrom(r *http.Request) (key string, fromQuery bool) {
	if k := r.Header.Get(adminHeader); k != "" {
		return k, false
	}
	return r.URL.Query().Get(adminParam), true
}
