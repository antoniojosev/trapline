// Package httpapi is the HTTP adapter: the REST API, the session cookie and
// the middleware around them.
//
// It is one of four clients of the same use cases (ADR 006), so it contains
// no logic of its own — only decoding, status mapping and encoding.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/antoniojosev/trapline/internal/artifactbundle"
	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/engine"
	"github.com/antoniojosev/trapline/internal/ssrfguard"
)

// maxRequestBody bounds any JSON body the panel or CLI sends. The ingest
// endpoint has its own, much larger, budget; this one only ever carries a
// project name or a login, so a generous kilobyte-scale cap is plenty and
// removes a trivial memory-exhaustion vector.
const maxRequestBody = 64 * 1024

// maxCommitSetBody is the one JSON body that is legitimately large: a
// release's commit set, with the changed paths of every commit in it. A first
// deploy of an existing repository sends thousands of them, and refusing that
// would mean the feature works for toy projects and fails on the real ones.
// It is still bounded, and the domain caps the number of commits on top of
// this, so the budget cannot be spent by one caller repeating itself.
const maxCommitSetBody = 8 * 1024 * 1024

// errorBody is the single error shape the API returns.
//
// One shape for every failure means a client — including an agent — can parse
// errors without special-casing endpoints.
type errorBody struct {
	Error string `json:"error"`
}

// writeJSON sends a value as JSON.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// The status line is already sent, so there is nothing to tell the
		// client. Log it and move on.
		slog.Error("encoding response", "error", err)
	}
}

// writeError maps an error to a status and a message.
//
// The mapping lives in one place so a new endpoint cannot invent its own
// status for a condition the rest of the API already answers consistently.
func writeError(w http.ResponseWriter, err error) {
	status, message := statusFor(err)
	if status >= http.StatusInternalServerError {
		// Internal failures are logged in full and reported flat: the detail
		// is for the operator, not for whoever triggered it.
		slog.Error("request failed", "error", err)
	}
	writeJSON(w, status, errorBody{Error: message})
}

func statusFor(err error) (status int, message string) {
	switch {
	case errors.Is(err, domain.ErrProjectNotFound),
		errors.Is(err, domain.ErrKeyNotFound),
		errors.Is(err, domain.ErrAdminNotFound),
		errors.Is(err, domain.ErrIssueNotFound),
		errors.Is(err, domain.ErrReleaseNotFound),
		errors.Is(err, domain.ErrAlertNotFound),
		errors.Is(err, domain.ErrMonitorNotFound),
		errors.Is(err, domain.ErrTraceNotFound),
		errors.Is(err, domain.ErrArtifactNotFound):
		return http.StatusNotFound, "not found"

	case errors.Is(err, domain.ErrArtifactBudgetExceeded):
		// 413 on the surfaces that can read one, which is every surface but
		// the chunked assembly: there the same condition has to travel inside
		// the body as {"state":"error","detail":…}, because a status code on
		// that endpoint reaches the user as "unknown error" (from the recording,
		// sentryartifacts_handler.go). The message is returned in full — it
		// names the project's usage, its ceiling and the setting that moves
		// it, which is the whole of what somebody needs to unblock a deploy.
		return http.StatusRequestEntityTooLarge, err.Error()

	case errors.Is(err, domain.ErrMonitorDisabled):
		// 410 rather than 404 or 409: the monitor existed, the caller's key
		// is right, and what changed is that somebody switched it off. It is
		// the one status that says exactly that, and a script pinging a key
		// it has held for a year needs to be able to tell it apart from a
		// typo (ADR 016).
		return http.StatusGone, err.Error()
	case errors.Is(err, ssrfguard.ErrBlocked):
		// 422 and not 400. The request was understood and well formed — the
		// URL parses, the JSON is right, the monitor is otherwise valid — and
		// what is being refused is the target itself. A caller told "bad
		// request" would go looking for a typo; the message here says which
		// address was resolved, what kind it is, and which of the two opt-ins
		// is missing (ADR 016, ssrfguard).
		return http.StatusUnprocessableEntity, err.Error()

	case errors.Is(err, domain.ErrForbidden):
		// Distinct from 401 on purpose: a caller holding a valid credential
		// with the wrong scope has a different problem from one holding none,
		// and telling them apart leaks nothing they do not already have.
		return http.StatusForbidden, err.Error()

	case errors.Is(err, domain.ErrInvalidCredentials),
		errors.Is(err, domain.ErrSessionNotFound),
		errors.Is(err, domain.ErrTokenNotFound):
		// One message for both, so the API is not an oracle for which
		// usernames exist.
		return http.StatusUnauthorized, "invalid credentials"

	case errors.Is(err, domain.ErrSetupComplete):
		return http.StatusConflict, err.Error()

	case errors.Is(err, domain.ErrTooManyActiveKeys):
		return http.StatusConflict, err.Error()

	case errors.Is(err, domain.ErrInvalidProject),
		errors.Is(err, domain.ErrInvalidAdmin),
		errors.Is(err, domain.ErrInvalidToken),
		errors.Is(err, domain.ErrInvalidIssue),
		errors.Is(err, domain.ErrInvalidRange),
		errors.Is(err, domain.ErrInvalidSearch),
		errors.Is(err, domain.ErrInvalidRelease),
		errors.Is(err, domain.ErrInvalidAlert),
		errors.Is(err, domain.ErrInvalidDigest),
		errors.Is(err, domain.ErrInvalidSetting),
		errors.Is(err, domain.ErrWeakPassword),
		errors.Is(err, domain.ErrInvalidDSN),
		errors.Is(err, domain.ErrInvalidMonitor),
		errors.Is(err, domain.ErrInvalidTransaction),
		errors.Is(err, domain.ErrInvalidArtifact),
		// An archive this server cannot read is the caller's input, not a
		// failure of this server, and the message says which way it was
		// unreadable — which is the whole of what somebody debugging a build
		// pipeline has to go on. The emulated surface never reaches here: it
		// carries the same condition inside the assemble body, because a
		// status code there reaches the user as "unknown error" (from the recording).
		errors.Is(err, artifactbundle.ErrNotABundle),
		errors.Is(err, ssrfguard.ErrInvalidURL),
		errors.Is(err, domain.ErrInvalidOrigin),
		errors.Is(err, errBadRequest):
		// Validation messages are safe to return: they describe the caller's
		// own input and are what makes an API usable without reading source.
		return http.StatusBadRequest, err.Error()

	default:
		return http.StatusInternalServerError, "internal error"
	}
}

