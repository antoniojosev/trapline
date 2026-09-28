package usecase

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
	"github.com/antoniojosev/trapline/internal/engine"
	"github.com/antoniojosev/trapline/internal/envelope"
	"github.com/antoniojosev/trapline/internal/ports"
	"github.com/antoniojosev/trapline/internal/sentry"
)

// Ingest turns an envelope into stored issues.
//
// This is the product's hot path and its most exposed one, so the order of
// operations is deliberate: authenticate, then decide whether the category is
// even wanted, then parse, then scrub, and only then persist. Every step that
// can reject cheaply runs before every step that costs something.
type Ingest struct {
	issues   ports.IssueRepository
	limiter  ports.RateLimiter
	scrubber *domain.Scrubber
	clock    ports.Clock
	debug    bool
	// feed is who to tell when an issue appears, comes back or happens
	// again. Optional, and nil-safe on the other side: an assembly without a
	// feed ingests exactly as it did before one existed.
	feed *Broadcaster
	// alerts is the two detectors this path drives: the error rate, counted
	// before the limiter, and the per-issue spike, judged after the event is
	// recorded. Optional in the same way — nil means an installation with no
	// alerting, which ingests exactly as it did before alerting existed.
	//
	// What is NOT here is the emission of new_issue and regression. Those are
	// written by the transaction that records the event, inside the
	// repository, because they have to be committed together with the fact
	// they are about (ADR 015). Doing it here would put a second transaction
	// between "the issue exists" and "somebody was told", and a restart in
	// that gap loses the notification.
	alerts *Alerts
	// crons is the cron-monitoring subsystem. Optional, and its absence is
	// what the check_in branch below falls back to: an assembly without it
	// accepts check-ins and counts them as not stored, exactly as every
	// build without crons did.
	crons *Crons
	// transactions is the tracing subsystem. Optional in the same way, and
	// its absence is what the transaction branch below falls back to: an
	// assembly without it accepts transactions and counts them as not stored,
	// exactly as every build without tracing did.
	transactions *Transactions
	// symbolicator turns minified JavaScript frames back into source, on the
	// way in. Optional in the same way as the rest, and its absence is exactly
	// what every build without source maps did: store the frame the SDK sent.
	symbolicator *Symbolicator

	// health is release health. Optional in the same way, and its absence is
	// what the session branch below falls back to: an assembly without it
	// accepts sessions and counts them as not stored, exactly as every build
	// without release health did.
	health *Health
}

// NewIngest wires the use case.
func NewIngest(
	issues ports.IssueRepository,
	limiter ports.RateLimiter,
	scrubber *domain.Scrubber,
	clock ports.Clock,
	debug bool,
) *Ingest {
	return &Ingest{issues: issues, limiter: limiter, scrubber: scrubber, clock: clock, debug: debug}
}

// WithAlerts turns on the two detectors that run on the ingest path.
//
// A setter for the same reason WithFeed is one: the hot path has collected
// four optional collaborators now, and each of them arriving as another
// constructor argument would rewrite every call site again.
func (i *Ingest) WithAlerts(alerts *Alerts) *Ingest {
	i.alerts = alerts
	return i
}

// WithCrons turns on cron monitoring.
//
// A setter for the same reason the others are: the hot path has collected
// several optional collaborators, and each arriving as another constructor
// argument would rewrite every call site again.
func (i *Ingest) WithCrons(crons *Crons) *Ingest {
	i.crons = crons
	return i
}

// WithTransactions turns on tracing.
//
// A setter for the same reason the others are, and with the same consequence:
// a build assembled without it goes on counting transactions as not stored
// rather than failing, which is what keeps ADR 005's promise that a subsystem
// nobody switched on costs nothing.
func (i *Ingest) WithTransactions(transactions *Transactions) *Ingest {
	i.transactions = transactions
	return i
}

// WithSourceMaps turns on symbolication.
//
// A setter for the same reason the others are, and with the same consequence:
// a build assembled without it stores the minified frame, which is what every
// build without source maps did.
func (i *Ingest) WithSourceMaps(symbolicator *Symbolicator) *Ingest {
	i.symbolicator = symbolicator
	return i
}

// WithHealth turns on release health.
//
// A setter for the same reason the others are, and with the same consequence:
// a build assembled without it goes on counting sessions as not stored rather
// than failing, which is what keeps ADR 005's promise that a subsystem nobody
// switched on costs nothing.
func (i *Ingest) WithHealth(health *Health) *Ingest {
	i.health = health
	return i
}

