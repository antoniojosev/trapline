package httpapi

import (
	"net/http"
	"time"

	"github.com/antoniojosev/trapline/internal/scheduler"
)

// jobLister reports the background jobs that are currently running.
//
// An interface, declared here on the consumer side like every other port, so
// this adapter depends on "something that can list jobs" rather than on the
// scheduler. A server built without one is a legitimate shape — a test, or an
// installation running only the API — and answers with an empty list.
type jobLister interface {
	Jobs() []scheduler.Status
}

// WithJobs makes the background jobs visible over the API.
func (s *Server) WithJobs(jobs jobLister) *Server {
	s.jobs = jobs
	return s
}

// jobsResponse wraps the list in an object rather than returning a bare array,
// so the endpoint can grow a field later without changing shape on clients
// that already parse it.
type jobsResponse struct {
	Jobs []jobPayload `json:"jobs"`
}

// jobPayload is one running job.
//
// Only running jobs appear. A subsystem with nothing to do has no row here and
// is not listed as inactive: "it does not exist" is the claim ADR 005 makes
// about a switched-off subsystem, and a row saying otherwise would soften it
// into "it exists but is idle", which is the thing this product refuses to be.
type jobPayload struct {
	Name string `json:"name"`
	// IntervalSeconds rather than a duration string: every other number in
	// this API is a number, and a client that has to parse "1h0m0s" to draw a
	// countdown is being handed a Go detail.
	IntervalSeconds int       `json:"interval_seconds"`
	StartedAt       time.Time `json:"started_at"`
	// LastRun is null until the first pass finishes, and LastError is null
	// while the job is healthy. Null rather than a zero value, so "never ran"
	// and "ran at the epoch" cannot be confused by a client.
	LastRun   *time.Time `json:"last_run"`
	NextRun   *time.Time `json:"next_run"`
	LastError *string    `json:"last_error"`
	Runs      int64      `json:"runs"`
	Failures  int64      `json:"failures"`
}

// handleListJobs reports what is running in the background.
//
// It exists because a job nobody can see is a job nobody notices has been
// failing for a week. The three fields that matter are the ones an operator
// would otherwise have to grep the log for: when it last ran, whether it
// failed, and when it will try again (ADR 014).
func (s *Server) handleListJobs(w http.ResponseWriter, _ *http.Request) {
	response := jobsResponse{Jobs: []jobPayload{}}
	if s.jobs != nil {
		// Indexed rather than ranged by value: a Status is large enough that
		// copying one per iteration is a warning the linter is right to raise.
		statuses := s.jobs.Jobs()
		for index := range statuses {
			response.Jobs = append(response.Jobs, newJobPayload(statuses[index]))
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func newJobPayload(status scheduler.Status) jobPayload {
	payload := jobPayload{
		Name:            status.Name,
		IntervalSeconds: int(status.Interval.Seconds()),
		StartedAt:       status.StartedAt,
		Runs:            status.Runs,
		Failures:        status.Failures,
	}
	if !status.LastRun.IsZero() {
		lastRun := status.LastRun
		payload.LastRun = &lastRun
	}
	if !status.NextRun.IsZero() {
		nextRun := status.NextRun
		payload.NextRun = &nextRun
	}
	if status.LastError != "" {
		lastError := status.LastError
		payload.LastError = &lastError
	}
	return payload
}
