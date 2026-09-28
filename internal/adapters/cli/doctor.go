package cli

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/antoniojosev/trapline/internal/adapters/secrets"
	"github.com/antoniojosev/trapline/internal/adapters/sqlite"
	"github.com/antoniojosev/trapline/internal/config"
)

// runDoctor diagnoses an installation.
//
// It exists because the alternative is a support conversation. Every check
// here is a real failure someone hits on a first install: the server not
// running, the wrong URL, a token that was never created, an origin left
// pointing at localhost so the DSNs work on the box and nowhere else. The
// command's job is to name the actual problem instead of leaving someone
// reading a stack trace.
// The --quick variant answers a narrower question — is the state on disk
// readable and shaped the way this build expects — and answers it without a
// network call, because it runs as the container HEALTHCHECK a few times a
// minute for the life of the deployment. A healthcheck that reached the API
// would be reporting on the whole install, and would restart a container for
// problems a restart cannot fix.
func runDoctor(c *context_, args []string) int {
	flags := newFlagSet(c, "doctor")
	remote := addRemoteFlags(flags)
	dbPath := flags.String("db", "", "path to the database, for -quick (env TRAPLINE_DB)")
	quick := flags.Bool("quick", false, "check only the local database: it opens and its pragmas are right")
	if err := flags.Parse(args); err != nil {
		return ExitUsage
	}

	if *quick {
		return runDoctorQuick(c, resolve(*dbPath, c.env, "TRAPLINE_DB", config.DefaultDBPath), *remote.asJSON)
	}

	baseURL := strings.TrimRight(resolve(*remote.url, c.env, "TRAPLINE_URL", defaultURL), "/")
	token := resolve(*remote.token, c.env, "TRAPLINE_TOKEN", "")

	return emitDoctorReport(c, diagnose(c, baseURL, token), *remote.asJSON)
}

// runDoctorQuick is the healthcheck: open the database, read the pragmas, say
// so. Everything it reports is something a restart or a fixed mount could
// plausibly change, which is the bar for a check a supervisor acts on.
func runDoctorQuick(c *context_, dbPath string, asJSON bool) int {
	report := doctorReport{OK: true}
	add := func(name string, ok bool, format string, args ...any) {
		report.Checks = append(report.Checks, doctorCheck{
			Name: name, OK: ok, Detail: fmt.Sprintf(format, args...),
		})
		if !ok {
			report.OK = false
		}
	}

	quick, err := sqlite.QuickCheck(c.ctx, dbPath)
	if err != nil {
		add("database", false, "%v", err)
		return emitDoctorReport(c, report, asJSON)
	}
	add("database", true, "%s opens read-only", quick.Path)
	add("journal mode", true, "%s", quick.JournalMode)
	add("auto vacuum", true, "incremental")
	add("schema", true, "migration %d applied", quick.SchemaVersion)
	checkSecretKey(c, dbPath, keyFileFor(c, dbPath), add)
	return emitDoctorReport(c, report, asJSON)
}

// keyFileFor is where this installation keeps the key that encrypts channel
// credentials: what was configured, or `<db>.key`.
func keyFileFor(c *context_, dbPath string) string {
	if configured := c.env("TRAPLINE_SECRET_KEY_FILE"); configured != "" {
		return configured
	}
	return secrets.KeyPathFor(dbPath)
}

// checkSecretKey verifies that the key beside this database still opens what
// the database holds.
//
// Nothing is reported when no channel is configured: alerting is opt-in, and a
// row saying "secrets: not applicable" on every healthcheck of every
// installation that never used it would be noise arguing against ADR 005.
//
// When a channel does exist, two things are checked and they fail for very
// different reasons. Permissions catch a key file anyone on the box can read.
// Decryption catches the mistake that actually happens: a restore, or a
// container mount, that brought the `.db` and left the `.key` behind. Both are
// things a fixed mount can change, which is the bar for a check a supervisor
// acts on.
func checkSecretKey(c *context_, dbPath, keyPath string, add func(string, bool, string, ...any)) {
	channel, found, err := sqlite.NewestSealedChannel(c.ctx, dbPath)
	if err != nil {
		add("secret key", false, "%v", err)
		return
	}
	if !found {
		return
	}

	cipher := secrets.At(keyPath)
	// Check before Open, and never the other way round: Open creates a key
	// when there is none, and a diagnosis that silently mints a new key would
	// turn "your key is missing" into "your channels are gone", permanently,
	// as a side effect of asking what was wrong.
	if err := cipher.Check(); err != nil {
		add("secret key", false, "%v", err)
		return
	}
	if _, err := cipher.Open(channel.Config); err != nil {
		add("secret key", false,
			"%s does not decrypt channel %d (%s): %v; a database restored without its key file "+
				"lists its channels and delivers nothing",
			keyPath, channel.ID, channel.Name, err)
		return
	}
	add("secret key", true, "%s decrypts channel %d (%s)", keyPath, channel.ID, channel.Name)
}

