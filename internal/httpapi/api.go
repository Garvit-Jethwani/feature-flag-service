// Package httpapi exposes the flag service over HTTP: routing,
// authentication, request decoding, and the mapping of errors to responses.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Garvit-Jethwani/feature-flag-service/internal/service"
)

// readinessTimeout bounds the database check behind /readyz.
const readinessTimeout = 2 * time.Second

type api struct {
	svc   *service.Service
	log   *slog.Logger
	ready func(context.Context) error
}

// New returns the HTTP handler for the service. ready reports whether the
// service's dependencies are reachable; it backs /readyz.
func New(svc *service.Service, auth *Authenticator, logger *slog.Logger, ready func(context.Context) error) http.Handler {
	a := &api{svc: svc, log: logger, ready: ready}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.healthz)
	mux.HandleFunc("GET /readyz", a.readyz)

	mux.Handle("GET /v1/flags", auth.require(RoleReader, a.listFlags))
	mux.Handle("POST /v1/flags", auth.require(RoleAdmin, a.createFlag))
	mux.Handle("GET /v1/flags/{name}", auth.require(RoleReader, a.getFlag))
	mux.Handle("PATCH /v1/flags/{name}", auth.require(RoleAdmin, a.updateFlag))
	mux.Handle("DELETE /v1/flags/{name}", auth.require(RoleAdmin, a.deleteFlag))
	mux.Handle("PUT /v1/flags/{name}/overrides/{userID}", auth.require(RoleAdmin, a.setOverride))
	mux.Handle("DELETE /v1/flags/{name}/overrides/{userID}", auth.require(RoleAdmin, a.deleteOverride))
	mux.Handle("GET /v1/flags/{name}/evaluate", auth.require(RoleReader, a.evaluate))

	return observe(logger, recoverPanic(logger, route(mux)))
}

// route dispatches to mux, records the matched route pattern for the access
// log, and answers unmatched requests in the API's JSON error shape instead
// of the mux's plain-text 404 and 405.
func route(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallback, pattern := mux.Handler(r)

		if pattern == "" {
			// Nothing matched. Ask the mux's own fallback whether this is an
			// unknown path or a known path with the wrong method.
			probe := &probeWriter{header: http.Header{}}
			fallback.ServeHTTP(probe, r)
			if probe.status == http.StatusMethodNotAllowed {
				w.Header().Set("Allow", probe.header.Get("Allow"))
				writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed for this path")
				return
			}
			writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
			return
		}

		// Registered patterns look like "GET /v1/flags/{name}". Anything else
		// is a redirect target built from the request path, which must not be
		// logged.
		if info := requestInfoFrom(r.Context()); info != nil {
			if strings.Contains(pattern, " ") {
				info.route = pattern
			} else {
				info.route = "redirect"
			}
		}
		mux.ServeHTTP(w, r)
	})
}

// probeWriter captures the status and headers of a response and discards the
// body.
type probeWriter struct {
	header http.Header
	status int
}

func (p *probeWriter) Header() http.Header { return p.header }

func (p *probeWriter) WriteHeader(status int) {
	if p.status == 0 {
		p.status = status
	}
}

func (p *probeWriter) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = http.StatusOK
	}
	return len(b), nil
}

func (a *api) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *api) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()
	if err := a.ready(ctx); err != nil {
		a.log.WarnContext(r.Context(), "readiness check failed", "error", err.Error())
		writeError(w, http.StatusServiceUnavailable, "not_ready", "the database is not reachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
