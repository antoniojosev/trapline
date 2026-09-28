package domain

import (
	"fmt"
	"hash/fnv"
	"math"
	"strings"
	"time"
)

// MinuteLayout is how a minute bucket is written down: UTC, to the minute, in
// a form that sorts lexicographically in the same order it sorts
// chronologically.
//
// The same choice HourLayout makes and for the same reason (ADR 010): a range
// query over buckets is an ordinary index scan on a TEXT column, and a row a
// human reads says what it means. Minutes and not hours for transactions,
// because "the deploy went out at 14:32 and the p95 tripled" is a question
// about minutes, and an hourly bucket would average the answer away.
const MinuteLayout = "2006-01-02T15:04"

// MinuteBucket is the bucket an instant belongs to.
func MinuteBucket(at time.Time) string {
	return at.UTC().Truncate(time.Minute).Format(MinuteLayout)
}

// ParseMinuteBucket reads a bucket back as the instant it starts at.
func ParseMinuteBucket(bucket string) (time.Time, error) {
	parsed, err := time.ParseInLocation(MinuteLayout, bucket, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %q is not a minute bucket", ErrInvalidRange, bucket)
	}
	return parsed, nil
}

// MinuteRetention is how long the per-minute buckets are kept.
//
// Forty-eight hours, because that is the window in which somebody asks a
// minute-level question: "what happened during the incident last night". Past
// it the hourly rollup answers everything anybody asks, at a sixtieth of the
// rows — and the rollup is exact, because merging sketches is exact (ADR 020,
// ADR 021).
const MinuteRetention = 48 * time.Hour

// DownsampleAge is how old a minute has to be before it is folded into its
// hour.
//
// Two hours, and the margin is the point. A minute is only safe to fold once
// nothing more can land in it, and something still can: an SDK batches and
// flushes, a mobile client reports a session it recorded while offline, a
// queue backs up. Folding at the top of the hour would silently lose whatever
// arrived late, and the loss would be invisible — the hour would simply be a
// little smaller than it should have been.
const DownsampleAge = 2 * time.Hour

// DefaultTracesSampleRate is the share of traces whose raw spans are kept when
// a project has not chosen otherwise.
//
// One in ten. The aggregates are computed over everything received, so this
// number costs no accuracy in the charts at all (ADR 021) — it only decides
// how many waterfalls are available to open. Ten percent of a busy service is
// still thousands of examples a day, and a hundred percent is a storage bill
// nobody agreed to.
const DefaultTracesSampleRate = 0.1

// TransactionStatus is the protocol's span status, as it arrives.
type TransactionStatus string

// The three statuses that do not mean something went wrong.
//
// Everything else — internal_error, deadline_exceeded, unavailable, the whole
// gRPC-derived list — counts as a failure. Listing the successes rather than
// the failures is deliberate: the failure list is open, and a status this
// build has never heard of is far more likely to be a new way of failing than
// a new way of succeeding. Getting that backwards would make a failure rate
// quietly under-report, which is the direction nobody notices.
const (
	StatusOK        TransactionStatus = "ok"
	StatusCancelled TransactionStatus = "cancelled"
	StatusUnknown   TransactionStatus = "unknown"
)

// Failed reports whether a status counts against the failure rate.
//
// An empty status is not a failure. Several SDKs omit it entirely for a
// transaction that completed normally, and reading absence as failure would
// put those services at a hundred percent failure the day tracing is switched
// on.
func (s TransactionStatus) Failed() bool {
	switch TransactionStatus(strings.ToLower(strings.TrimSpace(string(s)))) {
	case "", StatusOK, StatusCancelled, StatusUnknown:
		return false
	default:
		return true
	}
}

// MaxTransactionNameLength bounds the aggregation key.
//
// The name is chosen by the SDK, and an SDK that names transactions after
// URLs with ids in them is the classic way to turn an aggregate table into a
// copy of the event log. Truncation bounds one row; it does not bound the
// cardinality, which is the operator's problem and the panel's to surface.
const MaxTransactionNameLength = 200

// UnnamedTransaction is what a transaction with no name aggregates under.
//
// A name rather than a refusal: the SDK sent a real measurement and dropping
// it would put a hole in the p95 for a naming problem. A name rather than an
// empty string, because an empty key in a listing reads as a rendering bug.
const UnnamedTransaction = "<unnamed>"

// TransactionName normalises the aggregation key.
func TransactionName(raw string) string {
	name := strings.TrimSpace(raw)
	if name == "" {
		return UnnamedTransaction
	}
	if len(name) > MaxTransactionNameLength {
		return name[:MaxTransactionNameLength]
	}
	return name
}

// ValidTracesSampleRate reports whether a configured rate is one.
func ValidTracesSampleRate(rate float64) bool {
	return !math.IsNaN(rate) && rate >= 0 && rate <= 1
}

// SampleTrace decides whether a trace's raw spans are kept.
//
// Deterministic in the trace id, and that is the whole design. Sampling per
// transaction — a coin flip each time — would keep some transactions of a
// trace and drop others, and every waterfall would come out with holes in it
// that look exactly like instrumentation that was never added. Hashing the
// trace id instead means every transaction of one request reaches the same
// verdict, in this process and in the next one, without any shared state
// (ADR 021).
//
// FNV-1a because it is in the standard library, is not a credential decision,
// and is stable across builds and architectures. A cryptographic hash would
// cost more per transaction on the ingest path and buy nothing: the worst an
// adversary can do by choosing trace ids is get their own traces stored, which
// they can already do by sending more of them.
func SampleTrace(traceID string, rate float64) bool {
	switch {
	case traceID == "":
		// Nothing to hash, and nothing to correlate afterwards. Checked
		// before the rate rather than after, so that a rate of 1 cannot store
		// a trace whose identity is the empty string — which would be one row
		// that every other id-less trace then overwrote.
		return false
	case !ValidTracesSampleRate(rate), rate <= 0:
		return false
	case rate >= 1:
		return true
	}

	digest := fnv.New64a()
	// Hash.Write never returns an error, by contract.
	_, _ = digest.Write([]byte(traceID))

	// The top 53 bits of the mixed hash, mapped into [0,1). 53 because that is
	// what a float64 represents exactly, so the comparison below is the
	// comparison it looks like rather than one perturbed by rounding.
	const mantissa = 1 << 53
	fraction := float64(mix64(digest.Sum64())>>11) / float64(mantissa)
	return fraction < rate
}

