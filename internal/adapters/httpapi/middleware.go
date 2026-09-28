package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// CSRFHeader is the header a state-changing request must carry.
//
// This is the standard custom-header defence for a cookie-authenticated JSON
// API: a browser cannot set a custom header on a cross-origin request without
// a preflight, and no permissive CORS policy is served, so the preflight
// fails. It costs one header on the client and no token plumbing, state or
// per-form ceremony on the server. The SameSite=Lax cookie is the second
// layer, not the only one.
const CSRFHeader = "X-Trapline-Request"

// middleware is a decorator over a handler.
type middleware func(http.Handler) http.Handler

// chain applies middleware so the first listed is the outermost, which is the
// order they read in at the call site.
func chain(handler http.Handler, wrappers ...middleware) http.Handler {
	for i := len(wrappers) - 1; i >= 0; i-- {
		handler = wrappers[i](handler)
	}
	return handler
}

// recoverPanics turns a panic into a 500 rather than a dropped connection and
// a dead server.
//
// The domain forbids panics outside main, so reaching here is a bug. The point
// is that one bug in one handler must not take the whole installation down —
// this is an error tracker, and being the thing that goes silent during an
// incident is the one unacceptable failure.
func recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				// http.ErrAbortHandler is the documented way to abandon a
				// response on purpose; re-panic so the server handles it.
				if err, isError := recovered.(error); isError && errors.Is(err, http.ErrAbortHandler) {
					panic(recovered)
				}
				slog.Error("panic in handler",
					"panic", recovered,
					"method", r.Method,
					"path", r.URL.Path,
				)
				writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// securityHeaders sets the headers that cost nothing and close whole classes
// of attack.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("X-Frame-Options", "DENY")
		header.Set("Referrer-Policy", "no-referrer")
		// The panel is a self-contained SPA served from this binary: it needs
		// no external origin at all, so the policy can be maximally strict.
		header.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
				"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

// requireCSRFHeader rejects state-changing requests without the custom header.
func requireCSRFHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			// Safe methods change nothing, so there is nothing to forge.
		default:
			if r.Header.Get(CSRFHeader) == "" {
				writeJSON(w, http.StatusForbidden, errorBody{
					Error: "missing " + CSRFHeader + " header",
				})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the status code for the access log, which the
// standard ResponseWriter does not expose.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.wrote {
		r.status, r.wrote = status, true
	}
	r.ResponseWriter.WriteHeader(status)
}

// Unwrap exposes the writer underneath.
//
// http.ResponseController looks for this method to find the capabilities a
// wrapper does not implement itself — flushing, and the per-connection write
// deadline. Without it the access log, which exists only to record a status
// code, silently takes both away from every handler beneath it, and the
// event stream that needs them cannot work at all: an SSE response that
// cannot flush arrives when the connection closes, which for a stream is
// never.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.status, r.wrote = http.StatusOK, true
	}
	n, err := r.ResponseWriter.Write(b)
	if err != nil {
		return n, err //nolint:wrapcheck // io.Writer contract: pass through untouched.
	}
	return n, nil
}

// logRequests writes one structured line per request.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(recorder, r)

		level := slog.LevelInfo
		if recorder.status >= http.StatusInternalServerError {
			level = slog.LevelError
		}
		slog.Log(r.Context(), level, "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"duration_ms", time.Since(started).Milliseconds(),
		)
	})
}