func emitDoctorReport(c *context_, report doctorReport, asJSON bool) int {
	if asJSON {
		exit := ExitOK
		if !report.OK {
			exit = ExitError
		}
		// Emit before deciding the code so an agent always gets the report,
		// including — especially — when something is wrong.
		c.emit(true, report, "")
		return exit
	}

	var text strings.Builder
	for _, check := range report.Checks {
		marker := "ok  "
		if !check.OK {
			marker = "FAIL"
		}
		fmt.Fprintf(&text, "%s  %-22s %s\n", marker, check.Name, check.Detail)
	}
	fmt.Fprint(c.stdout, text.String())

	if !report.OK {
		return ExitError
	}
	return ExitOK
}

// doctorReport is the machine-readable diagnosis.
type doctorReport struct {
	OK     bool          `json:"ok"`
	Checks []doctorCheck `json:"checks"`
	// Jobs carries the background jobs with their fields intact, next to the
	// human-readable checks that summarise them. An agent asking "when did
	// retention last run" should get a timestamp, not a sentence about one
	// (ADR 006).
	Jobs []doctorJob `json:"jobs,omitempty"`
	// Channels carries the alert channels with the two halves of their
	// diagnosis apart: whether the secrets key opened the stored
	// configuration, and whether this machine can reach where it points.
	Channels []doctorChannel `json:"channels,omitempty"`
}

// doctorChannel mirrors one entry of GET /system/channels.
type doctorChannel struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	Name      string `json:"name"`
	Digest    bool   `json:"digest"`
	Endpoint  string `json:"endpoint,omitempty"`
	SecretOK  bool   `json:"secret_ok"`
	Reachable bool   `json:"reachable"`
	OK        bool   `json:"ok"`
	Detail    string `json:"detail"`
}

// doctorJob mirrors one entry of GET /system/jobs.
type doctorJob struct {
	Name            string     `json:"name"`
	IntervalSeconds int        `json:"interval_seconds"`
	StartedAt       time.Time  `json:"started_at"`
	LastRun         *time.Time `json:"last_run"`
	NextRun         *time.Time `json:"next_run"`
	LastError       *string    `json:"last_error"`
	Runs            int64      `json:"runs"`
	Failures        int64      `json:"failures"`
}

type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

func diagnose(c *context_, baseURL, token string) doctorReport {
	report := doctorReport{OK: true}

	add := func(name string, ok bool, format string, args ...any) {
		report.Checks = append(report.Checks, doctorCheck{
			Name:   name,
			OK:     ok,
			Detail: fmt.Sprintf(format, args...),
		})
		if !ok {
			report.OK = false
		}
	}

	// Reachability first: everything after it would fail for the same reason,
	// and reporting five failures for one cause is noise.
	client, err := newAPIClient(baseURL, "unauthenticated-probe")
	if err != nil {
		add("server reachable", false, "%v", err)
		return report
	}

	var health map[string]string
	if err := client.do(c.ctx, http.MethodGet, "/health", nil, &health); err != nil {
		add("server reachable", false, "cannot reach %s: %v", baseURL, err)
		return report
	}
	add("server reachable", true, "%s", baseURL)

	var setup struct {
		NeedsSetup bool `json:"needs_setup"`
	}
	if err := client.do(c.ctx, http.MethodGet, "/setup", nil, &setup); err != nil {
		add("setup status", false, "%v", err)
	} else if setup.NeedsSetup {
		add("setup complete", false, "no admin account yet; open %s and create one", baseURL)
	} else {
		add("setup complete", true, "an admin account exists")
	}

	if token == "" {
		add("api token", false, "%v", ErrNoToken)
		return report
	}

	authenticated, err := newAPIClient(baseURL, token)
	if err != nil {
		add("api token", false, "%v", err)
		return report
	}

	var projects []projectPayload
	if err := authenticated.do(c.ctx, http.MethodGet, "/projects", nil, &projects); err != nil {
		add("api token", false, "the token was rejected: %v", err)
		return report
	}
	add("api token", true, "accepted, with read access")
	add("projects", true, "%d configured", len(projects))

	// A DSN pointing at loopback is the single most common silent
	// misconfiguration: everything looks healthy and no SDK off the machine
	// can ever deliver an event.
	local := 0
	for _, project := range projects {
		if strings.Contains(project.DSN, "127.0.0.1") ||
			strings.Contains(project.DSN, "localhost") ||
			strings.Contains(project.DSN, "0.0.0.0") {
			local++
		}
	}
	if local > 0 {
		add("dsn reachability", false,
			"%d of %d DSNs point at this machine; set -origin or TRAPLINE_ORIGIN on the server to the address SDKs will use",
			local, len(projects))
	} else if len(projects) > 0 {
		add("dsn reachability", true, "all DSNs use a non-local origin")
	}

	// What the server does when nobody is asking it for anything. A job that
	// has been failing for a week looks exactly like a healthy server from
	// every other check here (ADR 014).
	var jobs struct {
		Jobs []doctorJob `json:"jobs"`
	}
	if err := authenticated.do(c.ctx, http.MethodGet, "/system/jobs", nil, &jobs); err != nil {
		add("background jobs", false, "cannot read the job list: %v", err)
		return report
	}
	report.Jobs = jobs.Jobs
	if len(jobs.Jobs) == 0 {
		// Retention is the one job that is not opt-in, so an empty list is not
		// "a quiet server", it is a server whose disk is going to fill up.
		add("background jobs", false, "nothing is running; retention is not optional and should always be here")
		return report
	}
	for _, job := range jobs.Jobs {
		if job.LastError != nil {
			add("job "+job.Name, false, "last run failed: %s", *job.LastError)
			continue
		}
		add("job "+job.Name, true, "%s", describeJob(job))
	}

	checkChannels(c, authenticated, &report, add)
	return report
}

