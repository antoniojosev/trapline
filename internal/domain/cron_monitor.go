package domain

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The five states a cron monitor can be in.
//
// Four of them are outcomes and one — unknown — is the absence of one. A
// monitor that has never been heard from is not healthy and is not broken:
// saying "ok" before the first check-in would make a status page green for a
// backup that has never run, and saying "missed" would page somebody the
// moment they created the monitor.
const (
	// CronUnknown means nothing has checked in yet.
	CronUnknown CronStatus = "unknown"
	// CronOK means the last run finished and reported success.
	CronOK CronStatus = "ok"
	// CronMissed means the run did not start within its margin.
	CronMissed CronStatus = "missed"
	// CronTimeout means a run started and never reported finishing.
	CronTimeout CronStatus = "timeout"
	// CronError means a run finished and reported failure.
	CronError CronStatus = "error"
)

// CronStatus is where a monitor stands.
type CronStatus string

// AllCronStatuses lists every status, for validating a filter.
func AllCronStatuses() []CronStatus {
	return []CronStatus{CronUnknown, CronOK, CronMissed, CronTimeout, CronError}
}

// Valid reports whether s is a status this build stores.
func (s CronStatus) Valid() bool { return slices.Contains(AllCronStatuses(), s) }

// Failing reports whether a status is one somebody should be told about. It
// is the test behind cron_recovered: recovery is a transition out of this set.
func (s CronStatus) Failing() bool {
	return s == CronMissed || s == CronTimeout || s == CronError
}

// The three states one check-in can be in. They are the protocol's own names
// (ADR 002): an SDK sends these strings and this product does not get to
// rename them.
const (
	// CheckInProgress is a run that has announced its start.
	CheckInProgress CheckInStatus = "in_progress"
	// CheckInOK is a run that finished successfully.
	CheckInOK CheckInStatus = "ok"
	// CheckInError is a run that finished and failed.
	CheckInError CheckInStatus = "error"
)

// CheckInStatus is what one check-in reported.
type CheckInStatus string

// AllCheckInStatuses lists every check-in status.
func AllCheckInStatuses() []CheckInStatus {
	return []CheckInStatus{CheckInProgress, CheckInOK, CheckInError}
}

// Valid reports whether s is a check-in status this build accepts.
func (s CheckInStatus) Valid() bool { return slices.Contains(AllCheckInStatuses(), s) }

// The two ways a schedule can be written.
const (
	// ScheduleCrontab is a five-field crontab expression.
	ScheduleCrontab ScheduleType = "crontab"
	// ScheduleInterval is "every N units", counted from the last check-in.
	ScheduleInterval ScheduleType = "interval"
)

// ScheduleType names how a schedule is written.
type ScheduleType string

// Valid reports whether t is a schedule type this build understands.
func (t ScheduleType) Valid() bool { return t == ScheduleCrontab || t == ScheduleInterval }

// The bounds and defaults of a monitor.
const (
	// MaxMonitorSlugLen bounds the slug. It is the identity an SDK sends on
	// every check-in, so it lives in a payload and in a URL.
	MaxMonitorSlugLen = 64
	// PingKeyBytes is the entropy of a ping key. Sixteen bytes, rendered as
	// thirty-two hex characters: the key *is* the authentication for
	// /ping/{key} (ADR 016), so it has to be a credential and not an id.
	PingKeyBytes = 16
	// DefaultCheckinMargin is how late a run may be before it counts as
	// missed, when the monitor does not say. A minute, because a machine
	// whose clock is a few seconds out, or a cron daemon that started a
	// job under load, is normal and is not an incident.
	DefaultCheckinMargin = time.Minute
	// MaxCheckinMargin bounds it. A margin longer than a day would mean a
	// daily job could be skipped entirely without anybody being told.
	MaxCheckinMargin = 24 * time.Hour
	// DefaultMaxRuntime is how long a started run may go without reporting
	// that it finished. Thirty minutes covers a database backup; anything
	// longer is a deliberate choice somebody should make on purpose.
	DefaultMaxRuntime = 30 * time.Minute
	// MaxMaxRuntime bounds it at a day, for the same reason as the margin.
	MaxMaxRuntime = 24 * time.Hour
	// MinInterval is the shortest interval schedule. A monitor that expects
	// a check-in more often than the watcher sweeps could be declared missed
	// before it has had a chance to report.
	MinInterval = time.Minute
	// MaxCheckInIDLen bounds the id an SDK sends to correlate a start with
	// its finish.
	MaxCheckInIDLen = 64
	// MaxEnvironmentLen bounds the environment a check-in names.
	MaxEnvironmentLen = 64
)

