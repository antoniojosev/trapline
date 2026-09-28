package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/usecase"
)

// WithAlerts mounts the alerting endpoints.
//
// Optional in the same way the panel and the releases are: a server built
// without them simply does not have those routes.
func (s *Server) WithAlerts(alerts *usecase.Alerts) *Server {
	s.alerts = alerts
	return s
}

// channelRequest is a channel as a client sends it.
type channelRequest struct {
	Type   string               `json:"type"`
	Name   string               `json:"name"`
	Config domain.ChannelConfig `json:"config"`
	Digest bool                 `json:"digest"`
}

// channelResponse is a channel as the API returns it.
//
// The configuration comes back redacted, always. A bot token that goes in and
// comes out again would make the encryption at rest a formality: the credential
// would simply leak through the API instead of through the file.
type channelResponse struct {
	ID        int64                `json:"id"`
	Type      domain.ChannelType   `json:"type"`
	Name      string               `json:"name"`
	Config    domain.ChannelConfig `json:"config"`
	Digest    bool                 `json:"digest"`
	CreatedAt time.Time            `json:"created_at"`
}

func newChannelResponse(channel *domain.AlertChannel) channelResponse {
	return channelResponse{
		ID:        channel.ID,
		Type:      channel.Type,
		Name:      channel.Name,
		Config:    channel.Redacted(),
		Digest:    channel.Digest,
		CreatedAt: channel.CreatedAt,
	}
}

func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	channels, err := s.alerts.Channels(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	payload := make([]channelResponse, 0, len(channels))
	// Indexed rather than ranged by value: a channel carries its whole
	// configuration, so copying one per iteration is a warning the linter is
	// right to raise.
	for index := range channels {
		payload = append(payload, newChannelResponse(&channels[index]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": payload})
}

func (s *Server) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	var request channelRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	channel, err := s.alerts.AddChannel(r.Context(),
		domain.ChannelType(request.Type), request.Name, request.Config, request.Digest)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, newChannelResponse(&channel))
}

func (s *Server) handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.alerts.RemoveChannel(r.Context(), id); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTestChannel delivers a sample message now and reports what happened.
//
// A failure answers 502 rather than 500: nothing here is wrong with this
// server, and an operator reading the status code should be pointed at the far
// end. The message carries what the provider said, which is the only thing
// that makes a misconfigured channel fixable without a packet capture.
func (s *Server) handleTestChannel(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.alerts.TestChannel(r.Context(), id); err != nil {
		if errors.Is(err, domain.ErrAlertNotFound) {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ruleRequest is a rule as a client sends it.
//
// The trigger arrives as the object it is stored as, not as a string to be
// parsed: there is no query language here, and inventing a syntax for four
// named conditions would be a parser to write, document and keep compatible
// for no gain (ADR 015).
type ruleRequest struct {
	ProjectID      *int64         `json:"project_id"`
	Name           string         `json:"name"`
	Trigger        domain.Trigger `json:"trigger"`
	ChannelIDs     []int64        `json:"channel_ids"`
	SilenceSeconds int            `json:"silence_seconds"`
	Enabled        *bool          `json:"enabled"`
}

type ruleResponse struct {
	ID             int64          `json:"id"`
	ProjectID      *int64         `json:"project_id"`
	Name           string         `json:"name"`
	Trigger        domain.Trigger `json:"trigger"`
	ChannelIDs     []int64        `json:"channel_ids"`
	SilenceSeconds int            `json:"silence_seconds"`
	Enabled        bool           `json:"enabled"`
}

func newRuleResponse(rule domain.AlertRule) ruleResponse {
	return ruleResponse{
		ID:             rule.ID,
		ProjectID:      rule.ProjectID,
		Name:           rule.Name,
		Trigger:        rule.Trigger,
		ChannelIDs:     rule.ChannelIDs,
		SilenceSeconds: rule.SilenceSeconds,
		Enabled:        rule.Enabled,
	}
}

func (s *Server) handleListRules(w http.ResponseWriter, r *http.Request) {
	var projectID *int64
	if raw := r.URL.Query().Get("project_id"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeError(w, fmt.Errorf("%w: project_id must be a positive integer, got %q", errBadRequest, raw))
			return
		}
		projectID = &parsed
	}
	rules, err := s.alerts.Rules(r.Context(), projectID)
	if err != nil {
		writeError(w, err)
		return
	}
	payload := make([]ruleResponse, 0, len(rules))
	for _, rule := range rules {
		payload = append(payload, newRuleResponse(rule))
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": payload})
}

func (s *Server) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	var request ruleRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	// A rule nobody asked to switch off is on. Creating something that does
	// nothing until a second request is the kind of API that produces a
	// support conversation per user.
	enabled := true
	if request.Enabled != nil {
		enabled = *request.Enabled
	}
	rule, err := s.alerts.AddRule(r.Context(), request.ProjectID, request.Name, request.Trigger,
		request.ChannelIDs, time.Duration(request.SilenceSeconds)*time.Second, enabled)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, newRuleResponse(rule))
}

func (s *Server) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.alerts.RemoveRule(r.Context(), id); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTestRule sends a synthetic alert through every channel a rule names.
func (s *Server) handleTestRule(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	results, err := s.alerts.TestRule(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	ok := true
	for _, result := range results {
		if !result.OK {
			ok = false
		}
	}
	// 200 either way, with the per-channel verdict in the body. The request
	// succeeded — the answer to "does this rule deliver" is the payload, and
	// an error status would make a caller throw away the part that says which
	// channel is broken.
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "results": results})
}

// notificationResponse is one row of the delivery log.
type notificationResponse struct {
	ID            int64                     `json:"id"`
	RuleID        int64                     `json:"rule_id"`
	ChannelID     int64                     `json:"channel_id"`
	SubjectKey    string                    `json:"subject_key"`
	Status        domain.NotificationStatus `json:"status"`
	Attempts      int                       `json:"attempts"`
	NextAttemptAt time.Time                 `json:"next_attempt_at"`
	LastError     string                    `json:"last_error,omitempty"`
	CreatedAt     time.Time                 `json:"created_at"`
	SentAt        *time.Time                `json:"sent_at"`
	Payload       domain.AlertPayload       `json:"payload"`
}

func newNotificationResponse(notification *domain.Notification) notificationResponse {
	return notificationResponse{
		ID:            notification.ID,
		RuleID:        notification.RuleID,
		ChannelID:     notification.ChannelID,
		SubjectKey:    notification.SubjectKey,
		Status:        notification.Status,
		Attempts:      notification.Attempts,
		NextAttemptAt: notification.NextAttemptAt,
		LastError:     notification.LastError,
		CreatedAt:     notification.CreatedAt,
		SentAt:        notification.SentAt,
		Payload:       notification.Payload,
	}
}

func (s *Server) handleListNotifications(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeError(w, fmt.Errorf("%w: limit must be a positive integer, got %q", errBadRequest, raw))
			return
		}
		limit = parsed
	}
	notifications, err := s.alerts.Notifications(r.Context(), r.URL.Query().Get("status"), limit)
	if err != nil {
		writeError(w, err)
		return
	}
	payload := make([]notificationResponse, 0, len(notifications))
	for index := range notifications {
		payload = append(payload, newNotificationResponse(&notifications[index]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"notifications": payload})
}

func (s *Server) handleRetryNotification(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	notification, err := s.alerts.Retry(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newNotificationResponse(&notification))
}
