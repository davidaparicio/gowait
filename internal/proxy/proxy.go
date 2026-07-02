// Package proxy builds the reverse proxy to the protected backend.
package proxy

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// New returns a reverse proxy targeting backend. If preserveHost is true the
// original Host header is forwarded instead of the backend's.
func New(backend *url.URL, preserveHost bool) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			host := r.In.Host
			r.SetURL(backend)
			r.SetXForwarded()
			if preserveHost {
				r.Out.Host = host
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Error("backend proxy error", "path", r.URL.Path, "err", err)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<!doctype html><title>Backend unavailable</title><h1>502 — backend unavailable</h1><p>The service behind this waiting room is not responding. Please try again shortly.</p>"))
		},
	}
}
