package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// requestTimeout bounds the work done for one request, including every
// database query it makes.
const requestTimeout = 5 * time.Second

// requestInfo collects facts about a request for the access log. Inner
// handlers fill in the route and actor as they become known.
type requestInfo struct {
	id    string
	route string
	actor string
}

type requestInfoKey struct{}

func requestInfoFrom(ctx context.Context) *requestInfo {
	info, _ := ctx.Value(requestInfoKey{}).(*requestInfo)
	return info
}

func requestID(ctx context.Context) string {
	if info := requestInfoFrom(ctx); info != nil {
		return info.id
	}
	return ""
}

func newRequestID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b)
}

// statusRecorder remembers the status code written to the client.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusRecorder) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// observe assigns a request ID, applies the request timeout, sets response
// headers common to every answer, and writes one access log line.
//
// The log records the matched route pattern rather than the raw URL, so flag
// names, user IDs, and query strings stay out of the logs. Headers and bodies
// are never logged.
func observe(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		info := &requestInfo{id: newRequestID(), route: "unmatched"}

		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		ctx = context.WithValue(ctx, requestInfoKey{}, info)

		w.Header().Set("X-Request-ID", info.id)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Responses depend on the caller's API key; shared caches must not
		// keep them.
		w.Header().Set("Cache-Control", "no-store")

		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r.WithContext(ctx))

		level := slog.LevelInfo
		if info.route == "GET /healthz" || info.route == "GET /readyz" {
			level = slog.LevelDebug // probes are frequent and uninteresting
		}
		logger.Log(ctx, level, "request",
			"request_id", info.id,
			"method", r.Method,
			"route", info.route,
			"status", recorder.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"actor", info.actor,
		)
	})
}

// recoverPanic turns a panic in a handler into a 500 response and a log
// entry, so one bad request cannot take the process down.
func recoverPanic(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			if recovered == http.ErrAbortHandler {
				panic(recovered) // the standard way to abort a response; let net/http handle it
			}
			logger.ErrorContext(r.Context(), "panic in handler",
				"request_id", requestID(r.Context()),
				"panic", recovered,
				"stack", string(debug.Stack()),
			)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		}()
		next.ServeHTTP(w, r)
	})
}