// mix64 is the avalanche step, and it is not optional.
//
// FNV-1a folds each byte into the low end of the accumulator, so its high bits
// are dominated by the bytes it saw first. Trace ids that share a prefix —
// which is what a test fixture, a sequential generator or a tracing library
// with a fixed epoch produces — then land in the same region of the top bits,
// and taking the top 53 of a raw FNV sum put four hundred such ids either all
// above a 10% threshold or all below it. Sampling looked like it worked at
// scale and stored nothing at all for a whole class of input.
//
// This is MurmurHash3's fmix64 finaliser: two xor-shifts and two odd
// multiplications, which is enough for one changed bit anywhere in the input to
// change roughly half the output bits. It is not a cryptographic step and does
// not need to be — the worst an adversary gains by choosing trace ids is
// storing their own traces, which sending more of them already achieves.
func mix64(value uint64) uint64 {
	value ^= value >> 33
	value *= 0xff51afd7ed558ccd
	value ^= value >> 33
	value *= 0xc4ceb9fe1a85ec53
	value ^= value >> 33
	return value
}

// Resolution is the granularity a transaction series is read at.
type Resolution string

// The two granularities stored.
const (
	// ResolutionMinute is the last 48 hours, one point per minute.
	ResolutionMinute Resolution = "minute"
	// ResolutionHour is the whole retained history, one point per hour.
	ResolutionHour Resolution = "hour"
)

// Resolutions lists both, so a client can render the choice without keeping
// its own copy of a list that goes stale.
func Resolutions() []Resolution { return []Resolution{ResolutionMinute, ResolutionHour} }

// ParseResolution validates a requested granularity, defaulting to minutes.
func ParseResolution(raw string) (Resolution, error) {
	switch Resolution(strings.TrimSpace(raw)) {
	case "", ResolutionMinute:
		return ResolutionMinute, nil
	case ResolutionHour:
		return ResolutionHour, nil
	default:
		return "", fmt.Errorf("%w: unknown resolution %q, expected minute or hour",
			ErrInvalidRange, raw)
	}
}

// Bucket renders an instant at this resolution.
func (r Resolution) Bucket(at time.Time) string {
	if r == ResolutionHour {
		return HourBucket(at)
	}
	return MinuteBucket(at)
}

// Step is the distance between two consecutive buckets.
func (r Resolution) Step() time.Duration {
	if r == ResolutionHour {
		return time.Hour
	}
	return time.Minute
}

// Buckets lists every bucket a range covers at this resolution, empty ones
// included — for the same reason Range.Buckets does: a chart drawn from only
// the buckets that had traffic draws a quiet night as a straight line between
// two spikes.
func (r Resolution) Buckets(window Range) []string {
	step := r.Step()
	// The range is a closed interval of whole hours, so at minute resolution
	// the last hour contributes its sixty minutes rather than only its first.
	last := window.To
	if r == ResolutionMinute {
		last = window.To.Add(time.Hour - time.Minute)
	}
	if last.Before(window.From) {
		return nil
	}

	count := int(last.Sub(window.From)/step) + 1
	if count > MaxSeriesPoints {
		count = MaxSeriesPoints
	}
	buckets := make([]string, 0, count)
	for at := window.From; len(buckets) < count; at = at.Add(step) {
		buckets = append(buckets, r.Bucket(at))
	}
	return buckets
}

// MaxSeriesPoints bounds one series response.
//
// 2880 is exactly the minute retention: 48 hours of minutes. A client asking
// for more at minute resolution is asking for buckets that no longer exist,
// and a response with tens of thousands of empty points is a chart nobody can
// draw and a payload nobody wants.
const MaxSeriesPoints = 2880

// TransactionSort is how a transaction listing is ranked.
type TransactionSort string

// The three rankings, which are the three questions.
const (
	// SortP95 is "what is slow" — the default, because that is what the page
	// is opened for.
	SortP95 TransactionSort = "p95"
	// SortCount is "what is busy", which is what decides whether slow
	// matters.
	SortCount TransactionSort = "count"
	// SortFail is "what is breaking", which a latency chart never shows: a
	// request that fails fast looks fast.
	SortFail TransactionSort = "fail"
)

// TransactionSorts lists every ranking.
func TransactionSorts() []TransactionSort { return []TransactionSort{SortP95, SortCount, SortFail} }

// ParseTransactionSort validates a requested ranking, defaulting to p95.
func ParseTransactionSort(raw string) (TransactionSort, error) {
	trimmed := TransactionSort(strings.TrimSpace(raw))
	if trimmed == "" {
		return SortP95, nil
	}
	for _, known := range TransactionSorts() {
		if trimmed == known {
			return known, nil
		}
	}
	names := make([]string, 0, len(TransactionSorts()))
	for _, known := range TransactionSorts() {
		names = append(names, string(known))
	}
	return "", fmt.Errorf("%w: unknown sort %q, expected one of %s",
		ErrInvalidRange, raw, strings.Join(names, ", "))
}
