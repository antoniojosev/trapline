package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/usecase"
)

// WithCrons mounts the cron-monitoring endpoints and the ping surface.
//
// Optional in the same way the panel, the releases and the alerts are: a
// server built without it simply does not have those routes — including
// /ping/, which is the one public path this product adds outside the
// protocol's own.
func (s *Server) WithCrons(crons *usecase.Crons) *Server {
	s.crons = crons
	return s
}

// monitorRequest is a cron monitor as a client sends it.
type monitorRequest struct {
	Slug string `json:"slug"`
	// ScheduleType is crontab or interval; empty means crontab, because that
	// is what somebody pasting a line out of their crontab has.
	ScheduleType string `json:"schedule_type,omitempty"`
	Schedule     string `json:"schedule"`
	Timezone     string `json:"timezone,omitempty"`
	// The two margins are seconds here, unlike the protocol's monitor_config
	// which uses minutes. Seconds because this is the API a person and an
	// agent drive, and every other duration in it is seconds; the conversion
	// from the protocol's units happens once, on the ingest path.
	CheckinMarginSeconds int   `json:"checkin_margin_s,omitempty"`
	MaxRuntimeSeconds    int   `json:"max_runtime_s,omitempty"`
	Enabled              *bool `json:"enabled,omitempty"`
}

// monitorUpdateRequest is a partial edit. Every field distinguishes absent
// from present, so "leave the schedule alone" and "set the schedule to this"
// are different requests rather than the same one.
type monitorUpdateRequest struct {
	ScheduleType         optional[string] `json:"schedule_type"`
	Schedule             optional[string] `json:"schedule"`
	Timezone             optional[string] `json:"timezone"`
	CheckinMarginSeconds optional[int]    `json:"checkin_margin_s"`
	MaxRuntimeSeconds    optional[int]    `json:"max_runtime_s"`
	Enabled              optional[bool]   `json:"enabled"`
}

// monitorResponse is a monitor as the API returns it.
//
// The ping key is included, and it is the one credential in this product that
// deliberately comes back out through the API it went in through. It has to:
// the whole feature is a URL somebody pastes at the end of a crontab line, and
// a key that could only be seen once would mean losing it costs a new monitor
// and an edit to a server's crontab. What protects it instead is the scope —
// monitors:read is its own permission for exactly this reason (domain/token.go)
// — and the fact that what it can do is report a job as having run.
type monitorResponse struct {
	ID        int64  `json:"id"`
	ProjectID int64  `json:"project_id"`
	Slug      string `json:"slug"`
	PingKey   string `json:"ping_key"`
	// PingURL is the whole point of the ping key, assembled once here so that
	// three clients do not each build it from an origin and a template.
	PingURL              string              `json:"ping_url"`
	ScheduleType         domain.ScheduleType `json:"schedule_type"`
	Schedule             string              `json:"schedule"`
	Timezone             string              `json:"timezone"`
	CheckinMarginSeconds int                 `json:"checkin_margin_s"`
	MaxRuntimeSeconds    int                 `json:"max_runtime_s"`
	Status               domain.CronStatus   `json:"status"`
	LastCheckinAt        *time.Time          `json:"last_checkin_at,omitempty"`
	NextExpectedAt       *time.Time          `json:"next_expected_at,omitempty"`
	Enabled              bool                `json:"enabled"`
	CreatedAt            time.Time           `json:"created_at"`
}

func (s *Server) newMonitorResponse(monitor *domain.CronMonitor) monitorResponse {
	return monitorResponse{
		ID:                   monitor.ID,
		ProjectID:            monitor.ProjectID,
		Slug:                 monitor.Slug,
		PingKey:              monitor.PingKey,
		PingURL:              s.origin.String() + pingPrefix + monitor.PingKey,
		ScheduleType:         monitor.Schedule.Type,
		Schedule:             monitor.Schedule.String(),
		Timezone:             monitor.Timezone,
		CheckinMarginSeconds: int(monitor.CheckinMargin.Seconds()),
		MaxRuntimeSeconds:    int(monitor.MaxRuntime.Seconds()),
		Status:               monitor.Status,
		LastCheckinAt:        monitor.LastCheckinAt,
		NextExpectedAt:       monitor.NextExpectedAt,
		Enabled:              monitor.Enabled,
		CreatedAt:            monitor.CreatedAt,
	}
}

// checkInResponse is one reported run.
type checkInResponse struct {
	ID          int64                `json:"id"`
	MonitorID   int64                `json:"monitor_id"`
	CheckInID   string               `json:"checkin_id,omitempty"`
	Status      domain.CheckInStatus `json:"status"`
	StartedAt   time.Time            `json:"started_at"`
	FinishedAt  *time.Time           `json:"finished_at,omitempty"`
	DurationMS  int64                `json:"duration_ms"`
	Environment string               `json:"environment,omitempty"`
}