// errBadRequest marks a decoding failure, so statusFor has a single place to
// recognise malformed input.
var errBadRequest = errors.New("bad request")

// decodeJSON reads a bounded, strict JSON body.
//
// Unknown fields are rejected rather than ignored: a client that misspells
// a field name should be told, not silently given a project called "".
func decodeJSON(r *http.Request, target any) error {
	return decodeJSONWithin(r, target, maxRequestBody)
}

// decodeJSONLarge reads a body from the one endpoint whose payload is
// legitimately measured in megabytes: a release's commit set.
func decodeJSONLarge(r *http.Request, target any) error {
	return decodeJSONWithin(r, target, maxCommitSetBody)
}

func decodeJSONWithin(r *http.Request, target any, limit int64) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, limit))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%w: %w", errBadRequest, err)
	}
	// A second value in the stream means the client sent something other than
	// the single object the endpoint documents.
	if decoder.More() {
		return fmt.Errorf("%w: unexpected trailing content", errBadRequest)
	}
	return nil
}

// optional is a request field that can tell "absent" from "null".
//
// A plain *T cannot: encoding/json sets a pointer field to nil both when the
// key is missing and when it is present as null, so an API that means
// "omitted leaves it alone, null clears it back to the default" has no way to
// implement the second half. UnmarshalJSON, on the other hand, is called for
// every key that appears in the body — null included — which is exactly the
// distinction, and nothing more clever than that.
type optional[T any] struct {
	// Present is true when the key appeared in the body at all.
	Present bool
	// Value is nil when the key appeared as null, i.e. "clear this".
	Value *T
}

// UnmarshalJSON records that the key was present and decodes it unless it is
// null.
func (o *optional[T]) UnmarshalJSON(data []byte) error {
	o.Present = true
	if string(data) == "null" {
		o.Value = nil
		return nil
	}
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("%w: %w", errBadRequest, err)
	}
	o.Value = &value
	return nil
}

// jsonRouterErrors rewrites the router's own plain-text 404 and 405 into the
// API's single error shape.
//
// Those two responses come from net/http, not from a handler, so they are the
// one place the API would otherwise answer in a different format than
// everywhere else. A client — an agent especially — should never have to
// parse "404 page not found" as a special case.
func jsonRouterErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&routerErrorWriter{ResponseWriter: w}, r)
	})
}

type routerErrorWriter struct {
	http.ResponseWriter
	replace bool
	done    bool
}

func (w *routerErrorWriter) WriteHeader(status int) {
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		w.replace = true
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	w.ResponseWriter.WriteHeader(status)
}

