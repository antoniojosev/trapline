package domain

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The four things v1 can notify about.
//
// Deliberately four named triggers with numeric parameters rather than a
// query language. A DSL would be a second product to design, document and
// keep compatible, and the four questions below are the ones a person
// actually asks of an error tracker: is this new, did it come back, is it
// getting worse, is the whole thing on fire. Monitors add their triggers to
// this list; nothing else is planned, because the fifth one would be the
// moment to stop and design the language properly.
const (
	// TriggerNewIssue fires the first time a fingerprint is seen.
	TriggerNewIssue TriggerKind = "new_issue"
	// TriggerRegression fires when a resolved issue reopens.
	TriggerRegression TriggerKind = "regression"
	// TriggerIssueSpike fires when one issue's rate jumps over its own past.
	TriggerIssueSpike TriggerKind = "issue_spike"
	// TriggerErrorRate fires when a project's overall rate crosses a ceiling.
	TriggerErrorRate TriggerKind = "error_rate"

	// The monitor triggers (ADR 016). They carry
	// no parameters: the numbers a cron monitor is judged against — its
	// schedule, its margin, its maximum runtime — belong to the monitor and
	// not to the rule, because they are facts about the job and a person
	// setting up a notification should not have to restate them.

	// TriggerCronMissed fires when a scheduled run did not start within its
	// check-in margin.
	TriggerCronMissed TriggerKind = "cron_missed"
	// TriggerCronTimeout fires when a run announced its start and never
	// reported finishing.
	TriggerCronTimeout TriggerKind = "cron_timeout"
	// TriggerCronFailed fires when a run finished and reported failure.
	//
	// Not in the plan's list of five, and added here under ADR 036: a cron
	// monitor whose job runs, fails and tells nobody is the one outcome this
	// feature cannot afford to be quiet about, and the other three triggers
	// all describe silence rather than a reported failure.
	TriggerCronFailed TriggerKind = "cron_failed"
	// TriggerCronRecovered fires when a monitor that was failing reports a
	// successful run.
	TriggerCronRecovered TriggerKind = "cron_recovered"
)

// TriggerKind names what a rule reacts to.
type TriggerKind string

// AllTriggerKinds lists every trigger this build understands.
//
// The uptime pair is declared in uptime.go, beside the subsystem that
// produces it, and joins the list here so a rule can be written against it
// like any other (ADR 016).
func AllTriggerKinds() []TriggerKind {
	return []TriggerKind{
		TriggerNewIssue, TriggerRegression, TriggerIssueSpike, TriggerErrorRate,
		TriggerCronMissed, TriggerCronTimeout, TriggerCronFailed, TriggerCronRecovered,
		TriggerUptimeDown, TriggerUptimeRecovered,
	}
}

// MonitorTriggerKinds lists the triggers a monitor of either family can
// produce. It is what the ingest-side detectors do not need and the two
// monitor use cases do, and having it here keeps "which kinds are about
// monitors" one list rather than a switch in three files — and one list is
// the point: cron and uptime are the same concept watched two ways, so
// anything that treats a monitor trigger differently from an issue trigger
// has to treat all six alike (ADR 016).
func MonitorTriggerKinds() []TriggerKind {
	return []TriggerKind{
		TriggerCronMissed, TriggerCronTimeout, TriggerCronFailed, TriggerCronRecovered,
		TriggerUptimeDown, TriggerUptimeRecovered,
	}
}

// Valid reports whether k is a trigger this build can evaluate.
func (k TriggerKind) Valid() bool { return slices.Contains(AllTriggerKinds(), k) }

// Trigger bounds on the parameters. They are limits on what can be configured,
// not on what can happen: a window of a year is not a rule, it is a mistake
// that will look like a broken feature for a year.
const (
	// MinTriggerWindow is the shortest window a rate rule may watch.
	MinTriggerWindow = time.Minute
	// MaxTriggerWindow is the longest. A day of history is the most the
	// hourly aggregates make cheap to read (ADR 010).
	MaxTriggerWindow = 24 * time.Hour
	// MaxErrorRateWindow is the longest window error_rate can average over,
	// and it is the length of the in-memory counter behind it.
	MaxErrorRateWindow = 5 * time.Minute
	// MaxSilence bounds a rule's quiet period at a week.
	MaxSilence = 7 * 24 * time.Hour
	// DefaultSilence is what a rule gets when it does not say. Fifteen
	// minutes is long enough that a deploy that breaks one endpoint sends one
	// message rather than four hundred, and short enough that a second,
	// unrelated incident within the hour is still heard.
	DefaultSilence = 15 * time.Minute
	// MaxRuleChannels bounds how many channels one rule can fan out to.
	MaxRuleChannels = 20
	// MaxRuleName bounds a rule's display name.
	MaxRuleName = 80
)