func (s *Server) handleListCronMonitors(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	monitors, err := s.crons.Monitors(r.Context(), projectID)
	if err != nil {
		writeError(w, err)
		return
	}
	payload := make([]monitorResponse, 0, len(monitors))
	// Indexed rather than ranged by value: a monitor carries its parsed
	// schedule, so copying one per iteration is a warning the linter is right
	// to raise.
	for index := range monitors {
		payload = append(payload, s.newMonitorResponse(&monitors[index]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"monitors": payload})
}

func (s *Server) handleCreateCronMonitor(w http.ResponseWriter, r *http.Request) {
	projectID, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	var request monitorRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}

	// A monitor created without saying is watched. Creating one switched off
	// is a legitimate thing to want — staging a change before the job exists
	// — but it is not the common case, and a monitor that quietly watches
	// nothing is the failure this feature is meant to catch.
	enabled := true
	if request.Enabled != nil {
		enabled = *request.Enabled
	}

	monitor, err := s.crons.Add(r.Context(), projectID, usecase.MonitorSpec{
		Slug:          request.Slug,
		ScheduleType:  request.ScheduleType,
		Schedule:      request.Schedule,
		Timezone:      request.Timezone,
		CheckinMargin: time.Duration(request.CheckinMarginSeconds) * time.Second,
		MaxRuntime:    time.Duration(request.MaxRuntimeSeconds) * time.Second,
		Enabled:       enabled,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.newMonitorResponse(&monitor))
}

func (s *Server) handleGetCronMonitor(w http.ResponseWriter, r *http.Request) {
	monitor, err := s.scopedMonitor(r)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.newMonitorResponse(&monitor))
}

func (s *Server) handleUpdateCronMonitor(w http.ResponseWriter, r *http.Request) {
	monitor, err := s.scopedMonitor(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var request monitorUpdateRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}

	changes := usecase.MonitorChanges{
		ScheduleType: request.ScheduleType.Value,
		Schedule:     request.Schedule.Value,
		Timezone:     request.Timezone.Value,
		Enabled:      request.Enabled.Value,
	}
	if request.CheckinMarginSeconds.Value != nil {
		margin := time.Duration(*request.CheckinMarginSeconds.Value) * time.Second
		changes.CheckinMargin = &margin
	}
	if request.MaxRuntimeSeconds.Value != nil {
		runtime := time.Duration(*request.MaxRuntimeSeconds.Value) * time.Second
		changes.MaxRuntime = &runtime
	}

	updated, err := s.crons.Update(r.Context(), monitor.ID, changes)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.newMonitorResponse(&updated))
}

func (s *Server) handleDeleteCronMonitor(w http.ResponseWriter, r *http.Request) {
	monitor, err := s.scopedMonitor(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.crons.Remove(r.Context(), monitor.ID); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListCheckIns(w http.ResponseWriter, r *http.Request) {
	monitor, err := s.scopedMonitor(r)
	if err != nil {
		writeError(w, err)
		return
	}
	limit, err := checkInLimit(r)
	if err != nil {
		writeError(w, err)
		return
	}

	checkIns, err := s.crons.CheckIns(r.Context(), monitor.ID, limit)
	if err != nil {
		writeError(w, err)
		return
	}
	payload := make([]checkInResponse, 0, len(checkIns))
	for index := range checkIns {
		checkIn := &checkIns[index]
		payload = append(payload, checkInResponse{
			ID:          checkIn.ID,
			MonitorID:   checkIn.MonitorID,
			CheckInID:   checkIn.CheckInID,
			Status:      checkIn.Status,
			StartedAt:   checkIn.StartedAt,
			FinishedAt:  checkIn.FinishedAt,
			DurationMS:  checkIn.DurationMS,
			Environment: checkIn.Environment,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"checkins": payload})
}

// scopedMonitor reads the monitor named in the path and refuses one that
// belongs to a different project than the path says.
//
// The check is not decoration. Without it, a monitor id from any project
// would be readable — and writable — through any project's URL, which would
// make the project a label rather than a boundary. The refusal is
// ErrMonitorNotFound rather than a forbidden, so the API does not confirm
// which ids exist elsewhere.
func (s *Server) scopedMonitor(r *http.Request) (domain.CronMonitor, error) {
	projectID, err := pathID(r, "id")
	if err != nil {
		return domain.CronMonitor{}, err
	}
	monitorID, err := pathID(r, "monitorID")
	if err != nil {
		return domain.CronMonitor{}, err
	}
	monitor, err := s.crons.Monitor(r.Context(), monitorID)
	if err != nil {
		return domain.CronMonitor{}, err
	}
	if monitor.ProjectID != projectID {
		return domain.CronMonitor{}, domain.ErrMonitorNotFound
	}
	return monitor, nil
}

func checkInLimit(r *http.Request) (int64, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return 0, nil
	}
	limit, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || limit < 0 {
		return 0, fmt.Errorf("%w: limit must be a non-negative integer, got %q", errBadRequest, raw)
	}
	return limit, nil
}
