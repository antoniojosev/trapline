package usecase

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/ports"
	"github.com/antoniojosev/trapline/internal/sentry"
)

// The issue bundle: everything needed to fix one bug, in one read.
//
// It exists because of what an agent costs. Assembling the same answer out of
// the REST API takes five calls — the issue, its events, its tags, its
// suspects, its history — and every one of them is a round trip the agent
// pays for twice: once in latency and once in the context window it spends
// deciding what to ask next. The bundle is the same information in one
// request, already prose, already ordered by what matters first.
//
// Markdown rather than JSON, and that is not a cosmetic choice. The consumer
// is a language model: JSON makes it re-derive the shape of the answer from
// field names on every call, while a document with headings puts the
// stacktrace where a stacktrace goes and says, in words, what a number means.
// The JSON is still there for anyone who wants it — it is `GET
// /projects/{id}/issues/{issueID}` and it has not moved.
//
// **Deterministic** is the property the golden tests exist to defend. The same
// issue must render to the same bytes, because the two consumers below both
// break otherwise: an agent's prompt cache is keyed on the text, and a diff
// between two bundles is how somebody checks whether an issue changed. Map
// iteration is the whole risk, so every map this file reads is sorted before
// it is written, and nothing here reads a clock — the windows are named by the
// caller and the times come from storage.

// bundleEventLimit is how many recent occurrences the bundle lists.
//
// Five, and only the newest one is rendered in full. The rest are one line
// each: what a reader wants from the others is whether this is still
// happening, on which release, and in which environment — not five copies of
// a stacktrace that is the same stacktrace, which is precisely what made them
// the same issue.
const bundleEventLimit = 5

// bundleFrameLimit bounds the stacktrace.
//
// A deep framework stack can run to hundreds of frames, and past the first
// few dozen it is describing the framework rather than the bug. The cut is
// announced in the output rather than silent, because a stacktrace that
// simply stops is one a reader assumes is complete.
const bundleFrameLimit = 40

// bundleCrumbLimit bounds the breadcrumbs, newest kept.
const bundleCrumbLimit = 20

// Bundle assembles one issue's whole story as a markdown document.
//
// It composes the use cases that already answer the pieces rather than
// reaching for the repositories underneath them: the bundle is a *projection*
// of the issue page, and a projection that read storage directly would be a
// second implementation of the same question, free to drift from the one the
// panel shows (ADR 006).
type Bundle struct {
	issues *Issues
	stats  *Stats
	// suspects is "which change probably caused this". Optional, like the
	// route that serves it: an assembly without it produces a bundle with no
	// suspect section rather than one that fails, because every other section
	// is still the answer somebody asked for (ADR 019).
	suspects *Suspects
	// resolution owns the release-aware columns. Optional for the same
	// reason, and read from here rather than from the issue row because they
	// are written by a different transaction from the one that records an
	// event (ADR 012).
	resolution ports.IssueResolutionRepository
}

// NewBundle wires the use case.
func NewBundle(issues *Issues, stats *Stats) *Bundle {
	return &Bundle{issues: issues, stats: stats}
}

// WithSuspects adds the commit attribution section.
func (b *Bundle) WithSuspects(suspects *Suspects) *Bundle {
	b.suspects = suspects
	return b
}

// WithResolution adds the release-aware lifecycle section.
func (b *Bundle) WithResolution(resolution ports.IssueResolutionRepository) *Bundle {
	b.resolution = resolution
	return b
}

// bundleData is everything the renderer needs, gathered.
//
// A struct between the reads and the text so the rendering can be a pure
// function of data somebody can write down in a test. Without it the golden
// tests would need a database, and a golden test that needs a database is one
// that fails for reasons that have nothing to do with the document.
type bundleData struct {
	Issue domain.Issue
	// Resolution is the release-aware lifecycle, present only when that half
	// of storage is wired.
	Resolution    ports.IssueResolution
	HasResolution bool
	// Events are the newest occurrences, newest first.
	Events []ports.StoredEvent
	Tags   map[string][]ports.TagCount
	// Last24h and Last14d come from the hourly aggregates, never from a scan
	// of the events (ADR 001, ADR 010). They are what answers "is this
	// getting worse", which the counter on the issue cannot: `times` is the
	// total since the beginning of the world.
	Last24h int64
	Last14d int64
	// Suspects is the commit attribution, present only when wired.
	Suspects    SuspectReport
	HasSuspects bool
}

