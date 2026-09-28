package sentry

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ErrInvalidCheckIn means the payload is not a decodable check-in.
var ErrInvalidCheckIn = fmt.Errorf("%w: not a check-in", ErrInvalidEvent)

// CheckIn is the `check_in` envelope item, as an SDK sends it.
//
// The interesting field is MonitorConfig. Sentry's SDKs let a decorator
// declare the schedule at the same moment they report a run —
// `@monitor(monitor_slug="…", monitor_config={…})` — and the server creates
// the monitor from it. That is what makes instrumenting a cron job one line,
// and it is why this decoder exists rather than a form somebody fills in
// (ADR 016).
type CheckIn struct {
	// CheckInID is the SDK's correlation id: the same value arrives twice,
	// once with in_progress and once with the outcome.
	CheckInID string
	// MonitorSlug is which monitor this is about.
	MonitorSlug string
	// Status is in_progress, ok or error. Validated by the domain, not here:
	// this package's job is to say what arrived.
	Status string
	// DurationSeconds is what the SDK measured, when it said. The protocol
	// sends seconds as a float; a check-in that omits it is normal.
	DurationSeconds float64
	// HasDuration tells "the SDK said zero" from "the SDK said nothing",
	// which a bare float cannot.
	HasDuration bool
	// Environment and Release are where it ran.
	Environment string
	Release     string
	// Config is the declaration, nil when the SDK sent none.
	Config *MonitorConfig
}

// MonitorConfig is the `monitor_config` object, in the units the protocol
// uses rather than the ones the domain does.
//
// The margins are minutes here because that is what an SDK sends. Converting
// them is the use case's job: a decoder that quietly turned minutes into
// seconds would be the kind of unit error that only shows up as a monitor
// reporting missed sixty times too early.
type MonitorConfig struct {
	// ScheduleType is "crontab" or "interval".
	ScheduleType string
	// Crontab is the expression, for a crontab schedule.
	Crontab string
	// IntervalValue and IntervalUnit are the two halves of an interval
	// schedule.
	IntervalValue int
	IntervalUnit  string
	// CheckinMarginMinutes and MaxRuntimeMinutes are optional; nil means the
	// SDK did not say and the server's defaults apply.
	CheckinMarginMinutes *int
	MaxRuntimeMinutes    *int
	// Timezone is an IANA zone name.
	Timezone string
}

// wireCheckIn is the payload's literal shape.
//
// Unknown fields are ignored rather than rejected, unlike the panel's API: an
// SDK that starts sending something new must not break an installation that
// has not been upgraded (ADR 002). This is the same rule the envelope parser
// follows and the opposite of the one the REST API follows, because the two
// have opposite failure modes — here a strict decoder silently loses a
// customer's check-ins, there a lenient one silently loses a typo.
type wireCheckIn struct {
	CheckInID   string          `json:"check_in_id"`
	MonitorSlug string          `json:"monitor_slug"`
	Status      string          `json:"status"`
	Duration    *float64        `json:"duration"`
	Environment string          `json:"environment"`
	Release     string          `json:"release"`
	Config      json.RawMessage `json:"monitor_config"`
}

type wireMonitorConfig struct {
	Schedule      json.RawMessage `json:"schedule"`
	CheckinMargin *int            `json:"checkin_margin"`
	MaxRuntime    *int            `json:"max_runtime"`
	Timezone      string          `json:"timezone"`
}

type wireSchedule struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
	Unit  string          `json:"unit"`
}

// DecodeCheckIn reads a check-in payload.
func DecodeCheckIn(payload []byte) (CheckIn, error) {
	var wire wireCheckIn
	if err := json.Unmarshal(payload, &wire); err != nil {
		return CheckIn{}, fmt.Errorf("%w: %w", ErrInvalidCheckIn, err)
	}
	if strings.TrimSpace(wire.MonitorSlug) == "" {
		return CheckIn{}, fmt.Errorf("%w: monitor_slug is required", ErrInvalidCheckIn)
	}

	checkIn := CheckIn{
		CheckInID:   strings.TrimSpace(wire.CheckInID),
		MonitorSlug: strings.TrimSpace(wire.MonitorSlug),
		Status:      strings.ToLower(strings.TrimSpace(wire.Status)),
		Environment: strings.TrimSpace(wire.Environment),
		Release:     strings.TrimSpace(wire.Release),
	}
	if wire.Duration != nil {
		checkIn.DurationSeconds, checkIn.HasDuration = *wire.Duration, true
	}
	if len(wire.Config) > 0 && string(wire.Config) != "null" {
		config, err := decodeMonitorConfig(wire.Config)
		if err != nil {
			return CheckIn{}, err
		}
		checkIn.Config = config
	}
	return checkIn, nil
}

func decodeMonitorConfig(raw json.RawMessage) (*MonitorConfig, error) {
	var wire wireMonitorConfig
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("%w: monitor_config: %w", ErrInvalidCheckIn, err)
	}

	config := &MonitorConfig{
		CheckinMarginMinutes: wire.CheckinMargin,
		MaxRuntimeMinutes:    wire.MaxRuntime,
		Timezone:             strings.TrimSpace(wire.Timezone),
	}
	if len(wire.Schedule) == 0 || string(wire.Schedule) == "null" {
		// A monitor_config with no schedule is a declaration of nothing. It
		// is not an error — the check-in is still a check-in — so the config
		// is dropped and the monitor, if it exists, keeps the schedule it has.
		return nil, nil
	}

	// Two shapes in the wild. The documented one is an object; several SDKs
	// and every hand-written payload send the bare crontab string, and
	// refusing that would fail on the simplest thing anybody writes.
	var asString string
	if err := json.Unmarshal(wire.Schedule, &asString); err == nil {
		config.ScheduleType, config.Crontab = "crontab", strings.TrimSpace(asString)
		return config, nil
	}

	var schedule wireSchedule
	if err := json.Unmarshal(wire.Schedule, &schedule); err != nil {
		return nil, fmt.Errorf("%w: monitor_config.schedule: %w", ErrInvalidCheckIn, err)
	}
	config.ScheduleType = strings.ToLower(strings.TrimSpace(schedule.Type))
	if config.ScheduleType == "" {
		config.ScheduleType = "crontab"
	}
	config.IntervalUnit = strings.ToLower(strings.TrimSpace(schedule.Unit))

	// The value is a string for a crontab and a number for an interval, in
	// the same field. Both are read, and which one is meaningful is decided
	// by the type — not by which one happened to parse.
	if config.ScheduleType == "interval" {
		var count float64
		if err := json.Unmarshal(schedule.Value, &count); err != nil {
			return nil, fmt.Errorf("%w: an interval schedule needs a numeric value: %w",
				ErrInvalidCheckIn, err)
		}
		config.IntervalValue = int(count)
		return config, nil
	}

	var expression string
	if err := json.Unmarshal(schedule.Value, &expression); err != nil {
		return nil, fmt.Errorf("%w: a crontab schedule needs a string value: %w",
			ErrInvalidCheckIn, err)
	}
	config.Crontab = strings.TrimSpace(expression)
	return config, nil
}
