package domain

import (
	"fmt"
	"strings"
	"time"
)

// DigestHours is how much of the past one digest covers: seven days of hourly
// buckets.
//
// A week and not "since the last one" on purpose. A server that was down for
// a fortnight comes back and sends one digest about the last week, not a
// double-length report nobody asked for; and a digest sent twice by accident
// says the same thing both times, which is the property that makes resending
// one safe.
const DigestHours = 7 * 24

// DigestSchedule is when an installation's weekly digest goes out: a weekday
// and an hour, in UTC.
//
// UTC and not a location, in this version. A timezone here is a real want —
// "Monday at nine" means nine where the reader is — but it costs an embedded
// zone database in a binary with a 30 MB budget, and the same decision is
// about to be forced by cron monitors (ADR 016), which need zones for a much
// sharper reason. Making it here first, alone, would mean deciding it twice.
// Until then the field is absent rather than present and ignored: a schedule
// that claims a timezone it does not honour is worse than one that says UTC.
type DigestSchedule struct {
	Weekday time.Weekday
	// Hour is 0–23, UTC. The digest covers the seven days ending at the top
	// of this hour, so it never reports on a partial hour — the aggregate
	// bucket for the hour it is sent in is still filling.
	Hour int
}

// DefaultDigestSchedule is Monday at 09:00 UTC.
//
// Monday morning because the thing a digest is for is deciding what the week
// contains, and a report that arrives on Friday evening is read on Monday
// anyway, by which time it is describing something else.
var DefaultDigestSchedule = DigestSchedule{Weekday: time.Monday, Hour: 9}

// weekdayNames maps the spellings a person types to the day they mean.
//
// Long names only, and lowercase. A CLI that also accepted "mon" would have
// to decide what "m" means, and a schedule is set once per installation:
// there is nothing to save by abbreviating it.
var weekdayNames = map[string]time.Weekday{
	"sunday":    time.Sunday,
	"monday":    time.Monday,
	"tuesday":   time.Tuesday,
	"wednesday": time.Wednesday,
	"thursday":  time.Thursday,
	"friday":    time.Friday,
	"saturday":  time.Saturday,
}

// ParseWeekday reads a day name, case-insensitively.
func ParseWeekday(raw string) (time.Weekday, error) {
	day, known := weekdayNames[strings.ToLower(strings.TrimSpace(raw))]
	if !known {
		return 0, fmt.Errorf(
			"%w: %q is not a day; use monday, tuesday, wednesday, thursday, friday, saturday or sunday",
			ErrInvalidDigest, raw)
	}
	return day, nil
}

// NewDigestSchedule validates a weekday and an hour.
func NewDigestSchedule(day time.Weekday, hour int) (DigestSchedule, error) {
	if day < time.Sunday || day > time.Saturday {
		return DigestSchedule{}, fmt.Errorf("%w: %d is not a weekday", ErrInvalidDigest, int(day))
	}
	if hour < 0 || hour > 23 {
		return DigestSchedule{}, fmt.Errorf("%w: %d is not an hour of the day", ErrInvalidDigest, hour)
	}
	return DigestSchedule{Weekday: day, Hour: hour}, nil
}

// Validate reports whether a schedule read back from storage still makes
// sense.
//
// Storage can hold anything a previous version wrote, or anything somebody
// edited into the file by hand, and a digest job that silently treats hour 47
// as "never" would be a subsystem that is switched on and does nothing.
func (s DigestSchedule) Validate() error {
	_, err := NewDigestSchedule(s.Weekday, s.Hour)
	return err
}

// String is the schedule as an operator would say it.
func (s DigestSchedule) String() string {
	return fmt.Sprintf("%s at %02d:00 UTC", s.Weekday, s.Hour)
}

// Previous is the most recent scheduled moment at or before at.
//
// "At or before", so calling it exactly on the hour returns that hour rather
// than the week before it. That is what makes the send decision a comparison
// against a stored marker instead of a window somebody has to widen when a
// job runs a minute late.
func (s DigestSchedule) Previous(at time.Time) time.Time {
	hour := at.UTC().Truncate(time.Hour)
	// Days back to the scheduled weekday, 0..6.
	back := (int(hour.Weekday()) - int(s.Weekday) + 7) % 7
	candidate := time.Date(hour.Year(), hour.Month(), hour.Day(), s.Hour, 0, 0, 0, time.UTC).
		AddDate(0, 0, -back)
	if candidate.After(hour) {
		// Same weekday, but the hour has not arrived yet: the last send was a
		// week ago.
		candidate = candidate.AddDate(0, 0, -7)
	}
	return candidate
}

// Next is the first scheduled moment strictly after at.
func (s DigestSchedule) Next(at time.Time) time.Time {
	next := s.Previous(at).AddDate(0, 0, 7)
	// Previous truncates to the hour, so an instant inside the scheduled hour
	// resolves to that hour and the next send is a week out. Anything else is
	// already correct.
	return next
}

// Covers is the range of hourly buckets a digest sent at sendAt reports on:
// the seven days ending with the hour before it.
//
// The hour of the send itself is excluded because its bucket is still being
// written to. Including it would make two digests over the same data disagree
// by however many events landed in between, which is the one thing a
// deterministic report must not do.
func (s DigestSchedule) Covers(sendAt time.Time) Range {
	to := sendAt.UTC().Truncate(time.Hour).Add(-time.Hour)
	return Range{From: to.Add(-(DigestHours - 1) * time.Hour), To: to}
}

// Preceding is the week before the one r covers, which is what a trend is
// measured against.
func Preceding(r Range) Range {
	span := time.Duration(r.Hours()) * time.Hour
	return Range{From: r.From.Add(-span), To: r.To.Add(-span)}
}