// WithFeed turns on the live event stream.
//
// A setter rather than an eighth constructor argument, for the same reason
// the server has them: adding the next optional collaborator should not
// rewrite every call site again.
func (i *Ingest) WithFeed(feed *Broadcaster) *Ingest {
	i.feed = feed
	return i
}

// ClientInfo is what the request itself said about the caller, as distinct
// from what the payload claimed.
//
// It exists because a browser reports one thing the SDK running inside it
// cannot: which browser it is. The page never sees a reliable name for itself,
// and the payload is written by the SDK, but the User-Agent is written by the
// browser on every request. Keeping it in its own type rather than passing a
// bare string leaves room for the other facts that only the request knows —
// the client address, for one — without changing this signature again.
type ClientInfo struct {
	UserAgent string
	// Baggage is the W3C `baggage` header, which is where the official SDKs
	// put the dynamic sampling context — including the rate the SDK itself
	// already applied. Read here rather than guessed, because sampling
	// composes: an SDK sending one in four and a server keeping one in ten
	// together keep one in forty, and a panel that showed only the server's
	// half would be off by the SDK's factor (ADR 021).
	Baggage string
}

// IngestResult reports what an envelope produced.
type IngestResult struct {
	Accepted  int
	NewIssues int
	// Regressions are resolved issues this envelope reopened.
	Regressions []domain.Issue
	// Dropped counts items that were parsed but not stored, by reason. It
	// exists so `--debug` can answer "why did my event not show up?", which is
	// otherwise the single most expensive question a user can ask.
	Dropped map[string]int
	// RateLimited names the categories that were refused, so the caller can
	// tell the SDK to stop sending them (ADR 005).
	RateLimited []engine.Category
	// NewMonitors counts cron monitors this envelope brought into existence,
	// which is what an SDK declaring one with its first check-in does.
	NewMonitors int
	// Transactions counts the transactions this envelope aggregated, and
	// SampledTraces how many of them also had their raw spans stored. The two
	// are reported apart because they are the two halves of ADR 021: the
	// first is what the percentiles were computed over — always everything
	// received — and the second is how many waterfalls somebody will be able
	// to open.
	Transactions  int
	SampledTraces int
	// Sessions counts the sessions this envelope accounted for — one per
	// `session` item, and the whole of an aggregate for a `sessions` one. It
	// is a count of sessions and not of items on purpose: those two numbers
	// differ by three orders of magnitude for an SDK in aggregate mode, and
	// the one somebody debugging with --debug wants is this one.
	Sessions int64
}

