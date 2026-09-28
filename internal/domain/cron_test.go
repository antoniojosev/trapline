package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("this machine has no tzdata for %s: %v", name, err)
	}
	return loc
}

func TestParseCronAccepts(t *testing.T) {
	for _, spec := range []string{
		"* * * * *",
		"*/1 * * * *",
		"0 3 * * *",
		"0,15,30,45 * * * *",
		"0-30/5 * * * *",
		"5/15 * * * *",
		"0 0 1 1 *",
		"0 0 * * 7",
		"30 2 * * 1-5",
		"@hourly", "@daily", "@weekly", "@monthly",
		"@DAILY",
		"  0   3   *   *   *  ",
	} {
		if _, err := domain.ParseCron(spec); err != nil {
			t.Errorf("ParseCron(%q) = %v, want it accepted", spec, err)
		}
	}
}

func TestParseCronRejects(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"four fields":      "* * * *",
		"six fields":       "* * * * * *",
		"unknown alias":    "@yearly",
		"minute too big":   "60 * * * *",
		"hour too big":     "* 24 * * *",
		"day zero":         "* * 0 * *",
		"month too big":    "* * * 13 *",
		"weekday too big":  "* * * * 8",
		"name not number":  "* * * * MON",
		"backwards range":  "30-10 * * * *",
		"zero step":        "*/0 * * * *",
		"negative step":    "*/-1 * * * *",
		"step not numeric": "*/x * * * *",
		"empty item":       "1,,2 * * * *",
		"too long":         strings.Repeat("1,", 200) + "1 * * * *",
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := domain.ParseCron(spec)
			if err == nil {
				t.Fatalf("ParseCron(%q) accepted it", spec)
			}
			if !errors.Is(err, domain.ErrInvalidMonitor) {
				t.Fatalf("error is %v, want it to wrap ErrInvalidMonitor", err)
			}
		})
	}
}

// The backwards range explains itself rather than saying "invalid": somebody
// who wrote 22-3 meant a night shift, and the message is what tells them how
// to write one.
func TestParseCronExplainsBackwardsRange(t *testing.T) {
	_, err := domain.ParseCron("0 22-3 * * *")
	if err == nil {
		t.Fatal("22-3 was accepted")
	}
	if !strings.Contains(err.Error(), "22-23,0-3") {
		t.Fatalf("message %q does not suggest the two-item form", err)
	}
}