// For renders one issue's bundle.
func (b *Bundle) For(ctx context.Context, projectID, issueID int64) (string, error) {
	data, err := b.gather(ctx, projectID, issueID)
	if err != nil {
		return "", err
	}
	return renderBundle(&data), nil
}

// gather does every read the document needs.
//
// The issue comes first, and a failure there is returned rather than skipped,
// so an id that belongs to another project answers "not found" instead of an
// empty document — which is what a brand-new issue would also look like.
// Everything after it is optional in the sense that its absence is a missing
// section and not a failure, and the comments below say which is which.
func (b *Bundle) gather(ctx context.Context, projectID, issueID int64) (bundleData, error) {
	detail, err := b.issues.Get(ctx, projectID, issueID, bundleEventLimit)
	if err != nil {
		return bundleData{}, err
	}

	data := bundleData{
		Issue:  detail.Issue,
		Events: detail.Events,
		Tags:   detail.Tags,
	}

	if b.resolution != nil {
		resolution, err := b.resolution.Resolution(ctx, projectID, issueID)
		if err != nil {
			return bundleData{}, err
		}
		data.Resolution, data.HasResolution = resolution, true
	}

	// The two windows, from the buckets. Both are read even though one is a
	// subset of the other: the aggregates are keyed by hour, and asking for
	// the fortnight tells you nothing about the last day unless you add up
	// twenty-four of its points — which is arithmetic this layer would be
	// doing on the client's behalf anyway.
	for window, target := range map[domain.Window]*int64{
		domain.Window24h: &data.Last24h,
		domain.Window14d: &data.Last14d,
	} {
		series, err := b.stats.IssueSeries(ctx, projectID, issueID, window)
		if err != nil {
			return bundleData{}, err
		}
		*target = series.Total
	}

	if b.suspects != nil {
		report, err := b.suspects.For(ctx, projectID, issueID)
		if err != nil {
			return bundleData{}, err
		}
		data.Suspects, data.HasSuspects = report, true
	}
	return data, nil
}

// renderBundle turns gathered data into the document.
//
// By pointer: bundleData carries five hundred bytes of issue and this walks it
// through six helpers, so copying it per section would be copying three
// kilobytes to render one document. Nothing here mutates it.
//
// A single function writing into one builder, in the order somebody reads:
// what broke, whether it is still broken, the code that broke, what led up to
// it, and what probably caused it. It is long because the document is long,
// and splitting it into a helper per heading would mean reading five
// functions to answer "what does this look like".
func renderBundle(data *bundleData) string {
	var out strings.Builder

	renderBundleHeading(&out, data)
	renderBundleFrequency(&out, data)
	renderBundleLifecycle(&out, data)

	latest, decoded := decodeLatest(data.Events)
	if decoded != nil {
		renderBundleStacktrace(&out, decoded)
		renderBundleContexts(&out, decoded)
		renderBundleBreadcrumbs(&out, decoded)
	}
	renderBundleTags(&out, data.Tags)
	renderBundleOccurrences(&out, data.Events, latest)
	renderBundleSuspects(&out, data)

	return out.String()
}

// decodeLatest reads the newest stored occurrence.
//
// A payload this build cannot read is not fatal here, unlike in the suspect
// report: the rest of the bundle is still every fact the issue row holds, and
// a document that refused to render because one blob is unreadable would
// withhold the answer over the one part of it nobody can fix anyway. The
// document says so where the stacktrace would have been.
func decodeLatest(events []ports.StoredEvent) (index int, event *sentry.Event) {
	for position := range events {
		decoded, err := sentry.DecodeEvent(events[position].Payload)
		if err != nil {
			continue
		}
		return position, &decoded
	}
	return -1, nil
}