// intervalUnits are the units an interval schedule may be written in, and how
// many of them make one step.
//
// Months and years are here even though they are not fixed durations, because
// a monitor for a quarterly job is a real thing and expressing it as "90 days"
// would drift. They are applied with calendar arithmetic rather than by
// multiplying a duration, which is why this is a list of names and not a map
// of durations.
var intervalUnits = []string{"minute", "hour", "day", "week", "month", "year"}

// IntervalUnits lists the units an interval schedule accepts.
func IntervalUnits() []string { return slices.Clone(intervalUnits) }

// CronSpec is a monitor's schedule, in either of the two forms it can take.
//
// One type rather than two, because every caller asks it the same question —
// when is the next one due — and a caller that had to switch on the type
// first would be a caller that could forget to.
type CronSpec struct {
	// Type is which of the two forms this is.
	Type ScheduleType
	// Cron is the parsed expression, for a crontab schedule.
	Cron CronSchedule
	// Every is how many Unit steps make one period, for an interval schedule.
	Every int
	// Unit is the unit those steps are counted in.
	Unit string
}

// ParseCronSpec reads a schedule of either type.
//
// The value of an interval schedule is written "<n> <unit>" — "5 minute",
// "1 day" — which is the same information an SDK's monitor_config carries as
// two fields, in the one string the schema stores (ADR 016). Keeping it as
// one column means adding a third schedule type later costs a parser and not
// a migration.
func ParseCronSpec(kind ScheduleType, value string) (CronSpec, error) {
	switch kind {
	case ScheduleCrontab:
		parsed, err := ParseCron(value)
		if err != nil {
			return CronSpec{}, err
		}
		return CronSpec{Type: ScheduleCrontab, Cron: parsed}, nil

	case ScheduleInterval:
		fields := strings.Fields(strings.ToLower(strings.TrimSpace(value)))
		if len(fields) != 2 {
			return CronSpec{}, fmt.Errorf(`%w: an interval schedule is written "<n> <unit>", got %q`,
				ErrInvalidMonitor, value)
		}
		every, err := strconv.Atoi(fields[0])
		if err != nil || every <= 0 {
			return CronSpec{}, fmt.Errorf("%w: an interval needs a positive count, got %q",
				ErrInvalidMonitor, fields[0])
		}
		// Both spellings, because "5 minutes" is what a person writes and
		// "minute" is what the protocol sends.
		unit := strings.TrimSuffix(fields[1], "s")
		if !slices.Contains(intervalUnits, unit) {
			return CronSpec{}, fmt.Errorf("%w: unknown interval unit %q, expected one of %s",
				ErrInvalidMonitor, fields[1], strings.Join(intervalUnits, ", "))
		}
		spec := CronSpec{Type: ScheduleInterval, Every: every, Unit: unit}
		// Months and years have no fixed length, so step reports zero for
		// them and there is nothing to compare: a monthly interval is
		// comfortably above any floor by construction.
		if step := spec.step(); step > 0 && step < MinInterval {
			return CronSpec{}, fmt.Errorf("%w: an interval shorter than %s cannot be watched reliably",
				ErrInvalidMonitor, MinInterval)
		}
		return spec, nil

	default:
		return CronSpec{}, fmt.Errorf("%w: unknown schedule type %q, expected %s or %s",
			ErrInvalidMonitor, kind, ScheduleCrontab, ScheduleInterval)
	}
}

// step is an interval's period as a duration, for the units that have one.
// Months and years return zero, which is why Next uses calendar arithmetic
// rather than this.
func (s CronSpec) step() time.Duration {
	switch s.Unit {
	case "minute":
		return time.Duration(s.Every) * time.Minute
	case "hour":
		return time.Duration(s.Every) * time.Hour
	case "day":
		return time.Duration(s.Every) * 24 * time.Hour
	case "week":
		return time.Duration(s.Every) * 7 * 24 * time.Hour
	default:
		return 0
	}
}

// String renders the schedule as it is stored and shown.
func (s CronSpec) String() string {
	if s.Type == ScheduleInterval {
		return strconv.Itoa(s.Every) + " " + s.Unit
	}
	return s.Cron.String()
}

