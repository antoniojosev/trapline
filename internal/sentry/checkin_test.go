package sentry

import (
	"errors"
	"strings"
	"testing"
)

func TestDecodeCheckInWithACrontabDeclaration(t *testing.T) {
	// The shape the Python SDK's @monitor decorator sends.
	payload := `{
		"check_in_id": "b1a2c3",
		"monitor_slug": "nightly-backup",
		"status": "in_progress",
		"environment": "production",
		"release": "app@1.0.0",
		"monitor_config": {
			"schedule": {"type": "crontab", "value": "0 3 * * *"},
			"checkin_margin": 5,
			"max_runtime": 10,
			"timezone": "America/Caracas"
		}
	}`
	checkIn, err := DecodeCheckIn([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case checkIn.CheckInID != "b1a2c3":
		t.Fatalf("check-in id = %q", checkIn.CheckInID)
	case checkIn.MonitorSlug != "nightly-backup":
		t.Fatalf("slug = %q", checkIn.MonitorSlug)
	case checkIn.Status != "in_progress":
		t.Fatalf("status = %q", checkIn.Status)
	case checkIn.Environment != "production" || checkIn.Release != "app@1.0.0":
		t.Fatalf("environment = %q, release = %q", checkIn.Environment, checkIn.Release)
	case checkIn.HasDuration:
		t.Fatal("a check-in with no duration reported one")
	case checkIn.Config == nil:
		t.Fatal("the declaration was dropped")
	case checkIn.Config.ScheduleType != "crontab" || checkIn.Config.Crontab != "0 3 * * *":
		t.Fatalf("schedule = %+v", checkIn.Config)
	case checkIn.Config.Timezone != "America/Caracas":
		t.Fatalf("timezone = %q", checkIn.Config.Timezone)
	case checkIn.Config.CheckinMarginMinutes == nil || *checkIn.Config.CheckinMarginMinutes != 5:
		t.Fatalf("margin = %v", checkIn.Config.CheckinMarginMinutes)
	case checkIn.Config.MaxRuntimeMinutes == nil || *checkIn.Config.MaxRuntimeMinutes != 10:
		t.Fatalf("max runtime = %v", checkIn.Config.MaxRuntimeMinutes)
	}
}

func TestDecodeCheckInWithAnIntervalDeclaration(t *testing.T) {
	payload := `{"monitor_slug":"poller","status":"ok","duration":12.5,
		"monitor_config":{"schedule":{"type":"interval","value":5,"unit":"minute"}}}`
	checkIn, err := DecodeCheckIn([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if !checkIn.HasDuration || checkIn.DurationSeconds != 12.5 {
		t.Fatalf("duration = %v (%v)", checkIn.DurationSeconds, checkIn.HasDuration)
	}
	if checkIn.Config.ScheduleType != "interval" ||
		checkIn.Config.IntervalValue != 5 || checkIn.Config.IntervalUnit != "minute" {
		t.Fatalf("schedule = %+v", checkIn.Config)
	}
}

// The bare-string form. Undocumented, sent by hand-written payloads and by
// more than one SDK, and refusing it would fail on the simplest thing anybody
// writes.
func TestDecodeCheckInAcceptsAScheduleAsAString(t *testing.T) {
	checkIn, err := DecodeCheckIn([]byte(
		`{"monitor_slug":"backup","status":"ok","monitor_config":{"schedule":"@daily"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if checkIn.Config.ScheduleType != "crontab" || checkIn.Config.Crontab != "@daily" {
		t.Fatalf("config = %+v", checkIn.Config)
	}
}

// A schedule with no type is a crontab, which is what every SDK that omits it
// means.
func TestDecodeCheckInDefaultsTheScheduleTypeToCrontab(t *testing.T) {
	checkIn, err := DecodeCheckIn([]byte(
		`{"monitor_slug":"backup","status":"ok","monitor_config":{"schedule":{"value":"0 * * * *"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if checkIn.Config.ScheduleType != "crontab" || checkIn.Config.Crontab != "0 * * * *" {
		t.Fatalf("config = %+v", checkIn.Config)
	}
}

// An SDK that starts sending a field this build does not know must not break
// an installation that has not been upgraded (ADR 002).
func TestDecodeCheckInIgnoresWhatItDoesNotKnow(t *testing.T) {
	checkIn, err := DecodeCheckIn([]byte(
		`{"monitor_slug":"backup","status":"ok","contexts":{"trace":{"trace_id":"x"}},
		  "monitor_config":{"schedule":"@daily","failure_issue_threshold":3,"recovery_threshold":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	if checkIn.MonitorSlug != "backup" || checkIn.Config == nil {
		t.Fatalf("check-in = %+v", checkIn)
	}
}

func TestDecodeCheckInRejects(t *testing.T) {
	cases := map[string]string{
		"not json":                `{`,
		"no slug":                 `{"status":"ok"}`,
		"blank slug":              `{"monitor_slug":"   ","status":"ok"}`,
		"config is not an object": `{"monitor_slug":"backup","monitor_config":7}`,
		"a crontab that is a number": `{"monitor_slug":"backup","monitor_config":
			{"schedule":{"type":"crontab","value":5}}}`,
		"an interval that is a string": `{"monitor_slug":"backup","monitor_config":
			{"schedule":{"type":"interval","value":"five","unit":"minute"}}}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeCheckIn([]byte(payload)); !errors.Is(err, ErrInvalidCheckIn) {
				t.Fatalf("err = %v, want ErrInvalidCheckIn", err)
			}
		})
	}
}

// A monitor_config with no schedule declares nothing, and that is not an
// error: the check-in is still a check-in.
func TestAConfigWithoutAScheduleDeclaresNothing(t *testing.T) {
	checkIn, err := DecodeCheckIn([]byte(
		`{"monitor_slug":"backup","status":"ok","monitor_config":{"timezone":"UTC"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if checkIn.Config != nil {
		t.Fatalf("config = %+v, want nothing declared", checkIn.Config)
	}
	if checkIn.MonitorSlug != "backup" {
		t.Fatalf("the check-in itself was lost: %+v", checkIn)
	}
}

func TestANullConfigIsNoConfig(t *testing.T) {
	checkIn, err := DecodeCheckIn([]byte(`{"monitor_slug":"backup","status":"ok","monitor_config":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if checkIn.Config != nil {
		t.Fatal("a null monitor_config produced a declaration")
	}
}

func TestTheErrorSaysItIsACheckIn(t *testing.T) {
	_, err := DecodeCheckIn([]byte(`{"status":"ok"}`))
	if err == nil || !strings.Contains(err.Error(), "monitor_slug") {
		t.Fatalf("err = %v, which does not say what is missing", err)
	}
	// And it is still an invalid-event error, so the ingest path's one
	// mapping covers it.
	if !errors.Is(err, ErrInvalidEvent) {
		t.Fatal("a bad check-in is not an invalid event")
	}
}

// FuzzDecodeCheckIn: this payload arrives on the public ingest endpoint,
// authenticated only by a key that ships inside browser bundles, so every
// byte reaching it is attacker-chosen (ADR 002).
func FuzzDecodeCheckIn(f *testing.F) {
	f.Add(`{"monitor_slug":"backup","status":"ok"}`)
	f.Add(`{"monitor_slug":"b","monitor_config":{"schedule":{"type":"interval","value":1e308,"unit":"minute"}}}`)
	f.Add(`{"monitor_slug":"b","monitor_config":{"schedule":[]}}`)
	f.Add(`{"monitor_slug":"b","duration":-0.0,"monitor_config":{"schedule":"@daily","checkin_margin":-1}}`)

	f.Fuzz(func(t *testing.T, payload string) {
		checkIn, err := DecodeCheckIn([]byte(payload))
		if err != nil {
			return
		}
		// The one invariant a caller relies on: a decoded check-in always
		// names a monitor, because everything downstream is keyed by it.
		if strings.TrimSpace(checkIn.MonitorSlug) == "" {
			t.Fatalf("decoded a check-in with no monitor from %q", payload)
		}
	})
}