// Process reads an envelope and stores what it contains.
//
// Unknown item types are skipped, never a reason to reject the envelope: an
// SDK that starts sending something new must not break an installation that
// has not been upgraded (ADR 002).
func (i *Ingest) Process(
	ctx context.Context, projectID int64, body io.Reader, limits envelope.Limits, client ClientInfo,
) (IngestResult, error) {
	parsed, err := envelope.Parse(body, limits)
	if err != nil {
		return IngestResult{}, err
	}

	result := IngestResult{Dropped: map[string]int{}}
	limited := map[engine.Category]bool{}

	for index := range parsed.Items {
		item := &parsed.Items[index]
		category, known := categoryFor(item.Type)
		if !known {
			result.Dropped["unknown_type:"+item.Type]++
			i.logDrop(projectID, item.Type, "the item type is not one this build stores")
			continue
		}

		// Counted before the limiter is asked, and this order is a decision
		// rather than an accident: the spike signal has to be measured on
		// what arrived, not on what survived. Counting after the limiter
		// would mean a flood large enough to be refused would silence the
		// alert about itself, at exactly the moment the alert is the point
		// (ADR 005, ADR 015).
		if i.alerts != nil && category == engine.CategoryError {
			i.alerts.ObserveEvent(ctx, projectID)
		}

		// The limiter is consulted before any parsing: refusing an item is
		// meant to be cheaper than accepting one, or a flood would cost the
		// same either way and the limit would protect nothing.
		allowed, err := i.limiter.Allow(ctx, projectID, category)
		if err != nil {
			return IngestResult{}, err
		}
		if !allowed {
			result.Dropped["rate_limited:"+string(category)]++
			if !limited[category] {
				limited[category] = true
				result.RateLimited = append(result.RateLimited, category)
			}
			i.logDrop(projectID, item.Type, "the category is off or over its limit")
			continue
		}

		if category == engine.CategoryCheckIn && i.crons != nil {
			// A check-in is not an event and does not become an issue: it
			// moves a monitor's state and may create the monitor outright
			// (ADR 016). It goes through the same limiter and the same
			// category switch as everything else, so an installation that
			// has not enabled check_in pays nothing for the feature (ADR 005).
			created, err := i.storeCheckIn(ctx, projectID, item)
			if err != nil {
				result.Dropped["invalid_check_in"]++
				i.logDrop(projectID, item.Type, err.Error())
				continue
			}
			result.Accepted++
			if created {
				result.NewMonitors++
			}
			continue
		}

		if category == engine.CategoryTransaction && i.transactions != nil {
			// A transaction is not an event and does not become an issue: it
			// is one sample of a latency distribution, and what is kept of it
			// is a count, a failure flag and one value folded into a
			// mergeable histogram (ADR 007, ADR 020). Its raw spans are kept
			// only if sampling says so, and that decision changes nothing
			// about the aggregate.
			receipt, err := i.transactions.Accept(ctx, projectID, item.Payload,
				sdkSampleRate(&parsed.Header, client))
			if err != nil {
				result.Dropped["invalid_transaction"]++
				i.logDrop(projectID, item.Type, err.Error())
				continue
			}
			result.Accepted++
			result.Transactions++
			if receipt.Sampled {
				result.SampledTraces++
			}
			continue
		}

		if category == engine.CategorySession && i.health != nil {
			// A session is not an event and never becomes an issue, and it
			// never becomes a row of its own either: it moves four counters
			// in a bounded in-memory window that only ever writes down
			// totals per (release, environment, hour). Never a row per
			// session is the rule that defines this subsystem, because a
			// product that stores sessions individually is a different
			// product — and it is where the competitor spends its
			// infrastructure (ADR 008, ADR 021).
			receipt, err := i.health.Accept(ctx, projectID, item.Type, item.Payload)
			if err != nil {
				result.Dropped["invalid_session"]++
				i.logDrop(projectID, item.Type, err.Error())
				continue
			}
			result.Accepted++
			result.Sessions += receipt.Sessions
			continue
		}

		if category != engine.CategoryError {
			// What is left is accepted by the protocol and stored in a later
			// phase, or in this build only when its subsystem was assembled.
			// Counting it here rather than silently discarding it is what
			// makes the gap visible.
			result.Dropped["not_yet_stored:"+string(category)]++
			continue
		}

		stored, err := i.storeEvent(ctx, projectID, &parsed.Header, item, client)
		if err != nil {
			// One bad item must not cost the rest of the envelope. An SDK
			// batches several events into one request, and rejecting all of
			// them because one was malformed would lose reports that were
			// perfectly fine.
			result.Dropped["invalid_event"]++
			i.logDrop(projectID, item.Type, err.Error())
			continue
		}

		result.Accepted++
		if stored.New {
			result.NewIssues++
		}
		if stored.Regressed {
			result.Regressions = append(result.Regressions, stored.Issue)
		}
		// Published after the event is stored, never before: a panel told
		// about an issue it cannot then read would be showing something that
		// does not exist yet, which is a worse failure than being told late.
		i.feed.Publish(projectID, &IssueEvent{Kind: issueEventKind(&stored), Issue: stored.Issue})
	}
	return result, nil
}

// storeCheckIn files a cron check-in, and reports whether it created the
// monitor it is about.
//
// The unit conversions live here rather than in the decoder: the protocol
// sends a duration in seconds and its two margins in minutes, and a decoder
// that quietly normalised them would be the kind of unit error that shows up
// only as a monitor reporting missed sixty times too early.
func (i *Ingest) storeCheckIn(ctx context.Context, projectID int64, item *envelope.Item) (bool, error) {
	decoded, err := sentry.DecodeCheckIn(item.Payload)
	if err != nil {
		return false, err
	}

	report := Report{
		Slug:        decoded.MonitorSlug,
		CheckInID:   decoded.CheckInID,
		Status:      domain.CheckInStatus(decoded.Status),
		Environment: decoded.Environment,
	}
	if decoded.HasDuration && decoded.DurationSeconds > 0 {
		report.Duration = time.Duration(decoded.DurationSeconds * float64(time.Second))
	}
	if config := decoded.Config; config != nil {
		spec := &MonitorSpec{
			ScheduleType: config.ScheduleType,
			Schedule:     config.Crontab,
			Timezone:     config.Timezone,
		}
		if config.ScheduleType == string(domain.ScheduleInterval) {
			spec.Schedule = strconv.Itoa(config.IntervalValue) + " " + config.IntervalUnit
		}
		if config.CheckinMarginMinutes != nil {
			spec.CheckinMargin = time.Duration(*config.CheckinMarginMinutes) * time.Minute
		}
		if config.MaxRuntimeMinutes != nil {
			spec.MaxRuntime = time.Duration(*config.MaxRuntimeMinutes) * time.Minute
		}
		report.Declare = spec
	}

	result, err := i.crons.Accept(ctx, projectID, report)
	if err != nil {
		return false, err
	}
	return result.Created, nil
}