// Next is when a run is due after `after`, read in loc.
//
// A crontab schedule answers from the calendar: the next matching minute,
// whatever happened last time. An interval answers from `after` itself, which
// is the difference between the two and the reason both exist — "every six
// hours" measured from the last run is what somebody with a job that
// sometimes takes two hours means, and measuring it from a fixed grid would
// declare it missed on the day it ran slowly.
func (s CronSpec) Next(after time.Time, loc *time.Location) (time.Time, error) {
	if loc == nil {
		loc = time.UTC
	}
	switch s.Type {
	case ScheduleCrontab:
		return s.Cron.Next(after, loc)
	case ScheduleInterval:
		local := after.In(loc)
		switch s.Unit {
		case "month":
			return local.AddDate(0, s.Every, 0), nil
		case "year":
			return local.AddDate(s.Every, 0, 0), nil
		default:
			return local.Add(s.step()), nil
		}
	default:
		return time.Time{}, fmt.Errorf("%w: unknown schedule type %q", ErrInvalidMonitor, s.Type)
	}
}

// CronMonitor is one scheduled job this installation watches.
type CronMonitor struct {
	// ID is the storage identity, zero before it is saved.
	ID int64
	// ProjectID is whose monitor it is.
	ProjectID int64
	// Slug identifies the monitor to an SDK. Unique per project, because that
	// is the key a check-in arrives with.
	Slug string
	// PingKey is the secret in /ping/{key}. It is the whole authentication of
	// that endpoint, which is why it is minted from crypto/rand and never
	// derived from the slug.
	PingKey string
	// Schedule is when a run is expected.
	Schedule CronSpec
	// Timezone is the IANA zone the schedule is read in. Never a fixed
	// offset: an offset is a fact about one instant, and a schedule outlives
	// the next time the clocks change.
	Timezone string
	// CheckinMargin is how late a run may be before it counts as missed.
	CheckinMargin time.Duration
	// MaxRuntime is how long a started run may go without finishing.
	MaxRuntime time.Duration
	// Status is where the monitor stands.
	Status CronStatus
	// LastCheckinAt is when anything was last heard from it.
	LastCheckinAt *time.Time
	// NextExpectedAt is when the next run is due, nil until something is
	// known. It is stored rather than derived on read so the watcher's sweep
	// is an indexed range query instead of a schedule evaluation per monitor.
	NextExpectedAt *time.Time
	// Enabled is whether it is watched at all. A disabled monitor answers 410
	// to a ping, so a script that keeps pinging one somebody switched off is
	// told, rather than pinging into silence.
	Enabled bool
	// CreatedAt is when it was declared.
	CreatedAt time.Time
}

// Location resolves the monitor's zone.
//
// An error here means the database names a zone this machine's tzdata does
// not have, which is a real condition on a minimal container image and is
// reported rather than silently treated as UTC: a backup monitor that
// silently moved four and a half hours is worse than one that says it cannot
// be evaluated.
func (m *CronMonitor) Location() (*time.Location, error) {
	if m.Timezone == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(m.Timezone)
	if err != nil {
		return nil, fmt.Errorf("%w: unknown timezone %q: %w", ErrInvalidMonitor, m.Timezone, err)
	}
	return loc, nil
}

// NewCronMonitor validates a monitor before it can be stored.
//
// The zero durations are filled with the defaults rather than rejected: the
// commonest way a monitor comes into existence is an SDK declaring it from a
// decorator that names a schedule and nothing else, and refusing that would
// mean instrumenting a cron job is not one line after all (ADR 016).
func NewCronMonitor(
	projectID int64, slug string, schedule CronSpec, timezone string,
	margin, maxRuntime time.Duration, enabled bool, now time.Time,
) (CronMonitor, error) {
	if projectID <= 0 {
		return CronMonitor{}, fmt.Errorf("%w: %d is not a project id", ErrInvalidMonitor, projectID)
	}
	cleanSlug, err := CleanMonitorSlug(slug)
	if err != nil {
		return CronMonitor{}, err
	}
	if !schedule.Type.Valid() {
		return CronMonitor{}, fmt.Errorf("%w: unknown schedule type %q", ErrInvalidMonitor, schedule.Type)
	}
	zone, err := CleanTimezone(timezone)
	if err != nil {
		return CronMonitor{}, err
	}
	if margin == 0 {
		margin = DefaultCheckinMargin
	}
	if margin < 0 || margin > MaxCheckinMargin {
		return CronMonitor{}, fmt.Errorf("%w: the check-in margin must be between 0 and %s, got %s",
			ErrInvalidMonitor, MaxCheckinMargin, margin)
	}
	if maxRuntime == 0 {
		maxRuntime = DefaultMaxRuntime
	}
	if maxRuntime <= 0 || maxRuntime > MaxMaxRuntime {
		return CronMonitor{}, fmt.Errorf("%w: the maximum runtime must be between 1s and %s, got %s",
			ErrInvalidMonitor, MaxMaxRuntime, maxRuntime)
	}

	key, err := NewPingKey()
	if err != nil {
		return CronMonitor{}, err
	}

	monitor := CronMonitor{
		ProjectID:     projectID,
		Slug:          cleanSlug,
		PingKey:       key,
		Schedule:      schedule,
		Timezone:      zone,
		CheckinMargin: margin,
		MaxRuntime:    maxRuntime,
		// Never ok. A monitor that has just been declared has told nobody
		// anything, and a green light for a backup that has never run is the
		// single most expensive lie a status page can tell.
		Status:    CronUnknown,
		Enabled:   enabled,
		CreatedAt: now.UTC(),
	}
	// The first deadline is computed at creation and not at the first
	// check-in, so a job that never runs at all is still missed. A monitor
	// that only starts watching once it has been told something would go on
	// saying nothing about the backup that stopped the day it was set up.
	loc, err := monitor.Location()
	if err != nil {
		return CronMonitor{}, err
	}
	next, err := schedule.Next(now, loc)
	if err != nil {
		return CronMonitor{}, err
	}
	next = next.UTC()
	monitor.NextExpectedAt = &next
	return monitor, nil
}

