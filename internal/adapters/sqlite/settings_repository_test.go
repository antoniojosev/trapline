package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/antoniojosev/trapline/internal/domain"
)

func TestSettingNotWrittenIsNotFound(t *testing.T) {
	settings := NewSettingsRepository(openTemp(t))

	_, err := settings.Setting(context.Background(), "digest.schedule")
	if !errors.Is(err, domain.ErrSettingNotFound) {
		t.Errorf("reading an unwritten key = %v, want ErrSettingNotFound", err)
	}
}

func TestSettingRoundTripsAndReplaces(t *testing.T) {
	settings := NewSettingsRepository(openTemp(t))
	ctx := context.Background()

	if err := settings.SetSetting(ctx, "digest.schedule", json.RawMessage(`{"weekday":1,"hour":9}`)); err != nil {
		t.Fatalf("writing: %v", err)
	}
	value, err := settings.Setting(ctx, "digest.schedule")
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if string(value) != `{"weekday":1,"hour":9}` {
		t.Errorf("read back %q", value)
	}

	// Writing the same key again replaces it rather than failing on the
	// primary key: a setting is a value, not an append-only log.
	if err := settings.SetSetting(ctx, "digest.schedule", json.RawMessage(`{"weekday":5,"hour":17}`)); err != nil {
		t.Fatalf("rewriting: %v", err)
	}
	value, err = settings.Setting(ctx, "digest.schedule")
	if err != nil {
		t.Fatalf("reading again: %v", err)
	}
	if string(value) != `{"weekday":5,"hour":17}` {
		t.Errorf("the second write did not replace the first: %q", value)
	}
}

// TestSettingRefusesSomethingThatIsNotJSON is checked in Go as well as by the
// table's CHECK, because the two messages point at different things: the
// constraint names a constraint and this one names the key, which is what an
// operator can act on.
func TestSettingRefusesSomethingThatIsNotJSON(t *testing.T) {
	settings := NewSettingsRepository(openTemp(t))

	err := settings.SetSetting(context.Background(), "digest.schedule", json.RawMessage(`{"weekday":`))
	if !errors.Is(err, domain.ErrInvalidSetting) {
		t.Errorf("writing a broken document = %v, want ErrInvalidSetting", err)
	}
}

// TestSettingsKeepKeysApart, because the whole point of one shared table is
// that three subsystems can use it without colliding.
func TestSettingsKeepKeysApart(t *testing.T) {
	settings := NewSettingsRepository(openTemp(t))
	ctx := context.Background()

	if err := settings.SetSetting(ctx, "digest.schedule", json.RawMessage(`{"hour":9}`)); err != nil {
		t.Fatalf("writing: %v", err)
	}
	if err := settings.SetSetting(ctx, "digest.last_sent", json.RawMessage(`{"at":"2026-08-31T09:00:00Z"}`)); err != nil {
		t.Fatalf("writing: %v", err)
	}

	schedule, err := settings.Setting(ctx, "digest.schedule")
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if string(schedule) != `{"hour":9}` {
		t.Errorf("one key overwrote another: %q", schedule)
	}
}

// TestSettingsRecordWhenTheyWereWritten: "was this ever configured, or is it
// the default" is the first question when a digest arrives at the wrong hour.
func TestSettingsRecordWhenTheyWereWritten(t *testing.T) {
	db := openTemp(t)
	settings := NewSettingsRepository(db)
	ctx := context.Background()

	if err := settings.SetSetting(ctx, "digest.schedule", json.RawMessage(`{"hour":9}`)); err != nil {
		t.Fatalf("writing: %v", err)
	}

	var stored string
	if err := db.QueryRowContext(ctx,
		"SELECT updated_at FROM settings WHERE key = ?", "digest.schedule").Scan(&stored); err != nil {
		t.Fatalf("reading updated_at: %v", err)
	}
	if _, err := parseTime(stored); err != nil {
		t.Errorf("updated_at holds %q, which is not a stored instant: %v", stored, err)
	}
}