func TestCronNextTable(t *testing.T) {
	utc := time.UTC
	cases := []struct {
		name  string
		spec  string
		from  time.Time
		want  time.Time
		zone  *time.Location
		alias bool
	}{
		{
			name: "every minute moves to the next whole minute",
			spec: "* * * * *",
			from: time.Date(2026, 3, 1, 10, 0, 30, 0, utc),
			want: time.Date(2026, 3, 1, 10, 1, 0, 0, utc),
		},
		{
			name: "a time already on the minute still advances",
			spec: "* * * * *",
			from: time.Date(2026, 3, 1, 10, 0, 0, 0, utc),
			want: time.Date(2026, 3, 1, 10, 1, 0, 0, utc),
		},
		{
			name: "daily at three, later today",
			spec: "0 3 * * *",
			from: time.Date(2026, 3, 1, 1, 0, 0, 0, utc),
			want: time.Date(2026, 3, 1, 3, 0, 0, 0, utc),
		},
		{
			name: "daily at three, tomorrow",
			spec: "0 3 * * *",
			from: time.Date(2026, 3, 1, 3, 0, 0, 0, utc),
			want: time.Date(2026, 3, 2, 3, 0, 0, 0, utc),
		},
		{
			name: "a step list",
			spec: "0,15,30,45 * * * *",
			from: time.Date(2026, 3, 1, 10, 16, 0, 0, utc),
			want: time.Date(2026, 3, 1, 10, 30, 0, 0, utc),
		},
		{
			name: "a range with a step",
			spec: "0-30/10 * * * *",
			from: time.Date(2026, 3, 1, 10, 21, 0, 0, utc),
			want: time.Date(2026, 3, 1, 10, 30, 0, 0, utc),
		},
		{
			name: "a range with a step wraps to the next hour",
			spec: "0-30/10 * * * *",
			from: time.Date(2026, 3, 1, 10, 31, 0, 0, utc),
			want: time.Date(2026, 3, 1, 11, 0, 0, 0, utc),
		},
		{
			name: "weekdays only",
			spec: "0 9 * * 1-5",
			// A Saturday.
			from: time.Date(2026, 3, 7, 12, 0, 0, 0, utc),
			want: time.Date(2026, 3, 9, 9, 0, 0, 0, utc),
		},
		{
			name: "seven is sunday",
			spec: "0 9 * * 7",
			from: time.Date(2026, 3, 7, 12, 0, 0, 0, utc),
			want: time.Date(2026, 3, 8, 9, 0, 0, 0, utc),
		},
		{
			name: "a month it does not run in is skipped whole",
			spec: "0 0 1 7 *",
			from: time.Date(2026, 3, 1, 0, 0, 0, 0, utc),
			want: time.Date(2026, 7, 1, 0, 0, 0, 0, utc),
		},
		{
			name: "the twenty-ninth of february is found in a leap year",
			spec: "0 0 29 2 *",
			from: time.Date(2026, 3, 1, 0, 0, 0, 0, utc),
			want: time.Date(2028, 2, 29, 0, 0, 0, 0, utc),
		},
		{
			// Cron's oldest rule: with both day fields restricted they are
			// OR'd. The first of the month comes before the next Monday here,
			// and an AND would have returned neither.
			name: "day-of-month and day-of-week are OR'd when both are set",
			spec: "0 0 1 * 1",
			from: time.Date(2026, 3, 26, 0, 0, 0, 0, utc), // a Thursday
			want: time.Date(2026, 3, 30, 0, 0, 0, 0, utc), // the Monday
		},
		{
			name:  "hourly alias",
			spec:  "@hourly",
			from:  time.Date(2026, 3, 1, 10, 30, 0, 0, utc),
			want:  time.Date(2026, 3, 1, 11, 0, 0, 0, utc),
			alias: true,
		},
		{
			name:  "weekly alias lands on a sunday",
			spec:  "@weekly",
			from:  time.Date(2026, 3, 3, 10, 0, 0, 0, utc),
			want:  time.Date(2026, 3, 8, 0, 0, 0, 0, utc),
			alias: true,
		},
		{
			name:  "monthly alias",
			spec:  "@monthly",
			from:  time.Date(2026, 3, 3, 10, 0, 0, 0, utc),
			want:  time.Date(2026, 4, 1, 0, 0, 0, 0, utc),
			alias: true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			schedule, err := domain.ParseCron(testCase.spec)
			if err != nil {
				t.Fatalf("ParseCron(%q): %v", testCase.spec, err)
			}
			zone := testCase.zone
			if zone == nil {
				zone = utc
			}
			got, err := schedule.Next(testCase.from, zone)
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if !got.Equal(testCase.want) {
				t.Fatalf("Next(%s) = %s, want %s", testCase.from, got, testCase.want)
			}
		})
	}
}