// CleanMonitorSlug validates the identity an SDK sends.
//
// Not Slugify: this string is a key an SDK already holds, and rewriting it
// would mean a check-in from `my_backup` created one monitor and a check-in
// from `my-backup` created another, with a person looking at two rows for one
// job. Refusing what cannot be a slug is the honest half of that trade.
func CleanMonitorSlug(raw string) (string, error) {
	slug := strings.ToLower(strings.TrimSpace(raw))
	if slug == "" {
		return "", fmt.Errorf("%w: a monitor needs a slug", ErrInvalidMonitor)
	}
	if len(slug) > MaxMonitorSlugLen {
		return "", fmt.Errorf("%w: a slug may not exceed %d characters", ErrInvalidMonitor, MaxMonitorSlugLen)
	}
	for _, symbol := range slug {
		switch {
		case symbol >= 'a' && symbol <= 'z', symbol >= '0' && symbol <= '9',
			symbol == '-', symbol == '_':
		default:
			return "", fmt.Errorf("%w: a slug may only contain a-z, 0-9, - and _, got %q",
				ErrInvalidMonitor, raw)
		}
	}
	return slug, nil
}

// CleanTimezone validates an IANA zone name.
//
// Empty means UTC. A fixed offset is refused rather than accepted, because an
// offset is a fact about one instant: a monitor written as "-04:00" in
// Caracas would be an hour wrong for half the year in any country that still
// changes its clocks, and the failure would look like a job that started
// reporting missed for no reason.
func CleanTimezone(raw string) (string, error) {
	zone := strings.TrimSpace(raw)
	if zone == "" {
		return "UTC", nil
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return "", fmt.Errorf("%w: %q is not an IANA timezone (e.g. UTC, America/Caracas): %w",
			ErrInvalidMonitor, raw, err)
	}
	return zone, nil
}

// NewPingKey mints the secret in /ping/{key}.
func NewPingKey() (string, error) {
	raw := make([]byte, PingKeyBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating a ping key: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// CronCheckIn is one reported run.
type CronCheckIn struct {
	// ID is the storage identity.
	ID int64
	// MonitorID is which monitor it belongs to.
	MonitorID int64
	// CheckInID is the id the SDK chose, and the only way a finish is matched
	// to the start it belongs to. Empty for a ping, which has no way to send
	// one.
	CheckInID string
	// Status is what the run reported.
	Status CheckInStatus
	// StartedAt is when the run announced itself, or when it reported
	// finishing if it never announced a start.
	StartedAt time.Time
	// FinishedAt is when it reported finishing, nil while it is in progress.
	FinishedAt *time.Time
	// DurationMS is how long it took, when known. It is what the SDK
	// measured when the SDK said so, and the difference between the two
	// timestamps otherwise.
	DurationMS int64
	// Environment is which deployment reported it.
	Environment string
}

// CleanCheckInID validates the correlation id an SDK sends.
func CleanCheckInID(raw string) (string, error) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", nil
	}
	if len(id) > MaxCheckInIDLen {
		return "", fmt.Errorf("%w: a check-in id may not exceed %d characters",
			ErrInvalidMonitor, MaxCheckInIDLen)
	}
	for _, symbol := range id {
		switch {
		case symbol >= 'a' && symbol <= 'z', symbol >= 'A' && symbol <= 'Z',
			symbol >= '0' && symbol <= '9', symbol == '-', symbol == '_':
		default:
			return "", fmt.Errorf("%w: a check-in id may only contain letters, digits, - and _",
				ErrInvalidMonitor)
		}
	}
	return id, nil
}

