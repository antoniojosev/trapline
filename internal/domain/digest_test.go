package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

func TestParseWeekdayReadsWhatAPersonTypes(t *testing.T) {
	for _, testCase := range []struct {
		raw  string
		want time.Weekday
	}{
		{"monday", time.Monday},
		{"Monday", time.Monday},
		{"  SUNDAY  ", time.Sunday},
		{"saturday", time.Saturday},
	} {
		got, err := domain.ParseWeekday(testCase.raw)
		if err != nil {
			t.Errorf("ParseWeekday(%q): %v", testCase.raw, err)
			continue
		}
		if got != testCase.want {
			t.Errorf("ParseWeekday(%q) = %v, want %v", testCase.raw, got, testCase.want)
		}
	}
}

func TestParseWeekdayRefusesAnythingElse(t *testing.T) {
	// "mon" is in here on purpose: abbreviations are not accepted, and a test
	// that only tried nonsense would let one in by accident later.
	for _, raw := range []string{"", "mon", "lunes", "8", "mondays"} {
		if _, err := domain.ParseWeekday(raw); err == nil {
			t.Errorf("ParseWeekday(%q) was accepted", raw)
		} else if !errors.Is(err, domain.ErrInvalidDigest) {
			t.Errorf("ParseWeekday(%q) = %v, which is not ErrInvalidDigest", raw, err)
		}
	}
}

func TestNewDigestScheduleRefusesAnHourThatDoesNotExist(t *testing.T) {
	for _, hour := range []int{-1, 24, 47} {
		if _, err := domain.NewDigestSchedule(time.Monday, hour); !errors.Is(err, domain.ErrInvalidDigest) {
			t.Errorf("hour %d was accepted: %v", hour, err)
		}
	}
	if _, err := domain.NewDigestSchedule(time.Weekday(9), 9); !errors.Is(err, domain.ErrInvalidDigest) {
		t.Errorf("weekday 9 was accepted: %v", err)
	}
	if _, err := domain.NewDigestSchedule(time.Monday, 0); err != nil {
		t.Errorf("midnight is an hour: %v", err)
	}
	if _, err := domain.NewDigestSchedule(time.Sunday, 23); err != nil {
		t.Errorf("Sunday at 23 is a time: %v", err)
	}
}

func TestValidateRejectsWhatStorageMightHold(t *testing.T) {
	// A schedule read back from a file somebody edited by hand. The digest
	// must say so rather than silently treating hour 47 as "never", which is
	// a subsystem that is switched on and does nothing.
	corrupt := domain.DigestSchedule{Weekday: time.Monday, Hour: 47}
	if err := corrupt.Validate(); !errors.Is(err, domain.ErrInvalidDigest) {
		t.Errorf("Validate() = %v, want ErrInvalidDigest", err)
	}
	if err := domain.DefaultDigestSchedule.Validate(); err != nil {
		t.Errorf("the default schedule does not validate: %v", err)
	}
}

func TestScheduleStringIsWhatAnOperatorWouldSay(t *testing.T) {
	if got := domain.DefaultDigestSchedule.String(); got != "Monday at 09:00 UTC" {
		t.Errorf("String() = %q", got)
	}
}

// TestPreviousIsAtOrBefore pins the boundary the whole send decision rests on.
//
// Previous is compared against a stored marker, so an off-by-one hour here is
// a digest that goes out twice, or one that never goes out at all.
func TestPreviousIsAtOrBefore(t *testing.T) {
	schedule := domain.DigestSchedule{Weekday: time.Monday, Hour: 9}
	// 2026-08-31 is a Monday.
	monday := time.Date(2026, time.August, 31, 9, 0, 0, 0, time.UTC)

	for _, testCase := range []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"exactly on the hour", monday, monday},
		{"inside the scheduled hour", monday.Add(59 * time.Minute), monday},
		{"an hour before it", monday.Add(-time.Hour), monday.AddDate(0, 0, -7)},
		{"a minute before it", monday.Add(-time.Minute), monday.AddDate(0, 0, -7)},
		{"the next day", monday.AddDate(0, 0, 1), monday},
		{"six days later", monday.AddDate(0, 0, 6), monday},
		{"a week later", monday.AddDate(0, 0, 7), monday.AddDate(0, 0, 7)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := schedule.Previous(testCase.now); !got.Equal(testCase.want) {
				t.Errorf("Previous(%s) = %s, want %s",
					testCase.now.Format(time.RFC3339), got.Format(time.RFC3339),
					testCase.want.Format(time.RFC3339))
			}
		})
	}
}

