package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/usecase"
)

// WithUptime mounts the uptime monitoring endpoints.
//
// Optional in the same way the panel, the releases and the alerts are: a
// server built without it simply does not have those routes, which is the
// truthful description of a build that cannot check anything.
func (s *Server) WithUptime(uptime *usecase.Uptime) *Server {
	s.uptime = uptime
	return s
}

// uptimeMonitorRequest is a monitor as a client sends it.
//
// Every field except the URL has a default, and the defaults are the domain's
// rather than this file's: a client that sends only a name and a URL gets a
// GET every sixty seconds expecting a 2xx, which is what somebody adding their
// first monitor means.
type uptimeMonitorRequest struct {
	Name                  string `json:"name"`
	URL                   string `json:"url"`
	Method                string `json:"method"`
	IntervalSeconds       int    `json:"interval_s"`
	TimeoutSeconds        int    `json:"timeout_s"`
	ExpectedStatusMin     int    `json:"expected_status_min"`
	ExpectedStatusMax     int    `json:"expected_status_max"`
	ExpectedBodySubstring string `json:"expected_body_substring"`
	FollowRedirects       *bool  `json:"follow_redirects"`
	// AllowPrivate is this monitor's half of the permission to reach an
	// address that is not globally routable. It grants nothing on its own:
	// the server must also have been started with -uptime-allow-private, and
	// a request that sets this on an installation that was not gets a 422
	// saying so (ADR 016).
	AllowPrivate bool  `json:"allow_private"`
	Public       bool  `json:"public"`
	Enabled      *bool `json:"enabled"`
}

// uptimeMonitorResponse is a monitor as the API returns it.
type uptimeMonitorResponse struct {
	ID                    int64               `json:"id"`
	ProjectID             int64               `json:"project_id"`
	Name                  string              `json:"name"`
	URL                   string              `json:"url"`
	Method                string              `json:"method"`
	IntervalSeconds       int                 `json:"interval_s"`
	TimeoutSeconds        int                 `json:"timeout_s"`
	ExpectedStatusMin     int                 `json:"expected_status_min"`
	ExpectedStatusMax     int                 `json:"expected_status_max"`
	ExpectedBodySubstring string              `json:"expected_body_substring,omitempty"`
	FollowRedirects       bool                `json:"follow_redirects"`
	AllowPrivate          bool                `json:"allow_private"`
	Public                bool                `json:"public"`
	Enabled               bool                `json:"enabled"`
	Status                domain.UptimeStatus `json:"status"`
	ConsecutiveFailures   int                 `json:"consecutive_failures"`
	LastCheckedAt         *time.Time          `json:"last_checked_at"`
	NextCheckAt           time.Time           `json:"next_check_at"`
	LastStatusChangeAt    *time.Time          `json:"last_status_change_at"`
	CreatedAt             time.Time           `json:"created_at"`
}

func newMonitorResponse(monitor *domain.UptimeMonitor) uptimeMonitorResponse {
	return uptimeMonitorResponse{
		ID:                    monitor.ID,
		ProjectID:             monitor.ProjectID,
		Name:                  monitor.Name,
		URL:                   monitor.URL,
		Method:                monitor.Method,
		IntervalSeconds:       monitor.IntervalSeconds,
		TimeoutSeconds:        monitor.TimeoutSeconds,
		ExpectedStatusMin:     monitor.ExpectedStatusMin,
		ExpectedStatusMax:     monitor.ExpectedStatusMax,
		ExpectedBodySubstring: monitor.ExpectedBodySubstring,
		FollowRedirects:       monitor.FollowRedirects,
		AllowPrivate:          monitor.AllowPrivate,
		Public:                monitor.Public,
		Enabled:               monitor.Enabled,
		Status:                monitor.Status,
		ConsecutiveFailures:   monitor.ConsecutiveFailures,
		LastCheckedAt:         monitor.LastCheckedAt,
		NextCheckAt:           monitor.NextCheckAt,
		LastStatusChangeAt:    monitor.LastStatusChangeAt,
		CreatedAt:             monitor.CreatedAt,
	}
}