// sdkSampleRate reads the rate the SDK already applied, from either of the two
// places it appears.
//
// The envelope header's `trace` object is where the official SDKs put the
// dynamic sampling context; the `baggage` HTTP header is where a proxy that
// continued a trace puts the same fact. A server that read only one of them
// would see the SDK's rate for some clients and not for others, which is a
// worse outcome than seeing it for none: the effective rate would be right on
// some rows of a panel and wrong on others, with nothing to say which.
//
// Zero means the SDK did not say, and then the server's knob is the whole
// story.
func sdkSampleRate(header *envelope.Header, client ClientInfo) float64 {
	if rate, found := sentry.SampleRateFromTrace(header.Extra["trace"]); found {
		return rate
	}
	if rate, found := sentry.SampleRateFromBaggage(client.Baggage); found {
		return rate
	}
	return 0
}

// issueEventKind names what an event did to its issue, in the order that
// matters to a reader: a regression outranks everything, because an issue
// coming back is news and another occurrence of it is not.
func issueEventKind(stored *ports.RecordEventResult) IssueEventKind {
	switch {
	case stored.Regressed:
		return IssueEventRegressed
	case stored.New:
		return IssueEventNew
	default:
		return IssueEventUpdated
	}
}

func (i *Ingest) storeEvent(
	ctx context.Context, projectID int64, header *envelope.Header, item *envelope.Item, client ClientInfo,
) (ports.RecordEventResult, error) {
	event, err := sentry.DecodeEvent(item.Payload)
	if err != nil {
		return ports.RecordEventResult{}, err
	}

	addBrowserContext(&event, client.UserAgent)

	// Symbolication runs here: after the payload is decoded, and before
	// anything reads the frames.
	//
	// Before grouping, because the fingerprint has to be computed over the
	// frames a human will see. Resolving afterwards would mean the panel shows
	// `checkout.js` while identity was taken from `bundle.min.js`, so the same
	// bug rebuilt with a different minifier would be a different issue — which
	// is the failure source maps exist to remove.
	//
	// And before scrubbing, because scrubbing is the last thing that touches
	// the payload before it is written down (SECURITY.md). A source map embeds
	// original source, and a context line lifted out of it goes through the
	// scrubber like everything else rather than around it.
	//
	// The consequence is written down in ADR 018 rather than left implicit:
	// grouping now reads the symbolicated frame, so switching source maps on
	// can split one minified issue into a before and an after. It does not
	// change GroupingVersion, because a fingerprint is only ever computed at
	// ingest and no stored issue is regrouped (ADR 003).
	if resolved := i.symbolicator.Apply(ctx, projectID, &event); resolved > 0 && i.debug {
		slog.Debug("symbolicated an event",
			"project_id", projectID, "frames", resolved)
	}

	// Scrubbing happens here, before anything is written down. Doing it at
	// display time would mean the secret is already in the database, in the
	// backup and in whatever an operator greps — at which point it has leaked
	// and a redaction on the way out is theatre (SECURITY.md).
	scrubbed := i.scrub(&event)

	eventID := event.EventID
	if eventID == "" {
		eventID = header.EventID
	}

	stored, err := i.issues.RecordEvent(ctx, &ports.RecordEventInput{
		ProjectID:   projectID,
		Fingerprint: domain.Fingerprint(groupingInputFor(&event)),
		Observation: domain.Observation{
			At:      event.Timestamp,
			Level:   domain.Level(event.Level),
			Title:   event.Title(),
			Culprit: culpritFor(&event),
			Release: event.Release,
		},
		EventID:     eventID,
		ReceivedAt:  i.clock.Now(),
		Environment: event.Environment,
		Message:     i.scrubber.ScrubString(event.Message),
		Tags:        tagsFor(&event),
		Payload:     scrubbed,
	})
	if err != nil {
		return ports.RecordEventResult{}, err
	}

	// After the event is recorded, because the hour bucket a spike is judged
	// against was written by the transaction that just committed: asking
	// before it would compare an hour missing its newest event against a
	// baseline that is not missing anything (ADR 010, ADR 015).
	if i.alerts != nil {
		i.alerts.DetectSpike(ctx, &stored.Issue, event.Environment)
	}
	return stored, nil
}