// CronOutcome is what one reported run or one sweep decided about a monitor.
//
// It is a value, not a mutation: the rules that decide a monitor's state are
// a pure function of the monitor, what arrived and the time, so they can be
// exhaustively tested without a database and without waiting a minute per
// case. The repository applies it.
type CronOutcome struct {
	// Status is the monitor's state afterwards.
	Status CronStatus
	// NextExpectedAt is when the next run is due afterwards.
	NextExpectedAt time.Time
	// Trigger is what to tell the alert rules, empty when nothing changed
	// worth telling anybody about.
	Trigger TriggerKind
}

// Changed reports whether the outcome moves the monitor's status.
func (o CronOutcome) Changed(previous CronStatus) bool { return o.Status != previous }

// ApplyCheckIn decides what a reported run does to a monitor.
//
// The interesting case is the third one. A run that reports success after the
// monitor was missed, timed out or errored is a recovery, and it is the only
// event in this subsystem that is good news — which is exactly why it has to
// be sent: an operator who was told a backup failed and never told it came
// back will go and look, every time, forever.
func ApplyCheckIn(monitor *CronMonitor, status CheckInStatus, at time.Time) (CronOutcome, error) {
	loc, err := monitor.Location()
	if err != nil {
		return CronOutcome{}, err
	}

	outcome := CronOutcome{}
	switch status {
	case CheckInProgress:
		// A start is not an outcome. It moves nothing and reports nothing:
		// what it does is open the window that MaxRuntime closes, and that
		// window is the check-in row, not the monitor's status.
		outcome.Status = monitor.Status
	case CheckInOK:
		outcome.Status = CronOK
		if monitor.Status.Failing() {
			outcome.Trigger = TriggerCronRecovered
		}
	case CheckInError:
		outcome.Status = CronError
		// Deliberately fired every time the monitor was not already failing,
		// and not only on the transition from ok: a job that has been missed
		// for an hour and now reports an error has changed what is wrong with
		// it, and the silence window (ADR 015) is what stops that from being
		// noisy.
		if monitor.Status != CronError {
			outcome.Trigger = TriggerCronFailed
		}
	default:
		return CronOutcome{}, fmt.Errorf("%w: unknown check-in status %q, expected one of %s",
			ErrInvalidMonitor, status, joinCheckInStatuses())
	}

	next, err := monitor.Schedule.Next(at, loc)
	if err != nil {
		return CronOutcome{}, err
	}
	outcome.NextExpectedAt = next.UTC()
	return outcome, nil
}

// SweepCron decides what the passage of time does to a monitor.
//
// openStartedAt is when the oldest unfinished run announced itself, nil when
// there is none. The two questions are asked in this order deliberately: a run
// that started and never finished is a timeout even if its next occurrence is
// also overdue, and reporting that as "missed" would send somebody looking for
// a job that never started when in fact it started and hung.
func SweepCron(monitor *CronMonitor, openStartedAt *time.Time, now time.Time) (CronOutcome, bool, error) {
	loc, err := monitor.Location()
	if err != nil {
		return CronOutcome{}, false, err
	}

	if openStartedAt != nil && now.Sub(*openStartedAt) > monitor.MaxRuntime {
		if monitor.Status == CronTimeout {
			return CronOutcome{}, false, nil
		}
		next, err := monitor.Schedule.Next(now, loc)
		if err != nil {
			return CronOutcome{}, false, err
		}
		return CronOutcome{
			Status:         CronTimeout,
			NextExpectedAt: next.UTC(),
			Trigger:        TriggerCronTimeout,
		}, true, nil
	}

	if monitor.NextExpectedAt == nil {
		return CronOutcome{}, false, nil
	}
	deadline := monitor.NextExpectedAt.Add(monitor.CheckinMargin)
	if !now.After(deadline) {
		return CronOutcome{}, false, nil
	}

	// Overdue. The deadline moves to the next occurrence after now, always —
	// including when the monitor was already missed, which is what stops a
	// job that has been down all week from being re-reported every thirty
	// seconds. The silence window would suppress the message, but the row
	// would still be written and the log would be useless.
	next, err := monitor.Schedule.Next(now, loc)
	if err != nil {
		return CronOutcome{}, false, err
	}
	outcome := CronOutcome{Status: CronMissed, NextExpectedAt: next.UTC()}
	if monitor.Status != CronMissed {
		outcome.Trigger = TriggerCronMissed
	}
	return outcome, true, nil
}

func joinCheckInStatuses() string {
	names := make([]string, 0, len(AllCheckInStatuses()))
	for _, status := range AllCheckInStatuses() {
		names = append(names, string(status))
	}
	return strings.Join(names, ", ")
}
