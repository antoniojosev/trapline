// Package sketch is a mergeable logarithmic histogram of positive values, in
// the shape of a DDSketch, with a bounded relative error.
//
// It exists because percentiles are not composable. Averaging the p95 of sixty
// minutes does not give the p95 of the hour, and with uneven traffic the
// deviation is enormous: a minute holding three slow requests would weigh the
// same as a minute holding ten thousand fast ones. Storing p50/p95/p99 as
// numbers would produce a dashboard whose figures look right and are not,
// which is the worst kind of observability bug — the one nobody detects and
// that makes people decide wrongly (ADR 007).
//
// So a window stores a sketch, and the percentile is computed when somebody
// asks, by merging the sketches of the range. A percentile is never persisted.
//
// # Why this and not t-digest
//
// Three reasons, in the order they mattered (ADR 020):
//
//   - Merging is exact. Two sketches with the same gamma have the same
//     buckets, so merging is adding counts — the result is bit-for-bit the
//     sketch the union of the inputs would have produced. A t-digest merge is
//     an approximation of an approximation, and downsampling applies it over
//     and over: every hour of history would be a slightly different shape
//     depending on the order the minutes happened to be folded in.
//   - It is deterministic. The same values in any order give the same
//     buckets, which is what lets a gate assert an exact figure and a test
//     compare two roads to the same number.
//   - It is small enough to read. A bucket index, a count, and a mapping that
//     fits in one line.
//
// # The boundary
//
// This lives inside internal/engine and imports nothing from the rest of the
// repository — the architecture test and depguard both enforce it. Windowed
// aggregation with sketches is a property of the engine, not of tracing (ADR
// 004, ADR 007): discovering it while implementing tracing would mean
// redesigning the engine with consumers already on top of it.
package sketch

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
)

// Alpha is the relative error the mapping guarantees: a quantile this sketch
// reports is within 1% of the value at that rank in the exact sample.
//
// One percent, because the question being answered is "which endpoint got
// slower after the deploy?" and nobody needs a p95 to the microsecond. It is
// also what keeps the bucket count small: at this gamma a range from one
// microsecond to one hour is about 1150 buckets, and a real transaction name
// occupies a few dozen of them.
const Alpha = 0.01

// gamma is the ratio between one bucket's upper bound and the previous one's.
//
// (1+α)/(1−α) is the value that makes the midpoint estimate below correct: a
// bucket covers (gamma^(i-1), gamma^i], and reporting 2·gamma^i/(gamma+1) for
// anything in it is wrong by at most α, relative, at both ends.
const gamma = (1 + Alpha) / (1 - Alpha)

// logGamma is precomputed because indexing runs once per recorded value, on
// the ingest path.
var logGamma = math.Log(gamma)

// The representable index window.
//
// gamma^i has to stay a finite float64, or the value a bucket reports would be
// +Inf and every quantile above it would be too. The limits are derived from
// the exponent range of float64 rather than picked: ln(MaxFloat64)/ln(gamma)
// is about 35 500, and the smallest subnormal lands at about −37 200. Rounded
// inwards, because a bucket whose reported value is one rounding step from
// infinity is not one anybody should be handed.
const (
	minIndex = -35000
	maxIndex = 35000
)

// maxBuckets bounds what UnmarshalBinary will allocate.
//
// It is far above what any real distribution reaches — the whole representable
// range is 70 000 buckets and a transaction's latencies occupy dozens — and it
// exists because these bytes come off disk, where a corrupted row could
// otherwise claim a bucket count that is a memory-exhaustion vector on the
// read path.
const maxBuckets = 1 << 20

// Errors this package returns.
var (
	// ErrInvalidValue means a value cannot be recorded: negative, NaN or
	// infinite. A duration is never any of those, and silently dropping one
	// would hide the bug that produced it.
	ErrInvalidValue = errors.New("sketch: value must be a finite number that is not negative")
	// ErrMalformed means a serialised sketch is not one.
	ErrMalformed = errors.New("sketch: malformed encoding")
)

// format is the version byte every encoding starts with.
//
// A version and not a magic string: this blob lives in a column beside the
// window it belongs to, so what it is is never in question — only which layout
// it uses, and that is exactly one byte's worth of information.
const format = 1

// Sketch is a mergeable histogram of non-negative values.
//
// The zero value is an empty sketch, ready to use. That matters on the read
// path: merging a range starts from a zero Sketch and folds the stored ones
// into it, and a constructor would only be a way to get that wrong.
type Sketch struct {
	// buckets maps a logarithmic index to how many values landed in it.
	// Sparse: a transaction whose latencies span two orders of magnitude
	// still only occupies the few hundred indices it actually reached.
	buckets map[int32]uint64
	// zeros counts exact zeros, which have no logarithm and therefore no
	// bucket. A sub-microsecond span rounded to zero milliseconds is an
	// ordinary observation, not an error, so it is counted rather than
	// refused.
	zeros uint64
	count uint64
	sum   float64
	// min and max are kept exactly. They are what the quantile is clamped to,
	// so a p99 can never be reported above the slowest request that actually
	// happened — which is the one figure a reader would immediately check
	// against their own logs.
	min float64
	max float64
}

