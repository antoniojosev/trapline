package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/antoniojosev/trapline/internal/usecase"
)

// heartbeatInterval is how often an idle stream proves it is still there.
//
// Twenty-five seconds, which is under every default idle timeout worth
// worrying about: nginx and most reverse proxies cut an idle upstream at
// sixty, and a browser that loses the connection will reconnect but will
// have missed whatever happened in between. A comment line is the cheapest
// thing that resets those timers — the EventSource API never sees it.
const heartbeatInterval = 25 * time.Second

// streamPreamble is the first thing every stream writes.
//
// Two lines. `retry` sets the reconnection pace — EventSource reconnects on
// its own, this only decides how fast: five seconds is slow enough that a
// restarting server does not get a reconnection storm from every open panel,
// and fast enough that nobody notices. The comment after it exists to be
// bytes, so the browser's `open` event fires now rather than whenever the
// project next breaks, and the panel can say it is watching.
const streamPreamble = "retry: 5000\n: watching\n\n"

// streamEvent is one issue, as the feed sends it.
//
// A deliberately smaller shape than the listing's: enough to render a row and
// to tell whether it is already on screen, and nothing else. The panel refetches
// the list when the reader asks it to, so this does not have to be a second
// copy of the issue resource that would then have to be kept in step with it.
type streamEvent struct {
	IssueID     int64     `json:"issue_id"`
	ProjectID   int64     `json:"project_id"`
	Title       string    `json:"title"`
	Culprit     string    `json:"culprit"`
	Level       string    `json:"level"`
	Status      string    `json:"status"`
	Times       int64     `json:"times"`
	LastSeen    time.Time `json:"last_seen"`
	LastRelease string    `json:"last_release,omitempty"`
}

// handleEventStream is the live feed a panel keeps open while somebody is
// looking at a project.
//
// Server-sent events, not a WebSocket. Everything here travels one way — the
// server tells, the browser listens — so the half of a WebSocket that carries
// messages back would never be used, and the half that is left costs an
// upgrade handshake, a framing layer, a ping/pong of its own and a proxy
// configuration on every deployment that has one in front. SSE is an ordinary
// HTTP response that never ends: it needs no library on either side, it
// reconnects by itself, and it crosses any proxy that can already serve this
// API (plan §3.1).
func (s *Server) handleEventStream(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	// The project is resolved before a single byte of the stream is written,
	// so an id nobody owns answers 404 rather than an open connection that
	// will never say anything — which is exactly what a working stream on a
	// quiet project looks like.
	if _, err := s.projects.Find(r.Context(), projectID); err != nil {
		writeError(w, err)
		return
	}
	if s.feed == nil {
		writeError(w, fmt.Errorf("%w: this server was built without the live feed", errBadRequest))
		return
	}

	control := http.NewResponseController(w)
	// The server's write deadline exists for a request that answers and stops.
	// A stream never stops, so thirty seconds in it would be closed under a
	// reader who was doing nothing wrong. Lifted for this connection only, and
	// nowhere else.
	if err := control.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		writeError(w, fmt.Errorf("preparing the stream: %w", err))
		return
	}

	// Subscribed before the headers go out, so nothing that happens while
	// this handler is still setting up is lost between the subscription and
	// the first read.
	events, unsubscribe := s.feed.Subscribe(projectID)
	defer unsubscribe()

	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	// The header nginx reads to stop buffering a response it would otherwise
	// hold until it is big enough to be worth forwarding. Harmless anywhere
	// else, and the difference between a live feed and a dead one behind the
	// commonest reverse proxy there is.
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// The project is deliberately not echoed into the preamble. Nothing here
	// needs to be told which project it asked for, and writing a path
	// parameter into a response body is a shape worth never starting, however
	// inert an int64 is.
	if _, err := io.WriteString(w, streamPreamble); err != nil {
		return
	}
	if err := control.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return
	}

	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			// The reader closed the tab, or the server is shutting down.
			// Either way there is nobody to write to.
			return

		case event, open := <-events:
			if !open {
				return
			}
			if err := writeStreamEvent(w, control, &event); err != nil {
				// A write that fails on a stream means the connection is
				// gone. Returning drops the subscription through the defer;
				// there is nothing to report to a client that left.
				return
			}

		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			if err := control.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
				return
			}
		}
	}
}

// writeStreamEvent renders one event in the wire format EventSource parses.
func writeStreamEvent(w http.ResponseWriter, control *http.ResponseController, event *usecase.IssueEvent) error {
	issue := &event.Issue
	encoded, err := json.Marshal(streamEvent{
		IssueID:     issue.ID,
		ProjectID:   issue.ProjectID,
		Title:       issue.Title,
		Culprit:     issue.Culprit,
		Level:       string(issue.Level),
		Status:      string(issue.Status),
		Times:       issue.Times,
		LastSeen:    issue.LastSeen,
		LastRelease: issue.LastRelease,
	})
	if err != nil {
		return fmt.Errorf("encoding a stream event: %w", err)
	}

	// `id:` is deliberately absent. EventSource replays Last-Event-ID on
	// reconnection and expects the server to resume from it, and this feed
	// keeps no history to resume from — the record is the issue row, which
	// the panel refetches. Promising a resumption that cannot happen is worse
	// than not offering one.
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Kind, encoded); err != nil {
		return fmt.Errorf("writing a stream event: %w", err)
	}
	if err := control.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return fmt.Errorf("flushing a stream event: %w", err)
	}
	return nil
}
