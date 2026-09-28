package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/antoniojosev/trapline/internal/digest"
	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
)

// The settings keys this use case owns. Namespaced by subsystem, so the one
// table the whole installation shares does not become a flat list of words
// nobody can attribute (migration 0016).
const (
	// DigestScheduleKey holds the weekday and hour the report goes out.
	DigestScheduleKey = "digest.schedule"
	// DigestLastSentKey holds the scheduled moment most recently reported on.
	//
	// Persisted rather than kept in memory, and that is the whole of the
	// restart story: a server that restarts twice on a Monday morning must
	// not send two digests, and one that is down at nine and comes back at
	// ten must still send one.
	DigestLastSentKey = "digest.last_sent"
)

// DigestInterval is how often the job wakes to ask whether the scheduled
// moment has passed.
//
// Ten minutes, which is the accuracy of a weekly mail's arrival and the cost
// of one indexed read per ten minutes. The alternative — sleeping until the
// exact moment — sounds tidier and is worse: the sleep would have to be
// recomputed whenever the schedule changed, survive a clock jump and be
// interruptible, and all three are ways to get a weekly report to arrive
// never.
const DigestInterval = 10 * time.Minute

// DigestListLimit is how many issues each list in the report shows.
const DigestListLimit = digest.ListLimit

// Digest composes the weekly report and decides when it goes out.
//
// Every number in it comes from the hourly buckets and from indexed columns
// on issues. Nothing here reads an event, which is what lets the report cover
// a week whose payloads retention has already deleted (ADR 001, ADR 010).
type Digest struct {
	projects ports.ProjectRepository
	stats    ports.StatsRepository
	digests  ports.DigestRepository
	settings ports.SettingsStore
	clock    ports.Clock
	origin   domain.Origin

	// channels is the notification subsystem, and it is optional. Without it
	// there is no job — a digest with nowhere to go is not a quiet
	// subsystem, it is an absent one (ADR 014, ADR 035) — and the preview
	// still works, because previewing is what somebody does *before*
	// configuring a channel.
	channels ports.AlertChannels

	log *slog.Logger
}

// NewDigest wires the use case.
func NewDigest(
	projects ports.ProjectRepository,
	stats ports.StatsRepository,
	digests ports.DigestRepository,
	settings ports.SettingsStore,
	clock ports.Clock,
	origin domain.Origin,
) *Digest {
	return &Digest{
		projects: projects,
		stats:    stats,
		digests:  digests,
		settings: settings,
		clock:    clock,
		origin:   origin,
		log:      slog.Default(),
	}
}

// WithChannels attaches the notification subsystem, which is what turns the
// digest from something you can preview into something that gets sent.
func (d *Digest) WithChannels(channels ports.AlertChannels) *Digest {
	d.channels = channels
	return d
}

// WithLogger replaces the logger, for a test that wants the output quiet.
func (d *Digest) WithLogger(log *slog.Logger) *Digest {
	if log != nil {
		d.log = log
	}
	return d
}

// Schedule reads the configured send time, falling back to the default.
//
// A stored schedule that no longer validates is treated as absent and said
// so, rather than disabling the digest: the subsystem was switched on by
// somebody, and answering a corrupt row with silence is how a feature stops
// working without anybody being told.
func (d *Digest) Schedule(ctx context.Context) (domain.DigestSchedule, error) {
	raw, err := d.settings.Setting(ctx, DigestScheduleKey)
	if errors.Is(err, domain.ErrSettingNotFound) {
		return domain.DefaultDigestSchedule, nil
	}
	if err != nil {
		return domain.DigestSchedule{}, err
	}

	var stored scheduleSetting
	if err := json.Unmarshal(raw, &stored); err != nil {
		d.log.Warn("the stored digest schedule is not readable, using the default",
			"stored", string(raw), "error", err, "default", domain.DefaultDigestSchedule.String())
		return domain.DefaultDigestSchedule, nil
	}

	schedule := domain.DigestSchedule{Weekday: time.Weekday(stored.Weekday), Hour: stored.Hour}
	if err := schedule.Validate(); err != nil {
		d.log.Warn("the stored digest schedule is not a time, using the default",
			"stored", string(raw), "error", err, "default", domain.DefaultDigestSchedule.String())
		return domain.DefaultDigestSchedule, nil
	}
	return schedule, nil
}

