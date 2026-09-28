package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

var _ ports.SettingsStore = (*SettingsRepository)(nil)

// SettingsRepository stores installation-wide settings as key and JSON value.
type SettingsRepository struct {
	db *DB
}

// NewSettingsRepository wires the repository to an open database.
func NewSettingsRepository(db *DB) *SettingsRepository {
	return &SettingsRepository{db: db}
}

// Setting reads one value.
//
// A key that has never been written is domain.ErrSettingNotFound and not an
// empty document, because the caller's next move differs: one applies its
// default, the other has been configured to something and must not be
// overridden by one.
func (r *SettingsRepository) Setting(ctx context.Context, key string) (json.RawMessage, error) {
	var value string
	err := r.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %q", domain.ErrSettingNotFound, key)
	}
	if err != nil {
		return nil, fmt.Errorf("reading setting %q: %w", key, err)
	}
	return json.RawMessage(value), nil
}

// SetSetting writes one value, replacing whatever was there.
//
// The value is checked for being JSON here as well as by the table's CHECK
// constraint. Not redundancy for its own sake: the constraint's error names a
// constraint, and this one names the key, which is the difference between a
// message an operator can act on and one they have to go looking for.
func (r *SettingsRepository) SetSetting(ctx context.Context, key string, value json.RawMessage) error {
	if !json.Valid(value) {
		return fmt.Errorf("%w: the value for %q is not JSON", domain.ErrInvalidSetting, key)
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, string(value), formatTime(nowUTC()))
	if err != nil {
		return fmt.Errorf("writing setting %q: %w", key, err)
	}
	return nil
}
