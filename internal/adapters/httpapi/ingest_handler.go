package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/antoniojosev/trapline/internal/adapters/compression"
	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/envelope"
	"github.com/antoniojosev/trapline/internal/usecase"
)

// Ingest limits. These are the ceiling on what one unauthenticated request may
// cost, and they are deliberately concrete rather than configurable: a knob
// here is a knob someone eventually turns up to make an error go away.
const (
	maxIngestBody = 20 << 20 // 20 MiB after decompression
	// rateLimitSeconds is how long an SDK is told to stop sending a refused
	// category. Long enough that a switched-off category is nearly free, short
	// enough that switching it back on takes effect the same afternoon.
	rateLimitSeconds = 300
)

// handleIngest accepts an envelope.
//
// This is the only endpoint that is public by design. Its authentication is a
// key that ships inside browser bundles, so the key proves which project to
// write to and nothing more — every other control here (size caps, rate
// limits, scrubbing) exists because authentication cannot be the defence
// (SECURITY.md).
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r, "projectID")
	if err != nil {
		writeIngestError(w, http.StatusBadRequest, "invalid project id")
		return
	}

	publicKey, found := ingestKey(r)
	if !found {
		writeIngestError(w, http.StatusUnauthorized, "missing sentry_key")
		return
	}

	key, err := s.projects.AuthenticateKey(r.Context(), publicKey, projectID)
	if err != nil {
		// One answer for an unknown key, a revoked key and a key belonging to
		// another project. Distinguishing them would let anyone with a key
		// enumerate which project ids exist.
		writeIngestError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	// The shared memory budget, claimed before the decompressor is built.
	//
	// The order matters: a zstd decoder allocates its history buffer from the
	// window size the frame declares, so a check made after DecompressRequest
	// returns would be a check made once the memory was already resident
	// (ingest_budget.go, ADR 039).
	encoding := r.Header.Get("Content-Encoding")
	hold, admitted := s.limits.ingestArena.hold(compression.WorkingSet(encoding))
	if !admitted {
		denyIngestBusy(w)
		return
	}
	// Released once, at the end of the request rather than when the body is
	// closed: the bytes it paid for stay alive in the parsed envelope until
	// this handler returns.
	defer hold.release()

	body, err := compression.DecompressRequest(r.Body, encoding, maxIngestBody)
	if err != nil {
		writeIngestError(w, http.StatusBadRequest, err.Error())
		return
	}
	body = hold.wrap(body)
	if closer, ok := body.(interface{ Close() error }); ok {
		defer func() { _ = closer.Close() }()
	}

	result, err := s.ingest.Process(r.Context(), key.ProjectID, body, envelope.Limits{
		MaxEnvelopeBytes: maxIngestBody,
	}, usecase.ClientInfo{
		// The browser's own account of itself, which no browser SDK puts in
		// the payload. Read here rather than in the use case because a header
		// is transport, and the rule about what it means is not.
		UserAgent: r.Header.Get("User-Agent"),
		// The dynamic sampling context, when a trace was continued through a
		// proxy rather than started here. It says what the SDK already
		// sampled, which composes with this server's own rate (ADR 021).
		Baggage: r.Header.Get("Baggage"),
	})
	switch {
	case errors.Is(err, errIngestBusy):
		// Not 413: nothing is wrong with this envelope, and telling a client
		// its payload is too large when the real answer is "come back in a
		// moment" would make it drop an event it should have retried.
		denyIngestBusy(w)
		return
	case errors.Is(err, envelope.ErrTooLarge), errors.Is(err, compression.ErrTooLarge):
		writeIngestError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	case errors.Is(err, envelope.ErrMalformed):
		writeIngestError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		slog.Error("ingesting an envelope", "project_id", key.ProjectID, "error", err)
		writeIngestError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Backpressure through the protocol's own mechanism. The official SDKs
	// honour this and stop sending the named categories, which is what makes a
	// switched-off subsystem cost nothing on the wire rather than merely
	// nothing on disk (ADR 005).
	if len(result.RateLimited) > 0 {
		header := usecase.FormatRateLimitHeader(result.RateLimited, rateLimitSeconds)
		w.Header().Set("X-Sentry-Rate-Limits", header)

		// 429 only when the whole envelope was refused. A partially accepted
		// envelope has to answer 200, or an SDK that batches categories
		// together would retry the events it already delivered.
		if result.Accepted == 0 {
			w.Header().Set("Retry-After", strconv.Itoa(rateLimitSeconds))
			writeIngestError(w, http.StatusTooManyRequests, "rate limited")
			return
		}
	}

	// Indexed rather than ranged by value: a domain.Issue is a large struct
	// and this runs on the ingest path.
	for index := range result.Regressions {
		regressed := &result.Regressions[index]
		slog.Info("issue regressed",
			"project_id", key.ProjectID,
			"issue_id", regressed.ID,
			"title", regressed.Title,
			"release", regressed.LastRelease,
		)
	}

	// The response body is the event id, which is what the protocol specifies
	// and what an SDK logs when asked to be verbose.
	writeJSON(w, http.StatusOK, map[string]string{"id": strings.ReplaceAll(domain.NewEventID(), "-", "")})
}

// ingestKey finds the public key an SDK sent.
//
// Three places, because different SDKs and different transports use different
// ones: the X-Sentry-Auth header is the common case, the query parameter is
// what a browser's sendBeacon uses, and a bare Authorization header appears in
// some older clients. Supporting only the first would silently drop events
// from real SDKs.
func ingestKey(r *http.Request) (string, bool) {
	for _, header := range []string{r.Header.Get("X-Sentry-Auth"), r.Header.Get("Authorization")} {
		if key, found := parseSentryAuth(header); found {
			return key, true
		}
	}
	if key := r.URL.Query().Get("sentry_key"); key != "" {
		return key, true
	}
	return "", false
}

// parseSentryAuth reads "Sentry sentry_version=7, sentry_key=abc, ...".
func parseSentryAuth(header string) (string, bool) {
	if header == "" {
		return "", false
	}
	if index := strings.IndexByte(header, ' '); index >= 0 {
		if !strings.EqualFold(strings.TrimSpace(header[:index]), "sentry") {
			return "", false
		}
		header = header[index+1:]
	}
	for _, part := range strings.Split(header, ",") {
		name, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found || !strings.EqualFold(strings.TrimSpace(name), "sentry_key") {
			continue
		}
		if trimmed := strings.Trim(strings.TrimSpace(value), `"`); trimmed != "" {
			return trimmed, true
		}
	}
	return "", false
}

// writeIngestError answers in the shape SDKs expect.
//
// The ingest endpoint predates this product's own error shape and SDKs parse
// it, so it keeps the protocol's "detail" field rather than the API's "error".
// Consistency with ourselves is worth less here than compatibility with the
// clients we exist to serve.
func writeIngestError(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"detail": detail})
}
