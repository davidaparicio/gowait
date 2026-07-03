// Package server wires the gatekeeper, status endpoint and reverse proxy
// into one http.Handler.
package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/davidaparicio/gowait/internal/config"
	"github.com/davidaparicio/gowait/internal/metrics"
	"github.com/davidaparicio/gowait/internal/proxy"
	"github.com/davidaparicio/gowait/internal/queue"
	"github.com/davidaparicio/gowait/internal/ticket"
	"github.com/davidaparicio/gowait/internal/waitpage"
)

const (
	ticketCookie = "gowait_ticket"
	adminCookie  = "gowait_admin"
	adminHeader  = "X-Gowait-Admin"
	adminParam   = "gowait_admin"

	// Reserved prefix for gowait's own endpoints; never proxied.
	ownPrefix = "/gowait/"

	adminSessionDuration = 12 * time.Hour
)

type Server struct {
	cfg      *config.Config
	ctrl     *queue.Controller
	signer   *ticket.Signer
	backend  http.Handler
	waitHTML []byte
	now      func() time.Time
	metrics  *metrics.Registry // nil-safe
}

// New builds the full gowait handler. now may be nil (defaults to time.Now);
// reg may be nil (no metrics).
func New(cfg *config.Config, ctrl *queue.Controller, signer *ticket.Signer, now func() time.Time, reg *metrics.Registry) (*Server, error) {
	html, err := waitpage.Render(waitpage.Options{
		PollInterval: cfg.PollInterval,
		Lang:         cfg.WaitLang,
		Title:        cfg.WaitTitle,
		Brand:        cfg.WaitBrand,
		Message:      cfg.WaitMessage,
		TemplatePath: cfg.WaitTemplate,
	})
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &Server{
		cfg:      cfg,
		ctrl:     ctrl,
		signer:   signer,
		backend:  proxy.New(cfg.BackendURL, cfg.PreserveHost),
		waitHTML: html,
		now:      now,
		metrics:  reg,
	}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(ownPrefix+"healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc(ownPrefix+"status", s.handleStatus)
	if s.cfg.Metrics && s.metrics != nil {
		mux.HandleFunc(ownPrefix+"metrics", s.handleMetrics)
	}
	s.registerAdmin(mux)
	mux.HandleFunc("/", s.gatekeeper)
	return mux
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	stats, err := s.ctrl.Stats(r.Context())
	if err != nil {
		http.Error(w, "stats unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.metrics.WriteTo(w, stats, s.ctrl.Capacity())
}

// --- cookie helpers ---

func (s *Server) setCookie(w http.ResponseWriter, name, payload string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    s.signer.Sign(payload),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.cfg.CookieSecure,
	})
}

// ticketID extracts a verified ticket ID from the request cookie, or mints a
// new one (setting the cookie) when missing or tampered with.
func (s *Server) ticketID(w http.ResponseWriter, r *http.Request) (string, error) {
	if c, err := r.Cookie(ticketCookie); err == nil {
		if id, ok := s.signer.Verify(c.Value); ok && id != "" {
			return id, nil
		}
	}
	id, err := ticket.NewID()
	if err != nil {
		return "", fmt.Errorf("minting ticket: %w", err)
	}
	s.setCookie(w, ticketCookie, id)
	return id, nil
}

// isAdmin reports whether the request carries a valid, unexpired admin cookie.
func (s *Server) isAdmin(r *http.Request) bool {
	c, err := r.Cookie(adminCookie)
	if err != nil {
		return false
	}
	payload, ok := s.signer.Verify(c.Value)
	if !ok || !strings.HasPrefix(payload, "admin:") {
		return false
	}
	exp, err := strconv.ParseInt(strings.TrimPrefix(payload, "admin:"), 10, 64)
	if err != nil {
		return false
	}
	return s.now().Unix() < exp
}

func acceptsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}