func renderBundleHeading(out *strings.Builder, data *bundleData) {
	fmt.Fprintf(out, "# %s\n\n", oneLine(data.Issue.Title))

	// The identifiers first, as a block a machine can read without parsing
	// prose: an agent that has just been handed this document has to be able
	// to call `resolve_issue` with it, and the two numbers that call needs
	// are these.
	fmt.Fprintf(out, "- **issue**: %d\n", data.Issue.ID)
	fmt.Fprintf(out, "- **project**: %d\n", data.Issue.ProjectID)
	fmt.Fprintf(out, "- **status**: %s\n", data.Issue.Status)
	fmt.Fprintf(out, "- **level**: %s\n", data.Issue.Level)
	if data.Issue.Culprit != "" {
		fmt.Fprintf(out, "- **culprit**: `%s`\n", data.Issue.Culprit)
	}
	fmt.Fprintf(out, "- **first seen**: %s\n", stamp(data.Issue.FirstSeen))
	fmt.Fprintf(out, "- **last seen**: %s\n", stamp(data.Issue.LastSeen))
	fmt.Fprintf(out, "- **events**: %d\n", data.Issue.Times)
	// The fingerprint is last because nobody reads it until they are asking
	// why two things that look identical are two issues (ADR 003).
	fmt.Fprintf(out, "- **fingerprint**: `%s` (grouping v%d)\n",
		data.Issue.Fingerprint, data.Issue.GroupingVersion)
	out.WriteString("\n")
}

// renderBundleFrequency is the section that answers "is this getting worse".
func renderBundleFrequency(out *strings.Builder, data *bundleData) {
	out.WriteString("## Frequency\n\n")
	fmt.Fprintf(out, "- last 24 h: %d\n", data.Last24h)
	fmt.Fprintf(out, "- last 14 d: %d\n", data.Last14d)
	// Said in words rather than left as two numbers to compare, because the
	// comparison is the whole point of printing both and a reader who has to
	// do it themselves will sometimes not.
	switch {
	case data.Last14d == 0:
		out.WriteString("\nNothing in the last fortnight. The counts above come from the " +
			"hourly aggregates, which outlive the events themselves (ADR 010), " +
			"so this is silence rather than expired history.\n")
	case data.Last24h == 0:
		out.WriteString("\nNothing in the last 24 hours, so whatever was happening has stopped.\n")
	case data.Last24h*14 > data.Last14d*2:
		// More than twice the fortnight's daily average landed today.
		out.WriteString("\nToday is well above this issue's own average for the fortnight: " +
			"it is getting worse, not merely continuing.\n")
	}
	out.WriteString("\n")
}

// renderBundleLifecycle is the release-aware half: when it was declared fixed,
// against which release, and whether it came back.
func renderBundleLifecycle(out *strings.Builder, data *bundleData) {
	if !data.HasResolution {
		return
	}
	resolution := data.Resolution
	// Nothing to say when the issue has never been triaged and has never
	// carried a release. An empty heading is a reader wondering what they
	// missed.
	if resolution.FirstRelease == "" && resolution.ResolvedAt == nil &&
		resolution.Regressions == 0 && data.Issue.LastRelease == "" {
		return
	}

	out.WriteString("## Releases\n\n")
	if resolution.FirstRelease != "" {
		fmt.Fprintf(out, "- **first seen in**: `%s`\n", resolution.FirstRelease)
	}
	if data.Issue.LastRelease != "" {
		fmt.Fprintf(out, "- **last seen in**: `%s`\n", data.Issue.LastRelease)
	}
	if resolution.ResolvedAt != nil {
		fmt.Fprintf(out, "- **resolved**: %s", stamp(*resolution.ResolvedAt))
		if resolution.ResolvedInRelease != "" {
			fmt.Fprintf(out, ", against `%s`", resolution.ResolvedInRelease)
		}
		out.WriteString("\n")
	}
	if resolution.ResolveNextRelease {
		// The line that stops an agent from reporting a false regression.
		// Without it, "resolved" plus "still receiving events" reads as a fix
		// that did not work, when it is the fleet that has not been
		// redeployed yet (ADR 012).
		out.WriteString("- **waiting for the next release**: events from `" +
			resolution.ResolvedInRelease + "` and anything older are expected, " +
			"not proof the fix failed\n")
	}
	if resolution.SeenInResolvedReleaseCount > 0 {
		fmt.Fprintf(out, "- **suppressed**: %d events counted from the resolved release "+
			"without reopening it\n", resolution.SeenInResolvedReleaseCount)
	}
	if resolution.Regressions > 0 {
		fmt.Fprintf(out, "- **regressions**: %d", resolution.Regressions)
		if resolution.RegressedInRelease != "" {
			fmt.Fprintf(out, ", most recently in `%s`", resolution.RegressedInRelease)
		}
		out.WriteString("\n")
	}
	out.WriteString("\n")
}

