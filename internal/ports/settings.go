package ports

import (
	"context"
	"encoding/json"
)

// SettingsStore holds the handful of facts that belong to the installation
// rather than to any project.
//
// Key-value, with a JSON document as the value. Deliberately not a typed
// interface per setting: every consumer knows the shape of its own setting
// and nothing else does, so a store that understood the shapes would be a
// second place to change every time one of them grows a field.
//
// It is one port and not several because the alternative — one repository per
// setting — means a migration and a table for every installation-wide fact,
// and the first three of them (the digest schedule, the status page's title,
// what the MCP surface remembers) are each one line long.
type SettingsStore interface {
	// Setting reads one value, returning domain.ErrSettingNotFound when the
	// key has never been written. Not an empty document: "never configured"
	// and "configured to nothing" are different answers, and a caller that
	// cannot tell them apart cannot apply a default.
	Setting(ctx context.Context, key string) (json.RawMessage, error)
	// SetSetting writes one value, replacing whatever was there.
	SetSetting(ctx context.Context, key string, value json.RawMessage) error
}