// Trigger is a rule's condition: a kind and the numbers it needs.
//
// The zero parameters of a kind that does not use them are not written out,
// so the stored JSON of a new_issue rule is `{"kind":"new_issue"}` and reads
// as what it is.
type Trigger struct {
	// Kind is which of the four conditions this is.
	Kind TriggerKind `json:"kind"`
	// WindowSeconds is how far back a rate trigger looks. Rounded up to whole
	// hours when it is evaluated against the hourly aggregates (ADR 010), and
	// the API says so rather than pretending to a resolution it does not have.
	WindowSeconds int `json:"window_s,omitempty"`
	// MinCount is the floor a spike must clear regardless of the ratio. It is
	// what stops "one event last hour, three this hour" from being a 3× spike.
	MinCount int `json:"min_count,omitempty"`
	// Factor is how many times the baseline the current window must reach.
	Factor float64 `json:"factor,omitempty"`
	// MinEventsPerMinute is the rate at which error_rate fires.
	MinEventsPerMinute int `json:"min_events_per_min,omitempty"`
}

// ParseTrigger reads a stored or submitted trigger.
//
// Unknown fields are rejected: a rule that quietly ignores `factor: 10`
// because the field is called something else is a rule that will not fire and
// nobody will know why until the incident it was written for.
func ParseTrigger(raw []byte) (Trigger, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()

	var trigger Trigger
	if err := decoder.Decode(&trigger); err != nil {
		return Trigger{}, fmt.Errorf("%w: %w", ErrInvalidAlert, err)
	}
	if decoder.More() {
		return Trigger{}, fmt.Errorf("%w: a trigger is one JSON object", ErrInvalidAlert)
	}
	if err := trigger.Validate(); err != nil {
		return Trigger{}, err
	}
	return trigger, nil
}

// Encode renders a trigger for storage.
func (t Trigger) Encode() ([]byte, error) {
	encoded, err := json.Marshal(t)
	if err != nil {
		return nil, fmt.Errorf("encoding trigger: %w", err)
	}
	return encoded, nil
}

// Validate applies the rules of the trigger's own kind, filling the defaults
// that have one.
func (t *Trigger) Validate() error {
	if !t.Kind.Valid() {
		known := make([]string, 0, len(AllTriggerKinds()))
		for _, kind := range AllTriggerKinds() {
			known = append(known, string(kind))
		}
		return fmt.Errorf("%w: unknown trigger %q, expected one of %s",
			ErrInvalidAlert, t.Kind, strings.Join(known, ", "))
	}

	switch t.Kind {
	case TriggerNewIssue, TriggerRegression,
		TriggerCronMissed, TriggerCronTimeout, TriggerCronFailed, TriggerCronRecovered,
		TriggerUptimeDown, TriggerUptimeRecovered:
		// These carry no parameters, and a parameter on one of them means the
		// author expected something the rule will not do. A monitor's own
		// threshold — two consecutive failures — is a property of the monitor
		// and not of the rule watching it, so there is nothing to configure
		// here either (ADR 016).
		if t.WindowSeconds != 0 || t.MinCount != 0 || t.Factor != 0 || t.MinEventsPerMinute != 0 {
			return fmt.Errorf("%w: %s takes no parameters", ErrInvalidAlert, t.Kind)
		}
		return nil

	case TriggerIssueSpike:
		if t.MinEventsPerMinute != 0 {
			return fmt.Errorf("%w: issue_spike takes window_s, min_count and factor", ErrInvalidAlert)
		}
		if err := validateWindow(t.WindowSeconds); err != nil {
			return err
		}
		if t.MinCount <= 0 {
			return fmt.Errorf("%w: issue_spike needs a positive min_count", ErrInvalidAlert)
		}
		if t.Factor < 1 || math.IsNaN(t.Factor) || math.IsInf(t.Factor, 0) {
			return fmt.Errorf("%w: issue_spike needs a factor of at least 1, got %v",
				ErrInvalidAlert, t.Factor)
		}
		return nil

	case TriggerErrorRate:
		if t.MinCount != 0 || t.Factor != 0 {
			return fmt.Errorf("%w: error_rate takes window_s and min_events_per_min", ErrInvalidAlert)
		}
		if err := validateWindow(t.WindowSeconds); err != nil {
			return err
		}
		// This one is measured before the rate limiter, from a counter that
		// has to live in memory (ADR 005/015), and that counter is five
		// minutes of per-minute buckets. A rule asking for an average over
		// six hours would be asking a five-minute counter a question it
		// cannot answer, and answering it approximately would be worse than
		// refusing: the number would look right.
		if t.Window() > MaxErrorRateWindow {
			return fmt.Errorf("%w: error_rate is measured from a %s sliding window, so window_s cannot exceed %d",
				ErrInvalidAlert, MaxErrorRateWindow, int(MaxErrorRateWindow.Seconds()))
		}
		if t.MinEventsPerMinute <= 0 {
			return fmt.Errorf("%w: error_rate needs a positive min_events_per_min", ErrInvalidAlert)
		}
		return nil

	default:
		return fmt.Errorf("%w: unknown trigger %q", ErrInvalidAlert, t.Kind)
	}
}