// SetSchedule writes the send time.
func (d *Digest) SetSchedule(ctx context.Context, schedule domain.DigestSchedule) error {
	if err := schedule.Validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(scheduleSetting{
		Weekday: int(schedule.Weekday),
		Hour:    schedule.Hour,
	})
	if err != nil {
		return fmt.Errorf("encoding the digest schedule: %w", err)
	}
	return d.settings.SetSetting(ctx, DigestScheduleKey, encoded)
}

// scheduleSetting is the stored shape of a schedule.
//
// The weekday is a number and not a name, because it is stored and not
// printed: a number cannot be spelled two ways, and the one place a human
// types a day is the CLI, which parses it.
type scheduleSetting struct {
	Weekday int `json:"weekday"`
	Hour    int `json:"hour"`
}

// lastSentSetting is the stored marker of the last period reported on.
type lastSentSetting struct {
	At time.Time `json:"at"`
}

// PreviewOptions narrows what a preview covers.
type PreviewOptions struct {
	// ProjectID limits the report to one project. Zero means every project.
	ProjectID int64
	// At is the scheduled moment to report as of. Zero means now, which is
	// what "what would go out if it went out this minute" means.
	At time.Time
}

// Preview builds the report and renders it, without sending anything.
//
// It exists because the alternative way to find out what a weekly mail says
// is to wait a week. It is also the only part of this that works before any
// channel is configured, which is the order somebody actually does things in:
// see the thing, then decide where to send it.
func (d *Digest) Preview(ctx context.Context, options PreviewOptions) (digest.Report, string, error) {
	sendAt := options.At
	if sendAt.IsZero() {
		sendAt = d.clock.Now()
	}
	schedule, err := d.Schedule(ctx)
	if err != nil {
		return digest.Report{}, "", err
	}

	report, err := d.build(ctx, schedule.Covers(sendAt), sendAt, options.ProjectID)
	if err != nil {
		return digest.Report{}, "", err
	}
	rendered, err := digest.Render(report)
	if err != nil {
		return digest.Report{}, "", err
	}
	return report, rendered, nil
}

// build gathers one report.
func (d *Digest) build(
	ctx context.Context, covers domain.Range, sendAt time.Time, onlyProject int64,
) (digest.Report, error) {
	previous := domain.Preceding(covers)

	projects, err := d.projects.List(ctx)
	if err != nil {
		return digest.Report{}, err
	}

	report := digest.Report{
		SentAt:   sendAt.UTC().Truncate(time.Hour),
		Covers:   digest.Window{From: covers.FirstBucket(), To: covers.LastBucket()},
		Previous: digest.Window{From: previous.FirstBucket(), To: previous.LastBucket()},
		Origin:   d.publicOrigin(),
	}

	for _, project := range projects {
		if onlyProject != 0 && project.ID != onlyProject {
			continue
		}
		section, err := d.project(ctx, project, covers, previous)
		if err != nil {
			return digest.Report{}, err
		}
		// A project that recorded nothing in either week is left out
		// entirely. A digest is read for what changed, and a column of
		// zeroes is what makes somebody stop reading one.
		if section.Events == 0 && section.PreviousEvents == 0 &&
			section.NewIssues == 0 && section.RegressedIssues == 0 {
			continue
		}
		report.Projects = append(report.Projects, section)
	}
	return report, nil
}