// Write swallows the router's plain-text body and emits the API's shape
// instead, once. It reports the original length as written so the caller,
// which is net/http and not ours to change, sees a complete write.
// Unwrap exposes the writer underneath, so http.ResponseController can still
// reach the flush and the write deadline this wrapper does not implement.
// See statusRecorder.Unwrap: the two wrappers are stacked, and one of them
// forgetting is enough to break a stream.
func (w *routerErrorWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *routerErrorWriter) Write(b []byte) (int, error) {
	if !w.replace {
		n, err := w.ResponseWriter.Write(b)
		if err != nil {
			return n, err //nolint:wrapcheck // io.Writer contract: pass through untouched.
		}
		return n, nil
	}
	if !w.done {
		w.done = true
		encoded, err := json.Marshal(errorBody{Error: "not found"})
		if err != nil {
			return 0, err //nolint:wrapcheck // encoding a constant struct cannot realistically fail.
		}
		if _, err := w.ResponseWriter.Write(append(encoded, '\n')); err != nil {
			return 0, err //nolint:wrapcheck // io.Writer contract.
		}
	}
	return len(b), nil
}

// apiNamespace splits the /api/ namespace three ways: the protocol's ingest
// path, the surface sentry-cli speaks, and this product's own REST API.
//
// The protocol owns /api/{number}/..., because that is what an SDK builds from
// a DSN and it is not ours to move. Everything else under /api/ is ours. The
// test is whether the first segment parses as a positive integer, which is
// exactly the distinction and nothing more clever than that.
//
// /api/0/ is the third, and it fits in front of that test without disturbing
// it: zero is not a positive integer, so no project could ever have been
// reached there, and the branch takes a path that until now fell through to
// the panel's API and answered 404 (ADR 013). A nil compat handler means the
// surface is not mounted, and those paths go on falling through exactly as
// they did.
func apiNamespace(ingest, compat, api http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		segment := strings.TrimPrefix(r.URL.Path, "/api/")
		if index := strings.IndexByte(segment, '/'); index >= 0 {
			segment = segment[:index]
		}
		if segment == "0" && compat != nil {
			compat.ServeHTTP(w, r)
			return
		}
		if id, err := strconv.ParseInt(segment, 10, 64); err == nil && id > 0 {
			ingest.ServeHTTP(w, r)
			return
		}
		api.ServeHTTP(w, r)
	})
}

// invalidCategory reports an unknown ingest category, naming the ones that
// exist: a client that got it wrong should not have to go looking.
func invalidCategory(category string) error {
	names := make([]string, 0, len(engine.Categories()))
	for _, known := range engine.Categories() {
		names = append(names, string(known))
	}
	return fmt.Errorf("%w: unknown category %q, expected one of %s",
		errBadRequest, category, strings.Join(names, ", "))
}

// invalidRetentionCategory reports an unknown keep-window category. It names a
// different set than invalidCategory: retention covers the aggregates, which
// nothing can send (ADR 010), so telling somebody who mistyped "aggregate" to
// pick from the ingest categories would send them looking for a name that is
// not there.
func invalidRetentionCategory(category string) error {
	names := make([]string, 0, len(engine.RetentionCategories()))
	for _, known := range engine.RetentionCategories() {
		names = append(names, string(known))
	}
	return fmt.Errorf("%w: %q is not a category anything is kept for, expected one of %s",
		errBadRequest, category, strings.Join(names, ", "))
}

func negativeLimit() error {
	return fmt.Errorf("%w: rate_limit_per_minute cannot be negative; use 0 for the default", errBadRequest)
}

// invalidSampleRate reports a traces_sample_rate that is not one.
//
// It names both ends and what the two meaningful extremes do, because the
// value is a share and "invalid" alone leaves somebody guessing whether the
// scale is 0-1 or 0-100 — and guessing 100 would be a request to store every
// waterfall on a busy service.
func invalidSampleRate(rate float64) error {
	return fmt.Errorf("%w: traces_sample_rate is %v; it is a share between 0 and 1, "+
		"where 0 keeps no raw traces and 1 keeps them all (the aggregates are computed "+
		"over every transaction either way)", errBadRequest, rate)
}

// negativeArtifactBudget reports an artifacts_max_mb that is not a size.
//
// It names what zero does, because zero is the value somebody reaches for when
// they mean "turn this off" and it does exactly that — unlike the retention
// map, where the same reflex used to mean "use the default" (ADR 031).
func negativeArtifactBudget() error {
	return fmt.Errorf("%w: artifacts_max_mb cannot be negative; omit it for the default of "+
		"%d MB, or set it to 0 to refuse every upload against this project",
		errBadRequest, domain.DefaultArtifactsMaxMB)
}

func negativeRetention(category string) error {
	return fmt.Errorf("%w: retention for %q cannot be negative; omit it for the default, "+
		"or set it to 0 to keep nothing", errBadRequest, category)
}