// Add records one value.
//
// It returns an error rather than ignoring a value it cannot place: a NaN
// duration means something upstream computed a duration from two timestamps
// that were not both there, and a sketch that quietly swallowed it would move
// the bug to whoever reads the chart.
func (s *Sketch) Add(value float64) error {
	return s.AddN(value, 1)
}

// AddN records the same value n times.
//
// It exists for the decoder that reads a stored sketch and for tests that
// build a known distribution; the ingest path uses Add.
func (s *Sketch) AddN(value float64, n uint64) error {
	if n == 0 {
		return nil
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return fmt.Errorf("%w: %v", ErrInvalidValue, value)
	}

	if s.count == 0 || value < s.min {
		s.min = value
	}
	if s.count == 0 || value > s.max {
		s.max = value
	}
	s.count += n
	s.sum += value * float64(n)

	if value == 0 {
		s.zeros += n
		return nil
	}
	s.addToBucket(indexOf(value), n)
	return nil
}

func (s *Sketch) addToBucket(index int32, n uint64) {
	if s.buckets == nil {
		s.buckets = make(map[int32]uint64, 16)
	}
	s.buckets[index] += n
}

// indexOf is the mapping: the bucket whose upper bound is the first power of
// gamma at or above value.
//
// Clamped rather than refused. A value small enough to fall off the bottom of
// the representable window is 1e-304 of whatever unit is in use, which is
// indistinguishable from zero for every purpose this serves, and a value large
// enough to fall off the top is a clock that ran backwards — neither is worth
// losing a whole observation over, and both stay visible in min and max, which
// are kept exactly.
func indexOf(value float64) int32 {
	index := int32(math.Ceil(math.Log(value) / logGamma))
	switch {
	case index < minIndex:
		return minIndex
	case index > maxIndex:
		return maxIndex
	default:
		return index
	}
}

// valueOf is what a bucket reports: the point in it that is within alpha,
// relative, of both of its ends.
func valueOf(index int32) float64 {
	return 2 * math.Pow(gamma, float64(index)) / (gamma + 1)
}

// Count is how many values were recorded.
func (s *Sketch) Count() uint64 { return s.count }

// Sum is their total, kept exactly rather than estimated from the buckets. It
// is what makes a mean available at no extra cost, and a mean is what a
// throughput chart is labelled with.
func (s *Sketch) Sum() float64 { return s.sum }

// Min and Max are the smallest and largest values recorded, exactly.
func (s *Sketch) Min() float64 { return s.min }

// Max is the largest value recorded.
func (s *Sketch) Max() float64 { return s.max }

// Mean is the arithmetic mean, or zero for an empty sketch.
func (s *Sketch) Mean() float64 {
	if s.count == 0 {
		return 0
	}
	return s.sum / float64(s.count)
}

// Merge folds another sketch into this one.
//
// Exact, and that is the property the whole design rests on: both sketches use
// the same gamma, so their buckets are the same buckets, and merging is adding
// counts. The result is identical to the sketch that would have been built by
// recording every value of both, in any order — which is what makes an hour
// built from sixty minutes the same hour however the minutes were folded in.
func (s *Sketch) Merge(other *Sketch) {
	if other == nil || other.count == 0 {
		return
	}
	if s.count == 0 {
		s.min, s.max = other.min, other.max
	} else {
		s.min = math.Min(s.min, other.min)
		s.max = math.Max(s.max, other.max)
	}
	s.count += other.count
	s.sum += other.sum
	s.zeros += other.zeros
	for index, count := range other.buckets {
		s.addToBucket(index, count)
	}
}

// Quantile reports the value at rank q, and whether there was anything to
// report.
//
// The rank convention is nearest-rank on a zero-based sorted sample:
// q maps to the element at floor(q·(count−1)). It is stated here because a
// test that compares this against an exact quantile has to use the same one,
// and "off by one element" and "the estimator is broken" look identical in a
// failure message otherwise.
//
// The result is clamped to the recorded minimum and maximum. That only ever
// reduces the error, and it removes the one artefact a reader would notice
// immediately: a p99 fractionally above the slowest request that happened.
func (s *Sketch) Quantile(q float64) (float64, bool) {
	if s.count == 0 || q < 0 || q > 1 || math.IsNaN(q) {
		return 0, false
	}

	rank := q * float64(s.count-1)
	cumulative := float64(s.zeros)
	if cumulative > rank {
		return clamp(0, s.min, s.max), true
	}

	for _, index := range s.sortedIndices() {
		cumulative += float64(s.buckets[index])
		if cumulative > rank {
			return clamp(valueOf(index), s.min, s.max), true
		}
	}
	// Only reachable through floating-point rounding at the very top of the
	// range. The largest value recorded is the honest answer there.
	return s.max, true
}

