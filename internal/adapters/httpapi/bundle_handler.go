package httpapi

import (
	"log/slog"
	"net/http"
)

// bundleContentType is what the bundle is served as.
//
// `text/markdown` and not `application/json`, because the body is a document
// and not a record: a client that received it as a JSON string would have to
// unescape every newline in a stacktrace before anything could read it, and
// the point of this endpoint is that what arrives is already the thing you
// paste into a prompt or a terminal.
//
// The charset is spelled out. Without it a browser guesses, and a stacktrace
// from a non-English codebase is exactly the input where the guess is wrong.
const bundleContentType = "text/markdown; charset=utf-8"

// handleIssueBundle serves one issue as markdown.
//
// Under /projects/{id}/issues/{issueID}/ like every other thing you can ask
// about an issue, rather than at a top-level /issues/{id}/bundle. An issue id
// is only unique within its project everywhere else in this API, and an
// endpoint that took it alone would be the one place a caller could read
// another project's issue by guessing a number.
func (s *Server) handleIssueBundle(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	issueID, err := pathID(r, "issueID")
	if err != nil {
		writeError(w, err)
		return
	}

	// Rendered before anything is written, so a failure halfway through the
	// reads is still a JSON error with a status code rather than half a
	// document with a 200 on it. An agent cannot tell a truncated bundle from
	// a short one.
	document, err := s.bundle.For(r.Context(), projectID, issueID)
	if err != nil {
		writeError(w, err)
		return
	}

	w.Header().Set("Content-Type", bundleContentType)
	w.WriteHeader(http.StatusOK)
	// The document is not HTML and is never served as HTML: the content type
	// is set above, `X-Content-Type-Options: nosniff` goes on every response
	// (middleware.go) so no browser may guess otherwise, and the panel puts
	// this on the clipboard rather than into the DOM. The taint is real —
	// every string in here was written by whoever holds a DSN — and what
	// bounds it is that markdown is not executed and that every value from an
	// event has its newlines collapsed before it is written, which is what
	// stops a payload forging a heading (usecase/bundle.go, SECURITY.md).
	if _, err := w.Write([]byte(document)); err != nil { //nolint:gosec // G705: not HTML, nosniff, never inserted into a page.
		// The status line is already sent, so there is nothing left to tell
		// the client.
		slog.Error("writing the issue bundle", "error", err)
	}
}
