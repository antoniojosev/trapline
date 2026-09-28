package httpapi

import (
	"embed"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/usecase"
)

// statusPrefix is where the public status page lives.
//
// Outside /api/ and outside the panel, like the ping surface and for a related
// reason: the reader is not a client of this API and not an operator. It is
// somebody who was handed a link because something they use stopped working,
// and the shortest URL that says what it is — /status/<project> — is the one
// that survives being pasted into a support ticket.
const statusPrefix = "/status/"

//go:embed templates/status.html
var statusTemplateFS embed.FS

// statusTemplate is parsed once, at start, so a broken template is a panic in
// the first second of the process rather than a 500 on a public page during an
// outage — which is precisely when this page is read.
var statusTemplate = template.Must(
	template.New("status.html").Funcs(statusTemplateFuncs()).ParseFS(statusTemplateFS, "templates/status.html"),
)

// WithStatusPage mounts the public status page.
//
// Optional like every other area: a server built without it has no /status/
// path at all, which is what an installation that publishes nothing should
// look like from the outside.
func (s *Server) WithStatusPage(status *usecase.StatusPage) *Server {
	s.status = status
	return s
}

// handleStatusPage renders one project's page.
//
// Server-rendered HTML with no JavaScript and no React, and that is the
// decision rather than a shortcut (ADR 017). This is the one page in the
// product that is read when things are broken, from a phone, on a bad
// connection, by somebody who has never heard of this software. Every
// dependency it does not have is one more way it cannot fail to tell them
// what they came to find out.
func (s *Server) handleStatusPage(w http.ResponseWriter, r *http.Request) {
	slug := strings.Trim(strings.TrimPrefix(r.URL.Path, statusPrefix), "/")
	if slug == "" || strings.Contains(slug, "/") {
		writeStatusNotFound(w)
		return
	}

	page, err := s.status.Page(r.Context(), slug)
	if err != nil {
		switch {
		case errors.Is(err, usecase.ErrStatusPageDisabled), errors.Is(err, domain.ErrProjectNotFound):
			// The same answer for both, deliberately. A project that exists
			// and keeps its status private must not be distinguishable from
			// one that does not exist: the difference is exactly the fact its
			// operator chose not to publish.
			writeStatusNotFound(w)
		default:
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}

	// Public and cacheable for as long as the use case will reuse it, so a
	// CDN or a browser in front of this server shares the same answer rather
	// than each reader arriving here (ADR 017).
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(int(domain.StatusPageTTL.Seconds())))
	if err := statusTemplate.Execute(w, page); err != nil {
		// The status line and the headers went out before the first byte of
		// the body, so there is nothing left to answer with. Saying so in the
		// log is the whole of what can be done.
		slog.Error("rendering the status page failed after the response started",
			"project", slug, "error", err)
	}
}

// writeStatusNotFound answers in HTML, because the reader is a browser.
func writeStatusNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Not cached: a page that was switched on a minute ago must appear, and
	// the 404 for an unknown slug is cheap enough not to need help.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("<!doctype html><meta charset=\"utf-8\"><title>Not found</title>" +
		"<p style=\"font:16px system-ui;padding:2rem\">No status page here.\n"))
}

// throttleStatus is the per-address ceiling on the public page.
//
// It shares the ingest limiter's ceiling for the reason the ping surface gives
// (ping_handler.go): the two are the same kind of traffic — public,
// unauthenticated, cheap per request — and an operator who has already tuned
// one number should not meet a second one with a different default. The page
// itself is a map lookup for thirty seconds out of every thirty, so the limit
// is there to bound the misses, not the hits (ADR 023).
func (s *Server) throttleStatus(next http.Handler) http.Handler {
	return limitByIP(s.limits.ingest, s.clientAddr, denyStatus, next)
}

// denyStatus refuses in the same shape the page answers in.
func denyStatus(w http.ResponseWriter, retryAfter time.Duration) {
	setRetryAfter(w, retryAfter)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte("<!doctype html><meta charset=\"utf-8\"><title>Too many requests</title>" +
		"<p style=\"font:16px system-ui;padding:2rem\">Too many requests from this address.\n"))
}

// statusTemplateFuncs are the handful of formatters the template needs.
//
// Formatting in Go rather than in the template, because these are the numbers
// the page exists to state and getting one of them wrong — a percentage
// rounded up to 100, a duration printed in nanoseconds — is the failure that
// matters here. Each of them is a domain function or a one-liner over one.
func statusTemplateFuncs() template.FuncMap {
	return template.FuncMap{
		"uptime": domain.FormatUptime,
		"date":   func(t time.Time) string { return t.Format("2006-01-02") },
		"stamp":  func(t time.Time) string { return t.Format("2006-01-02 15:04 MST") },
		"since": func(t *time.Time) string {
			if t == nil {
				return ""
			}
			return t.UTC().Format("2006-01-02 15:04 MST")
		},
		"duration": func(d time.Duration) string {
			switch {
			case d >= time.Hour:
				return strconv.Itoa(int(d.Hours())) + " h " + strconv.Itoa(int(d.Minutes())%60) + " min"
			case d >= time.Minute:
				return strconv.Itoa(int(d.Minutes())) + " min"
			default:
				return "under a minute"
			}
		},
		"statusLabel": func(status domain.UptimeStatus) string {
			switch status {
			case domain.UptimeUp:
				return "Operational"
			case domain.UptimeDown:
				return "Down"
			default:
				return "No data yet"
			}
		},
	}
}

// statusPageSettingsBody is the installation-wide title and description.
type statusPageSettingsBody struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

// handleGetStatusPageSettings reports what the public page is headed with.
func (s *Server) handleGetStatusPageSettings(w http.ResponseWriter, r *http.Request) {
	settings := s.status.Settings(r.Context())
	writeJSON(w, http.StatusOK, statusPageSettingsBody{
		Title: settings.Title, Description: settings.Description,
	})
}

// handleSetStatusPageSettings changes it.
//
// PUT of both fields rather than a patch of either: they are two lines of one
// heading, an operator editing one is looking at the other, and a partial
// update would be a way to leave a description under a title it no longer
// belongs to. Empty means "use the default", which for the title is the
// project's own name — so clearing it is how somebody undoes a bad one.
func (s *Server) handleSetStatusPageSettings(w http.ResponseWriter, r *http.Request) {
	var request statusPageSettingsBody
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	settings, err := s.status.SetSettings(r.Context(), domain.StatusPageSettings{
		Title: request.Title, Description: request.Description,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, statusPageSettingsBody{
		Title: settings.Title, Description: settings.Description,
	})
}