// renderBundleStacktrace writes the frames, failing call first.
//
// Reversed from the protocol's order, which is oldest first (sentry.Stacktrace
// says so). A document read top to bottom should open on the line that raised,
// not on the process entry point: the reader is looking for the bug, and the
// bug is at the bottom of the wire format.
func renderBundleStacktrace(out *strings.Builder, event *sentry.Event) {
	if exception, found := event.PrimaryException(); found {
		out.WriteString("## Exception\n\n")
		fmt.Fprintf(out, "```\n%s\n```\n\n", oneLine(event.Title()))
		if exception.Module != "" {
			fmt.Fprintf(out, "Raised from module `%s`.\n\n", exception.Module)
		}
	} else if event.Message != "" {
		out.WriteString("## Message\n\n")
		fmt.Fprintf(out, "```\n%s\n```\n\n", oneLine(event.Message))
	}

	stacktrace := event.BestStacktrace()
	if stacktrace == nil || len(stacktrace.Frames) == 0 {
		return
	}

	out.WriteString("## Stacktrace\n\n")
	out.WriteString("Failing call first.\n\n")

	frames := stacktrace.Frames
	written := 0
	for index := len(frames) - 1; index >= 0; index-- {
		if written == bundleFrameLimit {
			fmt.Fprintf(out, "_… %d more frames, omitted._\n\n", index+1)
			break
		}
		renderFrame(out, frames[index], written+1)
		written++
	}
}

// renderFrame writes one frame with whatever source it carries.
//
// Numbered from one, with #1 the call that raised. It is the numbering the
// suspect report already uses in its reasons ("frame #1"), and two clients of
// the same answer counting from different places is the kind of detail that
// costs somebody an afternoon (ADR 006, ADR 019).
func renderFrame(out *strings.Builder, frame sentry.Frame, depth int) {
	marker := "   "
	if frame.InApp {
		// The application's own code, which is the only code the reader can
		// change. Marked rather than filtered: a library frame is often what
		// says *which* call into the library went wrong.
		marker = "-> "
	}
	location := frame.Path()
	if location == "" {
		location = "<unknown>"
	}
	fmt.Fprintf(out, "%s#%d `%s`", marker, depth, location)
	if frame.Lineno > 0 {
		fmt.Fprintf(out, ":%d", frame.Lineno)
		if frame.Colno > 0 {
			fmt.Fprintf(out, ":%d", frame.Colno)
		}
	}
	if frame.Function != "" {
		fmt.Fprintf(out, " in `%s`", frame.Function)
	}
	out.WriteString("\n")

	// What the minified frame was, when symbolication resolved this one. It
	// is the line that lets an agent believe the path above: without it, a
	// resolved frame naming `src/checkout.ts` in a production bundle looks
	// like the server guessed (ADR 018).
	if raw := rawFramePath(frame.Raw); raw != "" {
		fmt.Fprintf(out, "      _symbolicated from_ `%s`", raw)
		if frame.Raw.Lineno > 0 {
			fmt.Fprintf(out, ":%d:%d", frame.Raw.Lineno, frame.Raw.Colno)
		}
		out.WriteString("\n")
	}

	if frame.ContextLine == "" && len(frame.PreContext) == 0 && len(frame.PostContext) == 0 {
		out.WriteString("\n")
		return
	}
	out.WriteString("\n```\n")
	first := frame.Lineno - len(frame.PreContext)
	for offset, line := range frame.PreContext {
		writeSourceLine(out, first+offset, " ", line)
	}
	if frame.ContextLine != "" {
		writeSourceLine(out, frame.Lineno, ">", frame.ContextLine)
	}
	for offset, line := range frame.PostContext {
		writeSourceLine(out, frame.Lineno+1+offset, " ", line)
	}
	out.WriteString("```\n\n")
}

// rawFramePath is the minified location a frame was resolved from, if any.
//
// It mirrors sentry.Frame.Path — absolute path first, filename second — and
// lives here rather than as a method on the protocol type because this is the
// only caller, and `internal/sentry` is a package another session is fuzzing
// this week.
func rawFramePath(raw *sentry.RawFrame) string {
	switch {
	case raw == nil:
		return ""
	case raw.AbsPath != "":
		return raw.AbsPath
	default:
		return raw.Filename
	}
}