// TestPreviousAndNextAreAWeekApart is the property that makes the marker work:
// whatever the moment, the two scheduled instants around it are exactly a week
// apart and the moment sits between them.
func TestPreviousAndNextAreAWeekApart(t *testing.T) {
	schedule := domain.DigestSchedule{Weekday: time.Thursday, Hour: 17}
	at := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

	// Every hour of a whole year, which also walks through every weekday and
	// every hour of the day without a table anybody has to maintain.
	for hour := 0; hour < 24*365; hour++ {
		now := at.Add(time.Duration(hour) * time.Hour)
		previous := schedule.Previous(now)
		next := schedule.Next(now)

		if next.Sub(previous) != 7*24*time.Hour {
			t.Fatalf("at %s the scheduled moments are %s apart, not a week",
				now.Format(time.RFC3339), next.Sub(previous))
		}
		if previous.After(now) {
			t.Fatalf("at %s the previous moment %s is in the future",
				now.Format(time.RFC3339), previous.Format(time.RFC3339))
		}
		if !next.After(now) {
			t.Fatalf("at %s the next moment %s is not in the future",
				now.Format(time.RFC3339), next.Format(time.RFC3339))
		}
		if previous.Weekday() != schedule.Weekday || previous.Hour() != schedule.Hour {
			t.Fatalf("at %s the previous moment %s is not the scheduled time",
				now.Format(time.RFC3339), previous.Format(time.RFC3339))
		}
	}
}

// TestCoversEndsBeforeTheSendHour is the determinism rule, written down: the
// bucket the digest is sent in is still filling, so it is not in the report.
func TestCoversEndsBeforeTheSendHour(t *testing.T) {
	schedule := domain.DefaultDigestSchedule
	sendAt := time.Date(2026, time.August, 31, 9, 30, 0, 0, time.UTC)

	covers := schedule.Covers(sendAt)
	if got := covers.LastBucket(); got != "2026-08-31T08" {
		t.Errorf("the last bucket is %q, want the hour before the send", got)
	}
	if got := covers.FirstBucket(); got != "2026-08-24T09" {
		t.Errorf("the first bucket is %q", got)
	}
	if got := covers.Hours(); got != domain.DigestHours {
		t.Errorf("the range covers %d hours, want %d", got, domain.DigestHours)
	}
}

func TestPrecedingIsTheWeekBefore(t *testing.T) {
	covers := domain.DefaultDigestSchedule.Covers(
		time.Date(2026, time.August, 31, 9, 0, 0, 0, time.UTC))
	previous := domain.Preceding(covers)

	if got := previous.LastBucket(); got != "2026-08-24T08" {
		t.Errorf("the previous week ends at %q, want the hour before this one starts", got)
	}
	if previous.Hours() != covers.Hours() {
		t.Errorf("the two weeks are different lengths: %d and %d", previous.Hours(), covers.Hours())
	}
	// They must not overlap by a single bucket, or one event would be counted
	// in both halves of the trend and the comparison would flatter itself.
	if !previous.To.Before(covers.From) {
		t.Errorf("the weeks overlap: previous ends %s, this one starts %s",
			previous.LastBucket(), covers.FirstBucket())
	}
}
