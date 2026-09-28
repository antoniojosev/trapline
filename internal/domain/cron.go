package domain

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The bounds of each crontab field, in the order they are written.
//
// They are the classic Vixie ranges and not an invention: a person
// instrumenting a cron job copies the line out of their crontab, and a parser
// that disagreed with the one that has been running that line for years would
// be wrong in the only way that matters here.
const (
	minMinute = 0
	maxMinute = 59
	minHour   = 0
	maxHour   = 23
	minDom    = 1
	maxDom    = 31
	minMonth  = 1
	maxMonth  = 12
	minDow    = 0
	// Seven is Sunday as well as zero. Both spellings are in the wild and
	// rejecting either would fail on lines that work everywhere else.
	maxDow = 7
)

// MaxCronSpecLen bounds a schedule string.
//
// A crontab line is five fields of at most a few dozen characters. The cap is
// here because this string arrives from an SDK's monitor_config over the
// public ingest endpoint (ADR 002), so it is attacker-chosen, and a parser
// that will happily walk a megabyte of commas is a parser that has been given
// a budget by whoever is sending to it.
const MaxCronSpecLen = 256

// CronSearchYears is how far Next looks before declaring that a schedule
// never comes round again.
//
// A crontab can name a day that does not exist — `0 0 30 2 *` is February the
// thirtieth — and the honest answer to "when is the next one" is that there
// is not one. Four years is past any leap cycle, so a schedule that has not
// matched by then never will.
const CronSearchYears = 4

// CronSchedule is a parsed five-field crontab expression.
//
// Each field is a bitmask rather than a list, so matching a candidate minute
// is a shift and an AND instead of a scan. That matters because Next walks
// forward minute by minute in the worst case, and this is called once per
// monitor per sweep.
//
// It is a value type with no pointers: a schedule is copied freely, compared
// with ==, and cannot be mutated by anything holding it.
type CronSchedule struct {
	// raw is the expression as written, kept so a monitor can be shown and
	// stored in the operator's own words rather than in a normalised form
	// they never typed.
	raw string

	minute uint64
	hour   uint64
	dom    uint64
	month  uint64
	dow    uint64

	// domRestricted and dowRestricted record whether the day-of-month and
	// day-of-week fields were written as anything other than `*`. Cron's
	// oldest and strangest rule depends on it: when both are restricted the
	// two are OR'd, not AND'd, so `0 0 1 * 1` means "the first of the month
	// AND every Monday", not "Mondays that fall on the first". Everybody who
	// has ever written a crontab depends on this behaviour whether they know
	// it or not.
	domRestricted bool
	dowRestricted bool
}

// The aliases this build understands, expanded to the expression they stand
// for.
//
// Four, and deliberately not the full set. `@reboot` has no meaning for a
// monitor that is not running the job, and `@yearly` is a schedule so long
// that a missed-detection window measured against it says nothing useful for
// a year. A fifth alias is one line here plus a test; the reason to stop at
// four is that nobody has asked for a fifth.
var cronAliases = map[string]string{
	"@hourly":  "0 * * * *",
	"@daily":   "0 0 * * *",
	"@weekly":  "0 0 * * 0",
	"@monthly": "0 0 1 * *",
}

// CronAliases lists the supported aliases, for an error message that names
// them rather than leaving somebody guessing.
func CronAliases() []string { return []string{"@hourly", "@daily", "@weekly", "@monthly"} }