// writeSourceLine writes one numbered line of source.
//
// The line number is printed even when it is meaningless — a frame with no
// `lineno` puts the context at line 0 — because the alternative is deciding
// per frame whether to print it, and a stacktrace whose gutter comes and goes
// is harder to read than one with an obviously wrong number in it.
func writeSourceLine(out *strings.Builder, number int, marker, text string) {
	fmt.Fprintf(out, "%s %4d | %s\n", marker, number, strings.TrimRight(text, "\r\n"))
}

// renderBundleContexts writes the structured context an SDK attached.
func renderBundleContexts(out *strings.Builder, event *sentry.Event) {
	sections := []struct {
		heading string
		values  map[string]any
	}{
		{"User", event.User},
		{"Request", event.Request},
		{"Contexts", event.Contexts},
		{"Extra", event.Extra},
	}
	var written bool
	for _, section := range sections {
		if len(section.values) == 0 {
			continue
		}
		if !written {
			out.WriteString("## Context\n\n")
			written = true
		}
		fmt.Fprintf(out, "### %s\n\n", section.heading)
		writeFlatMap(out, section.values)
		out.WriteString("\n")
	}
}

// writeFlatMap writes a map as sorted `key: value` lines.
//
// Sorted because this is the one place a Go map reaches the output, and map
// iteration order is the single thing standing between this document and the
// determinism its golden tests assert. One level deep, with nested values
// rendered as JSON-ish text by fmt: the alternative is a recursive pretty
// printer for arbitrary SDK payloads, and the caller who needs that has the
// raw event on the JSON endpoint.
func writeFlatMap(out *strings.Builder, values map[string]any) {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(out, "- `%s`: %s\n", key, oneLine(renderValue(values[key])))
	}
}

// renderValue turns one context value into a line.
func renderValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		// JSON numbers arrive as float64. Whole ones are printed without the
		// decimal point, because `"status_code": 500.000000` reads as a bug
		// in this product rather than as a status code.
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'g', -1, 64)
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, key+"="+renderValue(typed[key]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, renderValue(item))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		return fmt.Sprintf("%v", typed)
	}
}

// renderBundleBreadcrumbs writes what the program did before it failed.
func renderBundleBreadcrumbs(out *strings.Builder, event *sentry.Event) {
	crumbs := event.Breadcrumbs
	if len(crumbs) == 0 {
		return
	}
	out.WriteString("## Breadcrumbs\n\n")
	// Oldest first, which is the order they happened in and the order a
	// reader reconstructs a sequence from. When there are more than the
	// limit, the *newest* are kept: the ones nearest the failure are the ones
	// that explain it.
	if len(crumbs) > bundleCrumbLimit {
		fmt.Fprintf(out, "_The %d oldest are omitted; the %d nearest the failure follow._\n\n",
			len(crumbs)-bundleCrumbLimit, bundleCrumbLimit)
		crumbs = crumbs[len(crumbs)-bundleCrumbLimit:]
	}
	for index := range crumbs {
		crumb := &crumbs[index]
		fmt.Fprintf(out, "- `%s`", stamp(crumb.Timestamp))
		if crumb.Category != "" {
			fmt.Fprintf(out, " [%s]", crumb.Category)
		}
		if crumb.Level != "" {
			fmt.Fprintf(out, " (%s)", crumb.Level)
		}
		if crumb.Message != "" {
			fmt.Fprintf(out, " %s", oneLine(crumb.Message))
		}
		if len(crumb.Data) > 0 {
			fmt.Fprintf(out, " — %s", oneLine(renderValue(crumb.Data)))
		}
		out.WriteString("\n")
	}
	out.WriteString("\n")
}

// renderBundleTags writes the aggregated tags, most common value first.
func renderBundleTags(out *strings.Builder, tags map[string][]ports.TagCount) {
	if len(tags) == 0 {
		return
	}
	out.WriteString("## Tags\n\n")
	keys := make([]string, 0, len(tags))
	for key := range tags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		values := tags[key]
		if len(values) == 0 {
			continue
		}
		parts := make([]string, 0, len(values))
		for _, value := range values {
			parts = append(parts, fmt.Sprintf("`%s` (%d)", value.Value, value.Count))
		}
		fmt.Fprintf(out, "- **%s**: %s\n", key, strings.Join(parts, ", "))
	}
	out.WriteString("\n")
}