// A schedule is a wall-clock statement, so the same expression is a different
// instant in a different country. This is the whole reason the timezone is a
// per-monitor field: without it, "three in the morning" in Caracas would be
// watched as three in the morning in whatever zone the server happens to run.
func TestCronNextIsWallClockInItsZone(t *testing.T) {
	caracas := mustLoad(t, "America/Caracas")
	schedule, err := domain.ParseCron("0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}

	// 06:00 UTC on the first is 02:00 in Caracas (UTC-4), so the next run is
	// still today there — and would already have passed in UTC.
	from := time.Date(2026, 3, 1, 6, 0, 0, 0, time.UTC)
	got, err := schedule.Next(from, caracas)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 3, 1, 7, 0, 0, 0, time.UTC) // 03:00 -04:00
	if !got.Equal(want) {
		t.Fatalf("Next in Caracas = %s (%s UTC), want %s", got, got.UTC(), want)
	}

	// The same instant in UTC crosses the day boundary the other way: it is
	// already past three in the morning, so the next one is tomorrow.
	inUTC, err := schedule.Next(from, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if !inUTC.Equal(time.Date(2026, 3, 2, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("Next in UTC = %s, want the second at 03:00", inUTC)
	}
	if !inUTC.After(got) {
		t.Fatal("the two zones produced the same deadline, so the zone is being ignored")
	}
}

// A zone that still changes its clocks, across the spring gap. The run at
// 02:30 does not exist on that date, and the schedule must still produce a
// deadline that is strictly in the future rather than looping.
func TestCronNextCrossesADaylightSavingGap(t *testing.T) {
	madrid := mustLoad(t, "Europe/Madrid")
	schedule, err := domain.ParseCron("30 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-03-29 is the spring transition in Madrid: 02:00 becomes 03:00.
	from := time.Date(2026, 3, 28, 12, 0, 0, 0, madrid)
	got, err := schedule.Next(from, madrid)
	if err != nil {
		t.Fatal(err)
	}
	if !got.After(from) {
		t.Fatalf("Next(%s) = %s, which is not later", from, got)
	}
	// And the one after that is the following day at half past two, back to
	// normal — the gap costs one occurrence and nothing more.
	after, err := schedule.Next(got, madrid)
	if err != nil {
		t.Fatal(err)
	}
	if !after.After(got) {
		t.Fatalf("Next(%s) = %s, which is not later", got, after)
	}
}

func TestCronNextRejectsAScheduleThatNeverHappens(t *testing.T) {
	schedule, err := domain.ParseCron("0 0 30 2 *")
	if err != nil {
		t.Fatalf("30 February parses (it is a legal expression): %v", err)
	}
	if _, err := schedule.Next(time.Now(), time.UTC); err == nil {
		t.Fatal("Next found an occurrence of 30 February")
	}
}

func TestCronZeroValue(t *testing.T) {
	var schedule domain.CronSchedule
	if !schedule.IsZero() {
		t.Fatal("the zero schedule does not report itself as zero")
	}
	if _, err := schedule.Next(time.Now(), nil); err == nil {
		t.Fatal("the zero schedule produced an occurrence")
	}
}

func TestCronNextDefaultsToUTC(t *testing.T) {
	schedule, err := domain.ParseCron("0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}
	withNil, err := schedule.Next(time.Date(2026, 3, 1, 1, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !withNil.Equal(time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("a nil location gave %s", withNil)
	}
}

func TestCronStringRoundTrips(t *testing.T) {
	schedule, err := domain.ParseCron(" @daily ")
	if err != nil {
		t.Fatal(err)
	}
	// As written, not as expanded: a monitor is shown to the person who
	// created it in the words they used.
	if schedule.String() != "@daily" {
		t.Fatalf("String() = %q, want the expression as written", schedule.String())
	}
}

func TestCronAliasesAreNamedInTheError(t *testing.T) {
	_, err := domain.ParseCron("@fortnightly")
	if err == nil {
		t.Fatal("an unknown alias was accepted")
	}
	for _, alias := range domain.CronAliases() {
		if !strings.Contains(err.Error(), alias) {
			t.Fatalf("the error does not name %s: %v", alias, err)
		}
	}
}

// FuzzCronNext asserts the one property everything else rests on: whatever
// expression comes out of the parser, the next occurrence is strictly later
// than the instant it was asked about.
//
// It is the property that makes the watcher terminate. A Next that could
// return its own input would leave the sweep re-firing the same deadline
// forever, and a Next that could go backwards would re-fire it faster. The
// timezone is fuzzed too, because that is where the arithmetic is hard: the
// zones below are the ones that move their clocks, plus one whose offset is
// not a whole hour.
func FuzzCronNext(f *testing.F) {
	for _, seed := range []string{
		"* * * * *", "@daily", "0 3 * * *", "*/7 1-5 1,15 */2 1-5",
		"0-59/2 0-23/3 1-31/4 1-12/5 0-7/2", "59 23 31 12 6", "@weekly",
	} {
		for zone := range 4 {
			f.Add(seed, zone, int64(1772000000))
		}
	}

	zones := []string{"UTC", "America/Caracas", "Europe/Madrid", "Australia/Eucla"}

	f.Fuzz(func(t *testing.T, spec string, zoneIndex int, unix int64) {
		schedule, err := domain.ParseCron(spec)
		if err != nil {
			return
		}
		loc, err := time.LoadLocation(zones[((zoneIndex%len(zones))+len(zones))%len(zones)])
		if err != nil {
			t.Skip("no tzdata on this machine")
		}
		// Kept inside the window the storage layer can represent, because
		// outside it the question is meaningless rather than wrong.
		if unix < 0 || unix > 253402300799 {
			return
		}
		from := time.Unix(unix, 0).UTC()

		next, err := schedule.Next(from, loc)
		if err != nil {
			// "No occurrence within four years" is a legitimate answer for
			// expressions like 30 February.
			return
		}
		if !next.After(from) {
			t.Fatalf("Next(%q, %s, %s) = %s, which is not after it", spec, from, loc, next)
		}
		// And it is idempotent in the direction that matters: asking again
		// from the answer moves forward again, which is what the sweep does
		// every time it advances a deadline.
		again, err := schedule.Next(next, loc)
		if err != nil {
			return
		}
		if !again.After(next) {
			t.Fatalf("Next(%q, %s) = %s, which is not after it", spec, next, again)
		}
	})
}