// clamp keeps an estimate inside the range that actually occurred.
func clamp(value, low, high float64) float64 {
	switch {
	case value < low:
		return low
	case value > high:
		return high
	default:
		return value
	}
}

// sortedIndices returns the occupied buckets in ascending order.
//
// Sorted on every call rather than maintained: a quantile is read at query
// time over a merged sketch that is thrown away afterwards, while Add runs
// once per ingested transaction. Keeping the order on the write path would
// move the cost to the side that cannot afford it.
func (s *Sketch) sortedIndices() []int32 {
	indices := make([]int32, 0, len(s.buckets))
	for index := range s.buckets {
		indices = append(indices, index)
	}
	sort.Slice(indices, func(a, b int) bool { return indices[a] < indices[b] })
	return indices
}

// MarshalBinary encodes the sketch for storage.
//
// The layout, and why each piece is the shape it is:
//
//	version   1 byte
//	count     uvarint    how many values
//	zeros     uvarint    how many of them were exactly zero
//	sum       8 bytes    little-endian float64
//	min, max  8 bytes each
//	buckets   uvarint    how many pairs follow
//	pairs     varint index delta, uvarint count — ascending by index
//
// The indices are delta-encoded because a real distribution occupies a
// contiguous run of them: after the first, almost every delta is 1, which is
// one byte. Storing absolute indices would spend two or three bytes each to
// say the same thing.
//
// It never fails, so the error is always nil — the signature is
// encoding.BinaryMarshaler's, which is what lets a caller hand this to any
// code that takes one.
func (s *Sketch) MarshalBinary() ([]byte, error) {
	indices := s.sortedIndices()

	encoded := make([]byte, 0, 1+binary.MaxVarintLen64*2+24+binary.MaxVarintLen64+len(indices)*3)
	encoded = append(encoded, format)
	encoded = binary.AppendUvarint(encoded, s.count)
	encoded = binary.AppendUvarint(encoded, s.zeros)
	encoded = appendFloat(encoded, s.sum)
	encoded = appendFloat(encoded, s.min)
	encoded = appendFloat(encoded, s.max)
	encoded = binary.AppendUvarint(encoded, uint64(len(indices)))

	previous := int64(0)
	for _, index := range indices {
		encoded = binary.AppendVarint(encoded, int64(index)-previous)
		encoded = binary.AppendUvarint(encoded, s.buckets[index])
		previous = int64(index)
	}
	return encoded, nil
}

func appendFloat(dst []byte, value float64) []byte {
	return binary.LittleEndian.AppendUint64(dst, math.Float64bits(value))
}