// ParseCron reads a crontab expression.
//
// Five fields, `*`, lists, ranges, steps and the four aliases. Deliberately
// no names for months or weekdays, no `?`, no `L`, no `#`: each is a dialect
// somebody's scheduler speaks and this product's answer to a line it cannot
// parse must be to say so, not to guess a schedule and then report a job as
// missed at a time nobody expected.
func ParseCron(spec string) (CronSchedule, error) {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		return CronSchedule{}, fmt.Errorf("%w: a schedule is required", ErrInvalidMonitor)
	}
	if len(trimmed) > MaxCronSpecLen {
		return CronSchedule{}, fmt.Errorf("%w: a schedule may not exceed %d characters",
			ErrInvalidMonitor, MaxCronSpecLen)
	}

	expression := trimmed
	if strings.HasPrefix(expression, "@") {
		expanded, known := cronAliases[strings.ToLower(expression)]
		if !known {
			return CronSchedule{}, fmt.Errorf("%w: unknown alias %q, expected one of %s",
				ErrInvalidMonitor, expression, strings.Join(CronAliases(), ", "))
		}
		expression = expanded
	}

	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return CronSchedule{}, fmt.Errorf(
			"%w: a schedule has five fields (minute hour day-of-month month day-of-week), got %d in %q",
			ErrInvalidMonitor, len(fields), trimmed)
	}

	schedule := CronSchedule{raw: trimmed}
	var err error
	if schedule.minute, err = parseCronField(fields[0], minMinute, maxMinute, "minute"); err != nil {
		return CronSchedule{}, err
	}
	if schedule.hour, err = parseCronField(fields[1], minHour, maxHour, "hour"); err != nil {
		return CronSchedule{}, err
	}
	if schedule.dom, err = parseCronField(fields[2], minDom, maxDom, "day-of-month"); err != nil {
		return CronSchedule{}, err
	}
	if schedule.month, err = parseCronField(fields[3], minMonth, maxMonth, "month"); err != nil {
		return CronSchedule{}, err
	}
	if schedule.dow, err = parseCronField(fields[4], minDow, maxDow, "day-of-week"); err != nil {
		return CronSchedule{}, err
	}
	// Seven and zero are both Sunday, and are folded into one bit so the
	// match is a single test rather than two.
	if schedule.dow&(1<<uint(maxDow)) != 0 {
		schedule.dow |= 1 << uint(minDow)
		schedule.dow &^= 1 << uint(maxDow)
	}

	schedule.domRestricted = fields[2] != "*"
	schedule.dowRestricted = fields[4] != "*"
	return schedule, nil
}

// String renders the schedule as it was written.
func (s CronSchedule) String() string { return s.raw }

// IsZero reports whether this is the zero schedule, which matches nothing.
func (s CronSchedule) IsZero() bool { return s.raw == "" }

// parseCronField turns one field into a bitmask.
func parseCronField(field string, low, high int, name string) (uint64, error) {
	if field == "" {
		return 0, fmt.Errorf("%w: the %s field is empty", ErrInvalidMonitor, name)
	}

	var bits uint64
	for _, item := range strings.Split(field, ",") {
		itemBits, err := parseCronItem(item, low, high, name)
		if err != nil {
			return 0, err
		}
		bits |= itemBits
	}
	if bits == 0 {
		return 0, fmt.Errorf("%w: the %s field %q matches nothing", ErrInvalidMonitor, name, field)
	}
	return bits, nil
}

// parseCronItem reads one comma-separated element: `*`, `n`, `a-b`, and any
// of those with a `/step` suffix.
func parseCronItem(item string, low, high int, name string) (uint64, error) {
	value, rawStep, hasStep := strings.Cut(item, "/")

	step := 1
	if hasStep {
		parsed, err := strconv.Atoi(rawStep)
		if err != nil || parsed <= 0 {
			return 0, fmt.Errorf("%w: the %s field has step %q, which must be a positive integer",
				ErrInvalidMonitor, name, rawStep)
		}
		step = parsed
	}

	first, last, err := cronBounds(value, low, high, name, hasStep)
	if err != nil {
		return 0, err
	}

	var bits uint64
	for candidate := first; candidate <= last; candidate += step {
		bits |= cronBit(candidate)
	}
	return bits, nil
}

// cronBounds resolves the range an item covers, before its step is applied.
//
// A bare number with a step — `5/15` — means "from five to the end of the
// field", which is what every crontab implementation does with it and what
// somebody who wrote it meant. A bare number without one is just itself.
func cronBounds(value string, low, high int, name string, hasStep bool) (first, last int, err error) {
	if value == "*" {
		return low, high, nil
	}

	rawFirst, rawLast, isRange := strings.Cut(value, "-")
	first, err = parseCronNumber(rawFirst, low, high, name)
	if err != nil {
		return 0, 0, err
	}
	switch {
	case isRange:
		last, err = parseCronNumber(rawLast, low, high, name)
		if err != nil {
			return 0, 0, err
		}
		if last < first {
			// Deliberately an error rather than a wrap-around. `22-3` reads
			// like "ten at night until three in the morning" and some
			// dialects treat it that way; Vixie cron does not, and silently
			// picking one of the two readings for a monitor that decides when
			// to wake somebody is the wrong kind of helpful.
			return 0, 0, fmt.Errorf("%w: the %s range %q ends before it starts; write two items, %d-%d,%d-%d",
				ErrInvalidMonitor, name, value, first, high, low, last)
		}
	case hasStep:
		last = high
	default:
		last = first
	}
	return first, last, nil
}

func parseCronNumber(raw string, low, high int, name string) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: the %s field has %q, which is not a number "+
			"(this build takes numbers only, not names)", ErrInvalidMonitor, name, raw)
	}
	if value < low || value > high {
		return 0, fmt.Errorf("%w: the %s field has %d, which is outside %d-%d",
			ErrInvalidMonitor, name, value, low, high)
	}
	return value, nil
}