func validateWindow(seconds int) error {
	window := time.Duration(seconds) * time.Second
	if window < MinTriggerWindow || window > MaxTriggerWindow {
		return fmt.Errorf("%w: window_s must be between %d and %d seconds, got %d",
			ErrInvalidAlert, int(MinTriggerWindow.Seconds()), int(MaxTriggerWindow.Seconds()), seconds)
	}
	return nil
}

// Window is the trigger's window as a duration.
func (t Trigger) Window() time.Duration { return time.Duration(t.WindowSeconds) * time.Second }

// AlertRule is a condition, the channels it notifies and how long it then
// stays quiet.
type AlertRule struct {
	// ID is the storage identity, zero before it is saved.
	ID int64
	// ProjectID scopes the rule. Nil means every project, which is what an
	// installation with one application wants and what the first rule anyone
	// writes should be able to say without naming anything.
	ProjectID *int64
	// Name is what an operator recognises it by, and what a notification says
	// it came from.
	Name string
	// Trigger is the condition.
	Trigger Trigger
	// ChannelIDs are where it delivers.
	ChannelIDs []int64
	// SilenceSeconds is how long the rule stays quiet about one subject after
	// firing about it.
	SilenceSeconds int
	// Enabled is whether it is evaluated at all.
	Enabled bool
}

// NewAlertRule validates a rule before it can be stored.
func NewAlertRule(
	projectID *int64, name string, trigger Trigger, channelIDs []int64, silence time.Duration, enabled bool,
) (AlertRule, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return AlertRule{}, fmt.Errorf("%w: a rule needs a name", ErrInvalidAlert)
	}
	if len(name) > MaxRuleName {
		return AlertRule{}, fmt.Errorf("%w: name exceeds %d characters", ErrInvalidAlert, MaxRuleName)
	}
	if err := trigger.Validate(); err != nil {
		return AlertRule{}, err
	}
	if len(channelIDs) == 0 {
		return AlertRule{}, fmt.Errorf("%w: a rule with no channel notifies nobody", ErrInvalidAlert)
	}
	if len(channelIDs) > MaxRuleChannels {
		return AlertRule{}, fmt.Errorf("%w: a rule may name at most %d channels", ErrInvalidAlert, MaxRuleChannels)
	}
	deduplicated := slices.Clone(channelIDs)
	slices.Sort(deduplicated)
	deduplicated = slices.Compact(deduplicated)
	for _, id := range deduplicated {
		if id <= 0 {
			return AlertRule{}, fmt.Errorf("%w: %d is not a channel id", ErrInvalidAlert, id)
		}
	}
	if silence == 0 {
		silence = DefaultSilence
	}
	if silence < 0 || silence > MaxSilence {
		return AlertRule{}, fmt.Errorf("%w: silence must be between 0 and %s, got %s",
			ErrInvalidAlert, MaxSilence, silence)
	}
	if projectID != nil && *projectID <= 0 {
		return AlertRule{}, fmt.Errorf("%w: %d is not a project id", ErrInvalidAlert, *projectID)
	}

	scoped := projectID
	if scoped != nil {
		value := *scoped
		scoped = &value
	}
	return AlertRule{
		ProjectID:      scoped,
		Name:           name,
		Trigger:        trigger,
		ChannelIDs:     deduplicated,
		SilenceSeconds: int(silence.Seconds()),
		Enabled:        enabled,
	}, nil
}

// Silence is the rule's quiet period as a duration.
func (r AlertRule) Silence() time.Duration { return time.Duration(r.SilenceSeconds) * time.Second }

// AppliesTo reports whether the rule covers a project. A rule with no project
// covers all of them.
func (r AlertRule) AppliesTo(projectID int64) bool {
	return r.ProjectID == nil || *r.ProjectID == projectID
}

