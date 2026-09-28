package httpapi

import (
	"fmt"
	"net/http"
	"time"

	"github.com/antoniojosev/trapline/internal/digest"
	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/usecase"
)

// WithDigest mounts the weekly report's endpoints.
//
// Optional in the same way the panel and the releases are: a server built
// without it simply does not have the routes. It is a setter rather than
// another constructor argument so that adding an area does not rewrite every
// call site (server.go).
func (s *Server) WithDigest(weekly *usecase.Digest, channels *usecase.ChannelHealth) *Server {
	s.digest = weekly
	s.channels = channels
	return s
}

// digestPreviewRequest is what a caller can narrow a preview to.
//
// Both fields optional, and the empty body is the common case: "show me what
// would go out if it went out now" needs no arguments, and a preview that
// demanded a time range would be the stats endpoint with a different name.
type digestPreviewRequest struct {
	// ProjectID limits the report to one project. Absent means every project.
	ProjectID int64 `json:"project_id"`
	// At is the scheduled moment to report as of, RFC 3339. Absent means now.
	At string `json:"at"`
}

// digestPreviewResponse carries both shapes of the same report.
//
// The text is what a channel would actually deliver, and the structured
// report is the same numbers with fields — because the fourth client of this
// API is an agent, and an agent asked "how many new issues last week" should
// get a number rather than a paragraph to parse (ADR 006).
type digestPreviewResponse struct {
	Schedule digestScheduleBody `json:"schedule"`
	Text     string             `json:"text"`
	Report   digest.Report      `json:"report"`
}

// digestScheduleBody is the send time, as read and as written.
type digestScheduleBody struct {
	// Weekday is the name, not the number: this is the field a person reads
	// and types, and "3" is a day nobody can name without counting.
	Weekday string `json:"weekday"`
	Hour    int    `json:"hour"`
	// Timezone is always "UTC" in this version, and is present rather than
	// implied so that the day a schedule can name a zone, a client that
	// already reads this field does not have to learn a new one.
	Timezone string `json:"timezone"`
}

func newDigestScheduleBody(schedule domain.DigestSchedule) digestScheduleBody {
	return digestScheduleBody{
		Weekday:  schedule.Weekday.String(),
		Hour:     schedule.Hour,
		Timezone: "UTC",
	}
}

// handleDigestPreview renders the weekly report without sending it.
func (s *Server) handleDigestPreview(w http.ResponseWriter, r *http.Request) {
	request := digestPreviewRequest{}
	// An empty body is legitimate here, unlike everywhere else this helper is
	// used: the whole endpoint has sensible defaults for both its fields.
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &request); err != nil {
			writeError(w, err)
			return
		}
	}

	options := usecase.PreviewOptions{ProjectID: request.ProjectID}
	if request.At != "" {
		at, err := time.Parse(time.RFC3339, request.At)
		if err != nil {
			writeError(w, fmt.Errorf("%w: at must be an RFC 3339 instant: %w", errBadRequest, err))
			return
		}
		options.At = at
	}

	schedule, err := s.digest.Schedule(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	report, text, err := s.digest.Preview(r.Context(), options)
	if err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, digestPreviewResponse{
		Schedule: newDigestScheduleBody(schedule),
		Text:     text,
		Report:   report,
	})
}

// handleGetDigestSchedule reports when the weekly report goes out.
func (s *Server) handleGetDigestSchedule(w http.ResponseWriter, r *http.Request) {
	schedule, err := s.digest.Schedule(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newDigestScheduleBody(schedule))
}

// handleSetDigestSchedule changes when the weekly report goes out.
//
// PUT and not PATCH: a schedule is a weekday and an hour, and there is no
// meaningful way to set one without the other — "Tuesday, at whatever hour it
// was" is not a thing anybody means.
func (s *Server) handleSetDigestSchedule(w http.ResponseWriter, r *http.Request) {
	var request digestScheduleBody
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}

	weekday, err := domain.ParseWeekday(request.Weekday)
	if err != nil {
		writeError(w, err)
		return
	}
	schedule, err := domain.NewDigestSchedule(weekday, request.Hour)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.digest.SetSchedule(r.Context(), schedule); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newDigestScheduleBody(schedule))
}

// channelHealthResponse is what `doctor` reads.
type channelHealthResponse struct {
	// Configured says whether this installation has a notification subsystem
	// at all, which is a different answer from "no channels" and leads
	// somewhere different.
	Configured bool                  `json:"configured"`
	Channels   []channelHealthStatus `json:"channels"`
}

// channelHealthStatus is one channel's diagnosis, with the two halves kept
// apart: whether the key opened it, and whether the machine can reach it.
type channelHealthStatus struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	Name      string `json:"name"`
	Digest    bool   `json:"digest"`
	Endpoint  string `json:"endpoint,omitempty"`
	Secret    bool   `json:"secret_ok"`
	Reachable bool   `json:"reachable"`
	OK        bool   `json:"ok"`
	Detail    string `json:"detail"`
}

// handleChannelHealth checks every alert channel, without sending anything.
//
// A GET, and it opens network connections — which is unusual enough to be
// worth naming. It is still safe and still idempotent in the sense that
// matters: it changes nothing here and nothing at the other end. A POST would
// suggest it did.
func (s *Server) handleChannelHealth(w http.ResponseWriter, r *http.Request) {
	response := channelHealthResponse{
		Configured: s.channels.Configured(),
		Channels:   []channelHealthStatus{},
	}

	statuses, err := s.channels.Check(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	for _, status := range statuses {
		response.Channels = append(response.Channels, channelHealthStatus{
			ID:        status.ID,
			Type:      status.Type,
			Name:      status.Name,
			Digest:    status.Digest,
			Endpoint:  status.Endpoint,
			Secret:    status.Secret,
			Reachable: status.Reachable,
			OK:        status.OK(),
			Detail:    status.Detail,
		})
	}
	writeJSON(w, http.StatusOK, response)
}