// cronBit is the bit that stands for n in one of the schedule's field bitmaps.
//
// One function rather than six copies of `1 << uint(n)`, for two reasons. The
// conversion from a signed count is the kind a static analyser flags and
// cannot clear — it has no way of knowing that a time.Time's minute is
// between 0 and 59 — so the explanation belongs in one place with the bound
// beside it rather than repeated at six call sites. And the bound is now
// actually checked: every caller passes a field of a time.Time or a value
// cronBounds already validated, but a bitmap is 64 bits wide and a schedule
// parsed from a string a user typed has no business being able to shift past
// it. Out of range is no match, which is what the shift did anyway.
func cronBit(n int) uint64 {
	if n < 0 || n > 63 {
		return 0
	}
	return 1 << uint(n) // #nosec G115 -- bounds-checked on the line above.
}

// matchesDay applies cron's day rule: an OR when both day fields are
// restricted, an AND otherwise (see domRestricted).
func (s CronSchedule) matchesDay(t time.Time) bool {
	domHit := s.dom&cronBit(t.Day()) != 0
	dowHit := s.dow&cronBit(int(t.Weekday())) != 0

	if s.domRestricted && s.dowRestricted {
		return domHit || dowHit
	}
	return domHit && dowHit
}

// Next is the first instant strictly after `after` that the schedule matches,
// read in loc.
//
// The location is a parameter and not a field because it belongs to the
// monitor, not to the expression: `0 3 * * *` is a different moment in Caracas
// than in Madrid, and a schedule that carried its own zone would have to be
// re-parsed to move a monitor between them. A nil loc means UTC, which is the
// only defensible default for a server.
//
// The search walks forward by whole fields — skip to the next month, the next
// day, the next hour — so a yearly schedule costs a handful of steps rather
// than half a million.
func (s CronSchedule) Next(after time.Time, loc *time.Location) (time.Time, error) {
	if s.IsZero() {
		return time.Time{}, fmt.Errorf("%w: the zero schedule matches nothing", ErrInvalidMonitor)
	}
	if loc == nil {
		loc = time.UTC
	}

	local := after.In(loc)
	// The next whole minute. Truncating and adding rather than adding and
	// truncating, so a time that already sits exactly on a minute still moves
	// forward: Next must return something strictly later than what it was
	// given, and a schedule that matched `after` itself would otherwise
	// return it and the caller would never advance.
	candidate := time.Date(local.Year(), local.Month(), local.Day(),
		local.Hour(), local.Minute(), 0, 0, loc).Add(time.Minute)
	deadline := local.AddDate(CronSearchYears, 0, 0)

	for candidate.Before(deadline) {
		switch {
		case s.month&cronBit(int(candidate.Month())) == 0:
			candidate = advance(candidate, loc, time.Date(
				candidate.Year(), candidate.Month()+1, 1, 0, 0, 0, 0, loc))
		case !s.matchesDay(candidate):
			candidate = advance(candidate, loc, time.Date(
				candidate.Year(), candidate.Month(), candidate.Day()+1, 0, 0, 0, 0, loc))
		case s.hour&cronBit(candidate.Hour()) == 0:
			candidate = advance(candidate, loc, time.Date(
				candidate.Year(), candidate.Month(), candidate.Day(), candidate.Hour()+1, 0, 0, 0, loc))
		case s.minute&cronBit(candidate.Minute()) == 0:
			candidate = advance(candidate, loc, candidate.Add(time.Minute))
		default:
			return candidate, nil
		}
	}
	return time.Time{}, fmt.Errorf("%w: %q has no occurrence within %d years of %s",
		ErrInvalidMonitor, s.raw, CronSearchYears, after.UTC().Format(time.RFC3339))
}

// advance moves the search forward, and guarantees it actually moves.
//
// The guard is not paranoia about arithmetic. Every step above is expressed in
// wall-clock components, and wall-clock components do not always move forward
// when you add to them: an hour that does not exist because the clocks went
// forward, or one that happens twice because they went back, is resolved by
// time.Date to an instant the standard library explicitly declines to
// guarantee. One minute of absolute time always moves, so that is the floor.
// Without it a monitor in a zone with a transition could spin here forever,
// which is a worse failure than being a minute out on the two days a year the
// clocks change.
func advance(from time.Time, loc *time.Location, to time.Time) time.Time {
	if to.After(from) {
		return to.In(loc)
	}
	return from.Add(time.Minute).In(loc)
}