// AlertEvent is something that happened, offered to the rules.
//
// It is the whole vocabulary the rules see: a kind, a subject and the numbers
// that go with it. Anything a notification says has to be in here, because by
// the time a rule matches, the event that produced it is gone.
type AlertEvent struct {
	// Kind is which trigger this event can satisfy.
	Kind TriggerKind
	// ProjectID is whose event it is.
	ProjectID int64
	// IssueID is the issue this is about, zero for a project-wide event.
	IssueID int64
	// MonitorKind and MonitorID are the monitor this is about, empty and zero
	// when it is not about one. A monitor event never also carries an issue:
	// the two are different subjects, and sharing a field would make the
	// silence window confuse them. Together they are what makes that window
	// per monitor rather than per project — two services failing in the same
	// minute are two incidents and must produce two messages — and the kind
	// is half of the pair because the two families number their monitors from
	// separate tables (ADR 015, ADR 016).
	MonitorKind MonitorKind
	// MonitorID is the monitor's id within its family.
	MonitorID int64
	// MonitorSlug names that monitor in the message — its slug for a cron
	// monitor, its name for an uptime one — because "monitor 7 is missed" is
	// not something anybody can act on.
	MonitorSlug string
	// Title and Culprit describe the issue.
	Title string
	// Culprit is where it happened.
	Culprit string
	// Level is the issue's severity.
	Level Level
	// Release and Environment are where it was seen.
	Release string
	// Environment is which deployment it came from.
	Environment string
	// Count is the number the trigger fired on — occurrences in the window,
	// or events per minute for error_rate.
	Count int64
	// Baseline is what Count was compared against, for the triggers that
	// compare. Zero for the ones that do not.
	Baseline int64
	// At is when it happened.
	At time.Time
}

// SubjectKey identifies what a rule is being quiet about.
//
// The silence window is per subject, not per rule: a deploy that breaks two
// endpoints should produce two notifications, and then stay quiet about both.
// A rule-wide silence would let the first issue mask the second, which is the
// failure mode that makes people switch alerting off.
// A pointer receiver on a value type otherwise passed by value, for the same
// reason AlertPayload's renderers have one: an AlertEvent is now the better
// part of two hundred bytes and this method only reads it (ADR 015).
func (e *AlertEvent) SubjectKey() string {
	if e.IssueID > 0 {
		return "issue:" + strconv.FormatInt(e.IssueID, 10)
	}
	if e.MonitorID > 0 {
		return MonitorSubjectKey(e.MonitorKind, e.MonitorID)
	}
	return "project:" + strconv.FormatInt(e.ProjectID, 10)
}

// Matches reports whether an event satisfies a rule.
//
// The thresholds are checked here rather than at the detector, so the one
// place that decides whether something is worth waking somebody for is a pure
// function with a table of tests behind it.
//
// The event arrives by pointer, against this package's habit of passing domain
// values by value, for the reason AlertPayload's renderers already document:
// it is close to two hundred bytes, this is called once per rule per event on
// the ingest path, and nothing here mutates it.
func (r AlertRule) Matches(event *AlertEvent) bool {
	if !r.Enabled || r.Trigger.Kind != event.Kind || !r.AppliesTo(event.ProjectID) {
		return false
	}
	switch r.Trigger.Kind {
	case TriggerNewIssue, TriggerRegression,
		TriggerCronMissed, TriggerCronTimeout, TriggerCronFailed, TriggerCronRecovered,
		TriggerUptimeDown, TriggerUptimeRecovered:
		return true

	case TriggerIssueSpike:
		if event.Count < int64(r.Trigger.MinCount) {
			return false
		}
		if event.Baseline <= 0 {
			// Nothing to compare against: the issue has no history in the
			// window. Clearing min_count is the whole test, which is what
			// makes a burst on a brand-new issue reportable instead of
			// invisible because its baseline is zero.
			return true
		}
		return float64(event.Count) >= r.Trigger.Factor*float64(event.Baseline)

	case TriggerErrorRate:
		return event.Count >= int64(r.Trigger.MinEventsPerMinute)

	default:
		return false
	}
}

// Silenced reports whether a rule has already spoken about this subject
// recently enough to stay quiet.
//
// A zero lastFired means it never has. The comparison is inclusive of the
// boundary in the quiet direction, so a silence of zero — which is legal and
// means "every time" — never silences anything.
func Silenced(lastFired time.Time, silence time.Duration, now time.Time) bool {
	if lastFired.IsZero() || silence <= 0 {
		return false
	}
	return now.Before(lastFired.Add(silence))
}