// scrub redacts the payload's sensitive sections and re-encodes it.
//
// Only the sections that carry user data are walked — request, user, extra,
// contexts and breadcrumb data. Walking the whole document would also rewrite
// stack frames and exception values, which is both wasteful and how a
// stacktrace ends up full of [redacted] where the useful context was.
func (i *Ingest) scrub(event *sentry.Event) []byte {
	event.Request = i.scrubber.ScrubMap(event.Request)
	event.User = i.scrubber.ScrubMap(event.User)
	event.Extra = i.scrubber.ScrubMap(event.Extra)
	event.Contexts = i.scrubber.ScrubMap(event.Contexts)
	for index := range event.Breadcrumbs {
		event.Breadcrumbs[index].Data = i.scrubber.ScrubMap(event.Breadcrumbs[index].Data)
		event.Breadcrumbs[index].Message = i.scrubber.ScrubString(event.Breadcrumbs[index].Message)
	}

	encoded, err := sentry.EncodeEvent(event)
	if err != nil {
		// Re-encoding a document we just decoded should not fail. If it
		// somehow does, storing the unscrubbed original is not an option:
		// better a stored event with no payload than one with a live
		// credential in it.
		slog.Error("re-encoding a scrubbed event", "error", err)
		return []byte("{}")
	}
	return encoded
}

// addBrowserContext records which browser sent the event, when the payload did
// not already say.
//
// The browser SDK sends no browser context of its own — the page has no
// trustworthy name for itself — so without this an issue from a browser says
// nothing about where it happened, and "only on Safari", which is the answer
// to a large share of front-end bugs, is unanswerable. The User-Agent header
// is the one place the browser itself speaks.
//
// A context the SDK sent always wins. Someone who set it deliberately knows
// something about their application that a header cannot say.
func addBrowserContext(event *sentry.Event, userAgent string) {
	if userAgent == "" {
		return
	}
	if _, already := event.Contexts["browser"]; already {
		return
	}
	browser, found := domain.BrowserFromUserAgent(userAgent)
	if !found {
		return
	}
	if event.Contexts == nil {
		event.Contexts = map[string]any{}
	}
	recorded := map[string]any{"name": browser.Name}
	if browser.Version != "" {
		recorded["version"] = browser.Version
	}
	event.Contexts["browser"] = recorded
}

// tagsFor is the event's own tags plus the fields worth filtering by.
//
// Environment, release, level and server name arrive as top-level fields
// rather than tags, but they are exactly what someone filters a list by:
// "what is broken in production" is the first question during an incident.
// Promoting them here means one filtering mechanism covers both, instead of a
// special case per field, and it is what Sentry's own SDKs do.
//
// The event's own tags win a collision: if someone set a tag called
// "environment" deliberately, they meant it.
func tagsFor(event *sentry.Event) map[string]string {
	promoted := map[string]string{
		"environment": event.Environment,
		"release":     event.Release,
		"level":       string(event.Level),
		"server_name": event.ServerName,
	}

	tags := make(map[string]string, len(event.Tags)+len(promoted)+2)
	for key, value := range promoted {
		if value != "" {
			tags[key] = value
		}
	}
	for key, value := range browserTags(event.Contexts) {
		tags[key] = value
	}
	for key, value := range event.Tags {
		tags[key] = value
	}
	return tags
}