// renderBundleOccurrences lists the recent events, newest first.
func renderBundleOccurrences(out *strings.Builder, events []ports.StoredEvent, rendered int) {
	if len(events) == 0 {
		out.WriteString("## Recent occurrences\n\nNone stored. The counts above come from " +
			"the hourly aggregates, which outlive the payloads (ADR 010), so this issue " +
			"may well have happened — its events have simply passed their retention " +
			"window.\n\n")
		return
	}
	out.WriteString("## Recent occurrences\n\n")
	for index := range events {
		event := &events[index]
		fmt.Fprintf(out, "- %s  `%s`", stamp(event.OccurredAt), event.EventID)
		if event.Release != "" {
			fmt.Fprintf(out, "  release `%s`", event.Release)
		}
		if event.Environment != "" {
			fmt.Fprintf(out, "  env `%s`", event.Environment)
		}
		if index == rendered {
			// Which of them the sections above describe. Without it a reader
			// comparing the stacktrace against this list has no way to know
			// which row it came from, and the answer is not always the first
			// one — a payload that cannot be decoded is skipped.
			out.WriteString("  ← the occurrence above")
		}
		out.WriteString("\n")
	}
	if rendered < 0 {
		out.WriteString("\n_None of the stored payloads could be decoded by this build, " +
			"so there is no stacktrace above._\n")
	}
	out.WriteString("\n")
}

// renderBundleSuspects writes the commit attribution, or why there is none.
func renderBundleSuspects(out *strings.Builder, data *bundleData) {
	if !data.HasSuspects {
		return
	}
	report := data.Suspects
	out.WriteString("## Suspect commits\n\n")
	if report.Release != "" {
		fmt.Fprintf(out, "Considering the %d commits of `%s`, the release this issue was "+
			"first seen in (ADR 019).\n\n", report.CommitCount, report.Release)
	}

	for index := range report.Suspects {
		suspect := &report.Suspects[index]
		fmt.Fprintf(out, "%d. `%s` — %s\n", index+1,
			shortSHA(suspect.Commit.SHA), subject(suspect.Commit.Message))
		if suspect.Commit.AuthorName != "" {
			fmt.Fprintf(out, "   by %s\n", suspect.Commit.AuthorName)
		}
		for _, reason := range suspect.Reasons {
			fmt.Fprintf(out, "   touched `%s` (%s), which is frame #%d\n",
				reason.Path, reason.ChangeType, reason.FrameDepth+1)
		}
		out.WriteString("\n")
	}

	if len(report.Suspects) == 0 {
		for index := range report.Commits {
			commit := &report.Commits[index]
			fmt.Fprintf(out, "- `%s` — %s\n",
				shortSHA(commit.SHA), subject(commit.Message))
		}
		if count := report.CommitCount - len(report.Commits); count > 0 {
			fmt.Fprintf(out, "- _… and %d more_\n", count)
		}
		if len(report.Commits) > 0 {
			out.WriteString("\n")
		}
	}

	if report.Warning != "" {
		// The warning is the answer when there are no suspects, and it always
		// names what to do about it. Printing it as prose rather than hiding
		// it behind an empty list is the difference between "no idea" and "do
		// this and I will have one" (ADR 019).
		fmt.Fprintf(out, "%s\n\n", report.Warning)
	}
}

// subject is a commit message's first line, which is what a list wants: the
// body is the reasoning, and reproducing it here would bury the seven commits
// a reader is scanning under one author's paragraph.
func subject(message string) string {
	if cut := strings.IndexByte(message, '\n'); cut >= 0 {
		message = message[:cut]
	}
	return oneLine(message)
}

// shortSHA is what somebody actually types into `git show`.
func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// stamp formats a time the one way this document ever formats one.
//
// UTC and RFC 3339, with no local timezone anywhere: the reader is a program
// running somewhere this server knows nothing about, and a document whose
// timestamps depend on the renderer's machine is one whose golden test passes
// only in the timezone it was written in.
func stamp(when time.Time) string {
	if when.IsZero() {
		return "unknown"
	}
	return when.UTC().Format(time.RFC3339)
}

// oneLine collapses a value that must not break the layout.
//
// A newline in an exception message or a tag value would silently end a list
// item and turn the rest of the string into a paragraph of its own, which is
// how a crafted payload gets to forge a heading in a document an agent is
// about to act on.
func oneLine(text string) string {
	replaced := strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(text)
	return strings.TrimSpace(replaced)
}