// checkChannels asks the server whether the alarm would ring.
//
// This is the half of `doctor` that no other check can stand in for. A
// misconfigured alert channel is invisible from everywhere else: the rules
// exist, the panel shows them, every other line of this report is green — and
// the first delivery is attempted on the day something is already on fire,
// which is the worst possible moment to find out that a bot token was rotated
// in March or that an SMTP host stopped resolving.
//
// Nothing is sent. The server opens a connection to each endpoint, completes
// the TLS handshake when there is one, and hangs up; a check that posted "test"
// into a team's chat would be run once and then avoided, which is the same as
// not having it.
func checkChannels(
	c *context_, client *apiClient, report *doctorReport,
	add func(name string, ok bool, format string, args ...any),
) {
	var channels struct {
		Configured bool            `json:"configured"`
		Channels   []doctorChannel `json:"channels"`
	}
	if err := client.do(c.ctx, http.MethodGet, "/system/channels", nil, &channels); err != nil {
		var api apiError
		if errors.As(err, &api) && api.StatusCode == http.StatusNotFound {
			// An older server, or one built without the notification
			// subsystem. Not a failure of this installation: reporting one
			// would make `doctor` red for a build that never claimed to have
			// channels.
			return
		}
		add("alert channels", false, "cannot read the channel list: %v", err)
		return
	}

	report.Channels = channels.Channels
	if !channels.Configured {
		add("alert channels", true, "this build has no notification subsystem, so nothing can be alerted")
		return
	}
	if len(channels.Channels) == 0 {
		// Not a failure. An installation with no channels has made a choice,
		// and the only thing worth saying is what that choice costs.
		add("alert channels", true, "none configured; nothing will be notified when something breaks")
		return
	}

	digests := 0
	for _, channel := range channels.Channels {
		if channel.Digest {
			digests++
		}
		name := "channel " + channel.Name
		if channel.OK {
			add(name, true, "%s, %s", channel.Type, channel.Detail)
			continue
		}
		add(name, false, "%s: %s", channel.Type, channel.Detail)
	}
	if digests == 0 {
		// Said as a check rather than left for somebody to notice: an
		// operator looking for the `digest` job in the list above and not
		// finding it should find the reason here, rather than reading the
		// scheduler's source to learn that a job with nothing to do is not a
		// job (ADR 014).
		add("weekly digest", true,
			"no channel asked for it, so the digest job is not running; `trapline digest preview` still shows what it would say")
	}
}

// describeJob is the one-line summary an operator reads.
func describeJob(job doctorJob) string {
	every := (time.Duration(job.IntervalSeconds) * time.Second).String()
	if job.LastRun == nil {
		return fmt.Sprintf("every %s, has not run yet", every)
	}
	summary := fmt.Sprintf("every %s, %d run(s), last %s", every, job.Runs, job.LastRun.UTC().Format(time.RFC3339))
	if job.NextRun != nil {
		// Rounded to the second: the jitter makes the exact figure noise, and
		// what is being answered is "is it about to happen or did it stall".
		summary += fmt.Sprintf(", next in %s", time.Until(*job.NextRun).Round(time.Second))
	}
	if job.Failures > 0 {
		summary += fmt.Sprintf(", %d past failure(s)", job.Failures)
	}
	return summary
}