// browserTags promotes the browser context into the two tags people filter by.
//
// The context was already being recorded — addBrowserContext writes it from
// the User-Agent, which is the only place a browser names itself — but a
// context is not a tag, and only tags are filterable. So "does this only
// happen in Safari?", which is the answer to a large share of front-end bugs,
// could be read one event at a time and never asked of the list. Two tags fix
// that, and they are named the way Sentry names them so a query someone
// already knows keeps working:
//
//	browser.name  Safari          — the facet worth grouping by
//	browser       Safari 17.4     — the exact build, for when a version matters
//
// Only strings are promoted. A context is arbitrary JSON from an SDK this
// product does not control, and a tag whose value is "map[]" is worse than an
// absent one.
func browserTags(contexts map[string]any) map[string]string {
	browser, found := contexts["browser"].(map[string]any)
	if !found {
		return nil
	}
	name, _ := browser["name"].(string)
	if name == "" {
		// Without a name there is nothing to group by, and a bare version
		// number names no browser at all.
		return nil
	}

	tags := map[string]string{"browser.name": name}
	if version, _ := browser["version"].(string); version != "" {
		tags["browser"] = name + " " + version
	} else {
		tags["browser"] = name
	}
	return tags
}

// groupingInputFor maps a protocol event onto the domain's grouping input.
//
// The mapping lives here rather than in the domain because the domain must not
// know the wire format, and it does not live in the HTTP adapter because it is
// a rule about meaning, not about transport.
func groupingInputFor(event *sentry.Event) domain.GroupingInput {
	input := domain.GroupingInput{
		CustomFingerprint: event.Fingerprint,
		// The template, when the SDK sent one: a log statement is one issue
		// however many different values it has been called with.
		Message: firstNonEmpty(event.MessageTemplate, event.Message),
	}
	if exception, found := event.PrimaryException(); found {
		input.ExceptionType = exception.Type
		input.ExceptionValue = exception.Value
	}
	if stacktrace := event.BestStacktrace(); stacktrace != nil {
		// Indexed rather than ranged by value: a frame carries its context
		// lines, so copying one per iteration is expensive, and a stacktrace
		// is walked once per ingested event.
		input.Frames = make([]domain.Frame, 0, len(stacktrace.Frames))
		for index := range stacktrace.Frames {
			frame := &stacktrace.Frames[index]
			input.Frames = append(input.Frames, domain.Frame{
				Module:   frame.Module,
				Function: frame.Function,
				File:     frame.Path(),
				InApp:    frame.InApp,
			})
		}
	}
	return input
}

// firstNonEmpty returns the first value that is not empty.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// culpritFor names where an error happened, for the issue list.
//
// The last in-app frame, because frames arrive oldest first and the innermost
// frame the user wrote is the one they need to look at. Falling back to the
// transaction name keeps something useful for events with no stacktrace.
func culpritFor(event *sentry.Event) string {
	if stacktrace := event.BestStacktrace(); stacktrace != nil {
		for index := len(stacktrace.Frames) - 1; index >= 0; index-- {
			if !stacktrace.Frames[index].InApp {
				continue
			}
			return renderCulprit(&stacktrace.Frames[index])
		}
		if len(stacktrace.Frames) > 0 {
			return renderCulprit(&stacktrace.Frames[len(stacktrace.Frames)-1])
		}
	}
	return event.Transaction
}

func renderCulprit(frame *sentry.Frame) string {
	location := frame.Module
	if location == "" {
		location = frame.Path()
	}
	if frame.Function == "" {
		return location
	}
	if location == "" {
		return frame.Function
	}
	return location + " in " + frame.Function
}

// categoryFor maps a protocol item type onto an engine category.
func categoryFor(itemType string) (engine.Category, bool) {
	switch itemType {
	case "event":
		return engine.CategoryError, true
	case "transaction":
		return engine.CategoryTransaction, true
	case "session", "sessions":
		return engine.CategorySession, true
	case "check_in":
		return engine.CategoryCheckIn, true
	default:
		return "", false
	}
}

// logDrop explains a discarded item when --debug is on.
//
// "Why did my event not show up?" is the most expensive question a user can
// ask, because without this the answer requires someone else's help.
func (i *Ingest) logDrop(projectID int64, itemType, reason string) {
	if !i.debug {
		return
	}
	slog.Debug("dropped an envelope item",
		"project_id", projectID,
		"item_type", itemType,
		"reason", reason,
	)
}

// FormatRateLimitHeader renders the protocol's rate-limit header.
//
// This is the mechanism that makes a switched-off category actually free: the
// official SDKs honour it and stop sending that category for the stated
// duration, so the cost is not paid on the wire at all (ADR 005).
func FormatRateLimitHeader(categories []engine.Category, seconds int) string {
	if len(categories) == 0 {
		return ""
	}
	names := make([]string, 0, len(categories))
	for _, category := range categories {
		names = append(names, string(category))
	}
	return fmt.Sprintf("%d:%s:organization", seconds, strings.Join(names, ";"))
}