// project is one project's section.
func (d *Digest) project(
	ctx context.Context, project domain.Project, covers, previous domain.Range,
) (digest.Project, error) {
	section := digest.Project{ID: project.ID, Name: project.Name}

	thisWeek, err := d.total(ctx, project.ID, covers)
	if err != nil {
		return digest.Project{}, err
	}
	lastWeek, err := d.total(ctx, project.ID, previous)
	if err != nil {
		return digest.Project{}, err
	}
	section.Events, section.PreviousEvents = thisWeek, lastWeek

	fresh, err := d.digests.NewIssues(ctx, project.ID, covers, DigestListLimit)
	if err != nil {
		return digest.Project{}, err
	}
	section.NewIssues, section.New = fresh.Total, issueLines(fresh.Issues)

	back, err := d.digests.RegressedIssues(ctx, project.ID, covers, DigestListLimit)
	if err != nil {
		return digest.Project{}, err
	}
	section.RegressedIssues, section.Regressions = back.Total, issueLines(back.Issues)

	top, err := d.stats.TopIssues(ctx, project.ID, covers, DigestListLimit)
	if err != nil {
		return digest.Project{}, err
	}
	section.Top = issueLines(top)

	return section, nil
}

// total sums a project's week from the level buckets.
//
// Summed here rather than asked for as a total, because the buckets are what
// exists: the same rows draw the dashboard's chart, and adding a second
// storage shape for "the same number, pre-added" would be a cache with a
// consistency problem and no user (ADR 007's rule, applied to counts).
func (d *Digest) total(ctx context.Context, projectID int64, window domain.Range) (int64, error) {
	buckets, err := d.stats.ProjectSeries(ctx, projectID, window)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, bucket := range buckets {
		total += bucket.Count
	}
	return total, nil
}

// issueLines turns counted issues into report lines.
//
// Indexed rather than ranged by value: an IssueCount carries a whole
// domain.Issue, and copying one per iteration is a warning the linter is right
// to raise.
func issueLines(counted []ports.IssueCount) []digest.Issue {
	lines := make([]digest.Issue, 0, len(counted))
	for index := range counted {
		one := &counted[index]
		lines = append(lines, digest.Issue{
			ID:      one.Issue.ID,
			Title:   one.Issue.Title,
			Culprit: one.Issue.Culprit,
			Level:   string(one.Issue.Level),
			Count:   one.Count,
		})
	}
	return lines
}

// publicOrigin is the base URL links are built from, or empty when it points
// at this machine.
//
// An installation left on the default origin would otherwise mail out a
// report full of http://127.0.0.1 links, which is worse than a report with no
// links: the reader clicks one, gets nothing, and stops trusting the rest of
// the page. It is the same misconfiguration `doctor` already names for DSNs.
func (d *Digest) publicOrigin() string {
	if d.origin.IsLocal() {
		return ""
	}
	return d.origin.String()
}

// HasDigestChannels reports whether anything asked for the weekly report.
//
// This is the job's start condition, and the whole of it: with no channel
// asking, the job is not registered, has no goroutine and does not appear in
// the job list (ADR 005, ADR 014).
func (d *Digest) HasDigestChannels(ctx context.Context) (bool, error) {
	if d.channels == nil {
		return false, nil
	}
	channels, err := d.channels.Channels(ctx)
	if err != nil {
		return false, err
	}
	for _, channel := range channels {
		if channel.Digest {
			return true, nil
		}
	}
	return false, nil
}

// Job presents the digest as a scheduled job.
func (d *Digest) Job() DigestJob { return DigestJob{digest: d} }

// DigestJob is the weekly report seen through the scheduler's contract.
//
// It names no scheduler type: the contract is structural, which is what keeps
// the use cases from depending on the thing that runs them (ADR 014).
type DigestJob struct{ digest *Digest }

// Name identifies the job wherever it is reported.
func (j DigestJob) Name() string { return "digest" }

// Interval is how often the job asks whether the scheduled moment has passed.
func (j DigestJob) Interval() time.Duration { return DigestInterval }

// Run sends the digest if its moment has come, and does nothing otherwise.
func (j DigestJob) Run(ctx context.Context) error { return j.digest.Tick(ctx) }