// UnmarshalBinary decodes a stored sketch, replacing whatever this one held.
//
// These bytes come off disk, so this reads input it did not write: a truncated
// row, a blob from a future version, a column somebody edited by hand. It is
// fuzzed for that reason, and every field is checked rather than trusted —
// including the one check that is not about the encoding at all, that the
// bucket counts add up to the recorded total. A sketch whose count disagrees
// with its buckets would report quantiles from one distribution and a rate
// from another, and nothing downstream could tell.
func (s *Sketch) UnmarshalBinary(data []byte) error {
	reader := &cursor{data: data}

	version, err := reader.byteAt()
	if err != nil {
		return err
	}
	if version != format {
		return fmt.Errorf("%w: version %d, this build writes %d", ErrMalformed, version, format)
	}

	decoded := Sketch{}
	if decoded.count, err = reader.uvarint("count"); err != nil {
		return err
	}
	if decoded.zeros, err = reader.uvarint("zeros"); err != nil {
		return err
	}
	if decoded.sum, err = reader.float("sum"); err != nil {
		return err
	}
	if decoded.min, err = reader.float("min"); err != nil {
		return err
	}
	if decoded.max, err = reader.float("max"); err != nil {
		return err
	}

	bucketCount, err := reader.uvarint("bucket count")
	if err != nil {
		return err
	}
	if bucketCount > maxBuckets {
		return fmt.Errorf("%w: %d buckets, at most %d", ErrMalformed, bucketCount, maxBuckets)
	}
	// Two bytes is the smallest a pair can be, so a claimed count that cannot
	// fit in what is left is rejected before anything is allocated for it.
	// Compared in the widest signed type both sides fit in: bucketCount is
	// already known to be at most maxBuckets and remaining() cannot be
	// negative, so neither doubling nor the comparison can wrap.
	if remaining := int64(reader.remaining()); int64(bucketCount)*2 > remaining {
		return fmt.Errorf("%w: %d buckets do not fit in %d remaining bytes",
			ErrMalformed, bucketCount, remaining)
	}

	if bucketCount > 0 {
		decoded.buckets = make(map[int32]uint64, bucketCount)
	}
	total := decoded.zeros
	previous := int64(0)
	for range bucketCount {
		delta, err := reader.varint("bucket index")
		if err != nil {
			return err
		}
		index := previous + delta
		if index < minIndex || index > maxIndex {
			return fmt.Errorf("%w: bucket index %d is outside [%d, %d]",
				ErrMalformed, index, minIndex, maxIndex)
		}
		if len(decoded.buckets) > 0 && index <= previous {
			// Ascending order is what the delta encoding means. Accepting an
			// out-of-order pair would let two encodings describe one sketch,
			// and then a round trip would stop being a round trip.
			return fmt.Errorf("%w: bucket indices are not ascending (%d after %d)",
				ErrMalformed, index, previous)
		}
		count, err := reader.uvarint("bucket count")
		if err != nil {
			return err
		}
		if count == 0 {
			return fmt.Errorf("%w: bucket %d is empty; empty buckets are not written", ErrMalformed, index)
		}
		// Checked before adding, so a crafted pair of counts cannot wrap the
		// total around into agreeing with the header.
		if count > math.MaxUint64-total {
			return fmt.Errorf("%w: the bucket counts overflow", ErrMalformed)
		}
		total += count
		// #nosec G115 -- index is checked against [minIndex, maxIndex] above,
		// and both fit in an int32 by construction (they are the bucket range
		// this mapping can produce).
		decoded.buckets[int32(index)] = count
		previous = index
	}

	if reader.remaining() != 0 {
		return fmt.Errorf("%w: %d trailing bytes", ErrMalformed, reader.remaining())
	}
	if total != decoded.count {
		return fmt.Errorf("%w: the buckets hold %d values, the header says %d",
			ErrMalformed, total, decoded.count)
	}
	if err := decoded.validateSummary(); err != nil {
		return err
	}

	*s = decoded
	return nil
}

// validateSummary checks the three exact numbers against each other.
//
// They are not derivable from the buckets, so nothing else would catch a row
// whose min is above its max or whose sum is NaN — and both would travel
// straight into a response as a duration nobody can explain.
func (s *Sketch) validateSummary() error {
	if math.IsNaN(s.sum) || math.IsInf(s.sum, 0) {
		return fmt.Errorf("%w: sum is %v", ErrMalformed, s.sum)
	}
	if math.IsNaN(s.min) || math.IsNaN(s.max) {
		return fmt.Errorf("%w: min or max is not a number", ErrMalformed)
	}
	if s.count == 0 {
		if s.min != 0 || s.max != 0 || s.sum != 0 {
			return fmt.Errorf("%w: an empty sketch carries min %v, max %v, sum %v",
				ErrMalformed, s.min, s.max, s.sum)
		}
		return nil
	}
	if s.min > s.max {
		return fmt.Errorf("%w: min %v is above max %v", ErrMalformed, s.min, s.max)
	}
	if s.min < 0 {
		return fmt.Errorf("%w: min %v is negative", ErrMalformed, s.min)
	}
	return nil
}

// cursor reads the encoding one field at a time, naming the field it was
// reading when the bytes ran out.
//
// A bare bytes.Reader would report "unexpected EOF" for every one of them,
// which for a blob read off disk is the difference between knowing the row is
// truncated after its header and knowing only that it is bad.
type cursor struct {
	data   []byte
	offset int
}

func (c *cursor) remaining() int { return len(c.data) - c.offset }

func (c *cursor) byteAt() (byte, error) {
	if c.remaining() < 1 {
		return 0, fmt.Errorf("%w: empty", ErrMalformed)
	}
	value := c.data[c.offset]
	c.offset++
	return value, nil
}

func (c *cursor) uvarint(field string) (uint64, error) {
	value, read := binary.Uvarint(c.data[c.offset:])
	if read <= 0 {
		return 0, fmt.Errorf("%w: %s is truncated or overflows", ErrMalformed, field)
	}
	c.offset += read
	return value, nil
}

func (c *cursor) varint(field string) (int64, error) {
	value, read := binary.Varint(c.data[c.offset:])
	if read <= 0 {
		return 0, fmt.Errorf("%w: %s is truncated or overflows", ErrMalformed, field)
	}
	c.offset += read
	return value, nil
}

func (c *cursor) float(field string) (float64, error) {
	if c.remaining() < 8 {
		return 0, fmt.Errorf("%w: %s is truncated", ErrMalformed, field)
	}
	value := math.Float64frombits(binary.LittleEndian.Uint64(c.data[c.offset:]))
	c.offset += 8
	return value, nil
}