func (s *Server) handleCreateUptimeMonitor(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	var request uptimeMonitorRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}

	// A monitor nobody asked to switch off is on, and one nobody asked about
	// redirects follows them. Creating something that does nothing until a
	// second request is the kind of API that produces a support conversation
	// per user.
	enabled, follow := true, true
	if request.Enabled != nil {
		enabled = *request.Enabled
	}
	if request.FollowRedirects != nil {
		follow = *request.FollowRedirects
	}

	monitor, err := s.uptime.AddMonitor(r.Context(), &domain.UptimeMonitor{
		ProjectID:             projectID,
		Name:                  request.Name,
		URL:                   request.URL,
		Method:                request.Method,
		IntervalSeconds:       request.IntervalSeconds,
		TimeoutSeconds:        request.TimeoutSeconds,
		ExpectedStatusMin:     request.ExpectedStatusMin,
		ExpectedStatusMax:     request.ExpectedStatusMax,
		ExpectedBodySubstring: request.ExpectedBodySubstring,
		FollowRedirects:       follow,
		AllowPrivate:          request.AllowPrivate,
		Public:                request.Public,
		Enabled:               enabled,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, newMonitorResponse(&monitor))
}

func (s *Server) handleListUptimeMonitors(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	monitors, err := s.uptime.Monitors(r.Context(), &projectID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"monitors": monitorList(monitors)})
}

func monitorList(monitors []domain.UptimeMonitor) []uptimeMonitorResponse {
	payload := make([]uptimeMonitorResponse, 0, len(monitors))
	// Indexed rather than ranged by value: a monitor is a large struct, and
	// copying one per iteration is a warning the linter is right to raise.
	for index := range monitors {
		payload = append(payload, newMonitorResponse(&monitors[index]))
	}
	return payload
}

func (s *Server) handleGetUptimeMonitor(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "monitorID")
	if err != nil {
		writeError(w, err)
		return
	}
	monitor, err := s.uptime.Monitor(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newMonitorResponse(&monitor))
}

func (s *Server) handleDeleteUptimeMonitor(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "monitorID")
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.uptime.RemoveMonitor(r.Context(), id); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSetUptimeMonitorEnabled switches a monitor on or off.
//
// A route of its own rather than a general update, because it is the one
// change an operator makes in a hurry — a target is being deployed and the
// alerts are noise for ten minutes — and because switching off must keep the
// history rather than deleting it, which a PUT of the whole resource would
// invite somebody to get wrong.
func (s *Server) handleSetUptimeMonitorEnabled(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "monitorID")
	if err != nil {
		writeError(w, err)
		return
	}
	var request struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	monitor, err := s.uptime.SetEnabled(r.Context(), id, request.Enabled)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newMonitorResponse(&monitor))
}

// checkResponse is one check as the API returns it.
type checkResponse struct {
	At         time.Time `json:"at"`
	OK         bool      `json:"ok"`
	StatusCode int       `json:"status_code"`
	LatencyMS  int       `json:"latency_ms"`
	Error      string    `json:"error,omitempty"`
}

func (s *Server) handleUptimeResults(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "monitorID")
	if err != nil {
		writeError(w, err)
		return
	}
	limit, err := positiveQuery(r, "limit", 50)
	if err != nil {
		writeError(w, err)
		return
	}
	results, err := s.uptime.Results(r.Context(), id, int64(limit))
	if err != nil {
		writeError(w, err)
		return
	}
	payload := make([]checkResponse, 0, len(results))
	for _, result := range results {
		payload = append(payload, checkResponse{
			At: result.At, OK: result.OK, StatusCode: result.StatusCode,
			LatencyMS: result.LatencyMS, Error: result.Error,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": payload})
}

// dayResponse is one day of the roll-up.
//
// The percentage and the mean are computed here from the stored sums rather
// than stored: a stored average cannot be merged with another period's, and
// every range the status page draws is a merge (ADR 007's rule, ADR 017).
type dayResponse struct {
	Day           string  `json:"day"`
	Checks        int64   `json:"checks"`
	Failures      int64   `json:"failures"`
	Uptime        float64 `json:"uptime"`
	MeanLatencyMS int64   `json:"mean_latency_ms"`
}

func (s *Server) handleUptimeDaily(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "monitorID")
	if err != nil {
		writeError(w, err)
		return
	}
	days, err := positiveQuery(r, "days", 90)
	if err != nil {
		writeError(w, err)
		return
	}
	history, err := s.uptime.Daily(r.Context(), id, days)
	if err != nil {
		writeError(w, err)
		return
	}
	payload := make([]dayResponse, 0, len(history))
	for _, day := range history {
		payload = append(payload, dayResponse{
			Day:           domain.UptimeDayKey(day.Day),
			Checks:        day.Checks,
			Failures:      day.Failures,
			Uptime:        day.Uptime(),
			MeanLatencyMS: day.MeanLatencyMS(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": payload})
}

// positiveQuery reads an optional positive integer from the query string.
func positiveQuery(r *http.Request, name string, fallback int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%w: %s must be a positive integer, got %q", errBadRequest, name, raw)
	}
	return parsed, nil
}