// Tick is one decision: has the scheduled moment passed since the last report.
//
// Exposed so a test can drive it without a scheduler, and so the sequence is
// readable in one function rather than spread over a job and a helper.
func (d *Digest) Tick(ctx context.Context) error {
	schedule, err := d.Schedule(ctx)
	if err != nil {
		return err
	}
	now := d.clock.Now()
	due := schedule.Previous(now)

	last, configured, err := d.lastSent(ctx)
	if err != nil {
		return err
	}
	if !configured {
		// First tick of an installation that has just switched the digest
		// on. The marker is set to the period that has already passed and
		// nothing is sent: switching a subsystem on must not immediately
		// deliver a report about a week nobody was watching, which is
		// startling and, worse, is what makes somebody switch it off again.
		d.log.Info("the weekly digest is on; the first one goes out at the next scheduled time",
			"schedule", schedule.String(), "next", schedule.Next(now).Format(time.RFC3339))
		return d.recordSent(ctx, due)
	}
	if !due.After(last) {
		return nil
	}

	sent, err := d.Send(ctx, due)
	if err != nil {
		return err
	}
	d.log.Info("weekly digest sent", "channels", sent, "for", due.Format(time.RFC3339))
	return d.recordSent(ctx, due)
}

// Send builds the report for one scheduled moment and hands it to every
// channel that asked for it, returning how many took it.
//
// Hands it over rather than delivers it: the outbox owns retries, backoff and
// the record of what was sent (ADR 015). A digest that failed because a chat
// API was down for a moment would otherwise be noticed a week later, by nobody.
func (d *Digest) Send(ctx context.Context, sendAt time.Time) (int, error) {
	if d.channels == nil {
		return 0, nil
	}
	channels, err := d.channels.Channels(ctx)
	if err != nil {
		return 0, err
	}

	schedule, err := d.Schedule(ctx)
	if err != nil {
		return 0, err
	}
	covers := schedule.Covers(sendAt)
	report, err := d.build(ctx, covers, sendAt, 0)
	if err != nil {
		return 0, err
	}
	body, err := digest.Render(report)
	if err != nil {
		return 0, err
	}
	subject := fmt.Sprintf("trapline — the week of %s", covers.FirstBucket())

	// Every channel is attempted even when one fails, and the failures are
	// joined. Stopping at the first would make the delivery order decide who
	// gets a report, which is a rule nobody would be able to guess from the
	// outside.
	var failures []error
	queued := 0
	for _, channel := range channels {
		if !channel.Digest {
			continue
		}
		if err := d.channels.EnqueueDigest(ctx, channel.ID, subject, body); err != nil {
			failures = append(failures, fmt.Errorf("queueing the digest for %q: %w", channel.Name, err))
			continue
		}
		queued++
	}
	return queued, errors.Join(failures...)
}

// lastSent reads the marker of the last period reported on.
func (d *Digest) lastSent(ctx context.Context) (at time.Time, configured bool, err error) {
	raw, err := d.settings.Setting(ctx, DigestLastSentKey)
	if errors.Is(err, domain.ErrSettingNotFound) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}

	var stored lastSentSetting
	if err := json.Unmarshal(raw, &stored); err != nil {
		// Unreadable is treated as unset, which costs one skipped digest and
		// then self-corrects. The alternative — treating it as the epoch —
		// would send a report about 1970 and then one every tick.
		d.log.Warn("the digest's last-sent marker is not readable; the next scheduled digest will be skipped once",
			"stored", string(raw), "error", err)
		return time.Time{}, false, nil
	}
	return stored.At.UTC(), true, nil
}

// recordSent writes the marker.
//
// After the hand-over and not before, so a failure to queue is retried on the
// next tick rather than silently skipped. The cost of that choice is that a
// partial failure — three channels queued, the fourth refused — sends those
// three the same report twice. That is the right way round: a duplicate
// weekly mail is an annoyance, and a missing one is the feature not working.
func (d *Digest) recordSent(ctx context.Context, at time.Time) error {
	encoded, err := json.Marshal(lastSentSetting{At: at.UTC()})
	if err != nil {
		return fmt.Errorf("encoding the digest marker: %w", err)
	}
	return d.settings.SetSetting(ctx, DigestLastSentKey, encoded)
}
