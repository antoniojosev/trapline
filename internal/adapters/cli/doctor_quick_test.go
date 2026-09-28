package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antoniojosev/trapline/internal/adapters/sqlite"
)

func newDatabase(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trapline.db")
	db, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("creating the database: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing the database: %v", err)
	}
	return path
}

func TestDoctorQuick(t *testing.T) {
	path := newDatabase(t)

	code, stdout, stderr := run(t, "doctor", "-db", path, "--quick")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, ExitOK, stderr)
	}
	if !strings.Contains(stdout, "wal") {
		t.Errorf("stdout does not report the journal mode: %q", stdout)
	}
	if strings.Contains(stdout, "FAIL") {
		t.Errorf("a healthy database reported a failure: %q", stdout)
	}
}

// The healthcheck runs with --json in some setups and without it in others,
// but the thing a supervisor reads is the exit code. It has to be 1 for a
// database that is not there, whichever way the report is rendered.
func TestDoctorQuickFailsOnAMissingDatabase(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nowhere.db")

	for _, args := range [][]string{
		{"doctor", "-db", missing, "--quick"},
		{"doctor", "-db", missing, "--quick", "--json"},
	} {
		code, stdout, _ := run(t, args...)
		if code != ExitError {
			t.Errorf("%v: exit code = %d, want %d", args, code, ExitError)
		}
		if stdout == "" {
			t.Errorf("%v: nothing was reported", args)
		}
	}

	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("the check created the database it was meant to find missing")
	}
}

func TestDoctorQuickJSONIsAReport(t *testing.T) {
	path := newDatabase(t)

	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"doctor", "--quick", "--json"},
		func(key string) string {
			if key == "TRAPLINE_DB" {
				return path
			}
			return ""
		}, &out, &errOut)
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, ExitOK, errOut.String())
	}

	var report doctorReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decoding the report: %v", err)
	}
	if !report.OK || len(report.Checks) == 0 {
		t.Errorf("report = %+v, want an ok report with checks", report)
	}
}

// --quick must not reach the network: it runs inside a container whose only
// certainty is the volume. With no server anywhere and TRAPLINE_URL pointing
// at a closed port, it still has to answer about the file.
func TestDoctorQuickMakesNoNetworkCall(t *testing.T) {
	path := newDatabase(t)

	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"doctor", "-db", path, "--quick"},
		func(key string) string {
			if key == "TRAPLINE_URL" {
				// Port 1 is reserved and never listening; a check that
				// reached for it would fail rather than pass.
				return "http://127.0.0.1:1"
			}
			return ""
		}, &out, &errOut)
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d (stdout: %s)", code, ExitOK, out.String())
	}
}
