package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
)

// QuickReport is what a container healthcheck needs to know about the store:
// the file is there, it opens, and the two pragmas that cannot be fixed later
// are the ones this build expects.
type QuickReport struct {
	Path          string `json:"path"`
	JournalMode   string `json:"journal_mode"`
	AutoVacuum    int    `json:"auto_vacuum"`
	SchemaVersion int    `json:"schema_version"`
}

// QuickCheck opens the database read-only and reads the pragmas. It touches
// nothing else — no network, no migration, no integrity scan.
//
// Three properties matter, because this runs as a Docker HEALTHCHECK every few
// seconds for the life of the container:
//
//   - It never creates anything. A healthcheck pointed at the wrong path must
//     say so, not quietly create an empty database next to the real one and
//     then report health forever. Hence the stat before the open, and the
//     read-only URI after it.
//   - It is cheap and bounded. PRAGMA integrity_check would grow with the
//     store, so a healthy install would get slower to declare healthy the
//     longer it ran.
//   - It answers the question a healthcheck is actually asking: is the state
//     this process depends on readable and shaped the way this build expects.
//
// auto_vacuum is checked because it is the one property of the file that
// cannot be repaired while running (see Open): a database created without it
// needs a full VACUUM offline, and finding that out during a retention sweep
// is finding out too late.
func QuickCheck(ctx context.Context, path string) (QuickReport, error) {
	report := QuickReport{Path: path}

	info, err := os.Stat(path)
	if err != nil {
		return report, fmt.Errorf("%w: %w", ErrSchema, err)
	}
	if info.IsDir() {
		return report, fmt.Errorf("%w: %s is a directory", ErrSchema, path)
	}

	handle, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return report, fmt.Errorf("opening %s read-only: %w", path, err)
	}
	defer func() { _ = handle.Close() }()
	handle.SetMaxOpenConns(1)

	if err := handle.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&report.JournalMode); err != nil {
		return report, fmt.Errorf("reading journal_mode from %s: %w", path, err)
	}
	if err := handle.QueryRowContext(ctx, "PRAGMA auto_vacuum").Scan(&report.AutoVacuum); err != nil {
		return report, fmt.Errorf("reading auto_vacuum: %w", err)
	}

	// A NULL max means the table exists but is empty, which is a database that
	// was created and never migrated — reported as version 0 rather than as an
	// error, so the check below names the real problem.
	var version sql.NullInt64
	if err := handle.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil {
		return report, fmt.Errorf("%w: reading schema_migrations from %s: %w", ErrSchema, path, err)
	}
	report.SchemaVersion = int(version.Int64)

	return report, report.problem()
}

// problem reports what is wrong with an otherwise readable database, or nil.
func (r QuickReport) problem() error {
	const incremental = 2
	var problems []error
	if r.JournalMode != "wal" {
		problems = append(problems, fmt.Errorf(
			"%w: journal_mode is %q, want \"wal\"", ErrSchema, r.JournalMode))
	}
	if r.AutoVacuum != incremental {
		problems = append(problems, fmt.Errorf(
			"%w: auto_vacuum is %d, want %d (incremental)", ErrSchema, r.AutoVacuum, incremental))
	}
	if r.SchemaVersion == 0 {
		problems = append(problems, fmt.Errorf(
			"%w: no migrations applied; this database was never opened by a server", ErrSchema))
	}
	return errors.Join(problems...)
}

// SealedChannel is one encrypted channel configuration, read without opening
// the database for writing.
type SealedChannel struct {
	// ID and Name identify the channel to a person reading a diagnosis.
	ID   int64
	Name string
	// Config is the ciphertext, for `doctor` to try the key against.
	Config []byte
}

// NewestSealedChannel reads the most recently configured channel, read-only.
//
// It exists for one check: whether the key file beside this database can still
// decrypt what the database holds. That question has exactly one likely wrong
// answer — a restore that brought the `.db` and left the `.key` behind — and
// the symptom without this check is channels that are listed, look configured
// and never deliver (ADR 015).
//
// A database with no channels reports none, and that is not a problem: it is
// an installation that has not configured alerting.
func NewestSealedChannel(ctx context.Context, path string) (SealedChannel, bool, error) {
	handle, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return SealedChannel{}, false, fmt.Errorf("opening %s read-only: %w", path, err)
	}
	defer func() { _ = handle.Close() }()
	handle.SetMaxOpenConns(1)

	var channel SealedChannel
	err = handle.QueryRowContext(ctx,
		"SELECT id, name, config_enc FROM alert_channels ORDER BY id DESC LIMIT 1").
		Scan(&channel.ID, &channel.Name, &channel.Config)
	if errors.Is(err, sql.ErrNoRows) {
		return SealedChannel{}, false, nil
	}
	if err != nil {
		return SealedChannel{}, false, fmt.Errorf("%w: reading alert_channels from %s: %w", ErrSchema, path, err)
	}
	return channel, true, nil
}
