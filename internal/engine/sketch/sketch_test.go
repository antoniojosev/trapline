package sketch

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"math/rand/v2"
	"reflect"
	"sort"
	"testing"
)

// quantiles are the three the product actually reports, plus the two ends,
// because the ends are where a rank convention goes wrong and the middle is
// where it never shows.
var quantiles = []float64{0, 0.5, 0.75, 0.9, 0.95, 0.99, 1}

// errorBudget is Alpha with room for the last bit of a float64.
//
// The bound is tight by construction, not loose: a value sitting exactly on a
// bucket's lower edge is wrong by exactly Alpha, and 1.0 is such a value —
// bucket 0 covers (0.9802, 1] and reports 0.99. Computing that error in
// floating point yields 0.010000000000000009, so a bare `> Alpha` turns the
// guarantee "at most one percent" into a test that fails on rounding. The
// slack is twelve orders of magnitude below the thing being measured, so it
// cannot hide a real regression.
const errorBudget = Alpha + 1e-12

// exactQuantile is the value at rank q in a sorted sample, under the same
// nearest-rank convention Quantile documents: floor(q·(n−1)), zero-based.
//
// Stated once, here, because the whole property being tested is "the sketch
// agrees with this to within Alpha", and a test that used a different
// convention would be measuring the disagreement between two definitions
// rather than the error of the estimator.
func exactQuantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(q * float64(len(sorted)-1))
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

// distribution is one synthetic sample with a name to blame in a failure.
type distribution struct {
	name   string
	values []float64
}

// syntheticDistributions are the three shapes a latency sample actually takes.
//
// Uniform is the easy case and catches an estimator that is simply wrong.
// Log-normal is what real request latency looks like, and it is the one that
// punishes a linear bucketing. Bimodal — a cache hit and a cache miss — is the
// one that breaks anything relying on a single mode, which is precisely the
// case an average hides and a p95 exists to expose.
func syntheticDistributions(t *testing.T) []distribution {
	t.Helper()
	// A fixed seed: this is a property test, not a search. A different sample
	// every run means a failure nobody can reproduce, and the property holds
	// for every sample or it does not hold.
	random := rand.New(rand.NewPCG(0x5EED, 0xF5A))

	const n = 20000

	uniform := make([]float64, n)
	for i := range uniform {
		uniform[i] = 1 + random.Float64()*999
	}

	lognormal := make([]float64, n)
	for i := range lognormal {
		lognormal[i] = math.Exp(random.NormFloat64()*1.2 + 3)
	}

	bimodal := make([]float64, n)
	for i := range bimodal {
		if random.Float64() < 0.85 {
			bimodal[i] = 2 + random.Float64()*3 // a cache hit
			continue
		}
		bimodal[i] = 400 + random.Float64()*600 // a cache miss
	}

	return []distribution{
		{"uniform", uniform},
		{"log-normal", lognormal},
		{"bimodal", bimodal},
	}
}

func TestQuantileIsWithinAlphaOfTheExactValue(t *testing.T) {
	for _, dist := range syntheticDistributions(t) {
		t.Run(dist.name, func(t *testing.T) {
			var s Sketch
			for _, value := range dist.values {
				if err := s.Add(value); err != nil {
					t.Fatalf("adding %v: %v", value, err)
				}
			}

			sorted := append([]float64(nil), dist.values...)
			sort.Float64s(sorted)

			for _, q := range quantiles {
				got, ok := s.Quantile(q)
				if !ok {
					t.Fatalf("q=%v: the sketch reports nothing for a sample of %d", q, len(dist.values))
				}
				want := exactQuantile(sorted, q)
				if relativeError(got, want) > errorBudget {
					t.Errorf("q=%v: got %.6f, exact %.6f, relative error %.5f > budget %.5f",
						q, got, want, relativeError(got, want), errorBudget)
				}
			}
		})
	}
}

func relativeError(got, want float64) float64 {
	if want == 0 {
		if got == 0 {
			return 0
		}
		return math.Inf(1)
	}
	return math.Abs(got-want) / math.Abs(want)
}

// TestMergingNSketchesEqualsOneSketchOfTheUnion is the property downsampling
// rests on.
//
// An hour is built by merging sixty minutes, and then a day would be built by
// merging hours. If merging were approximate — as it is for a t-digest — every
// level of that would drift, and the drift would depend on the order the
// windows happened to be folded in. Here the buckets have to come out
// identical, not close (ADR 020).
func TestMergingNSketchesEqualsOneSketchOfTheUnion(t *testing.T) {
	for _, dist := range syntheticDistributions(t) {
		t.Run(dist.name, func(t *testing.T) {
			const parts = 60 // as many parts as an hour has minutes

			union := &Sketch{}
			partials := make([]*Sketch, parts)
			for i := range partials {
				partials[i] = &Sketch{}
			}
			for index, value := range dist.values {
				if err := union.Add(value); err != nil {
					t.Fatalf("adding to the union: %v", err)
				}
				if err := partials[index%parts].Add(value); err != nil {
					t.Fatalf("adding to partial %d: %v", index%parts, err)
				}
			}

			merged := &Sketch{}
			for _, partial := range partials {
				merged.Merge(partial)
			}

			assertSameShape(t, merged, union)

			// And in the other order, because "exact" has to mean
			// order-independent or downsampling is a lottery.
			reversed := &Sketch{}
			for i := parts - 1; i >= 0; i-- {
				reversed.Merge(partials[i])
			}
			assertSameShape(t, reversed, union)
		})
	}
}

// assertSameShape compares everything that decides an answer.
//
// The sum is compared with a tolerance and everything else exactly: floating
// point addition is not associative, so folding sixty partial sums cannot be
// expected to reproduce one long sum bit for bit, while the counts are
// integers and must.
func assertSameShape(t *testing.T, got, want *Sketch) {
	t.Helper()
	if got.count != want.count {
		t.Errorf("count %d, want %d", got.count, want.count)
	}
	if got.zeros != want.zeros {
		t.Errorf("zeros %d, want %d", got.zeros, want.zeros)
	}
	if got.min != want.min || got.max != want.max {
		t.Errorf("range [%v, %v], want [%v, %v]", got.min, got.max, want.min, want.max)
	}
	if relativeError(got.sum, want.sum) > 1e-9 {
		t.Errorf("sum %v, want %v", got.sum, want.sum)
	}
	if !reflect.DeepEqual(got.buckets, want.buckets) {
		t.Errorf("the buckets differ: %d against %d occupied", len(got.buckets), len(want.buckets))
	}
	for _, q := range quantiles {
		gotValue, _ := got.Quantile(q)
		wantValue, _ := want.Quantile(q)
		if gotValue != wantValue {
			t.Errorf("q=%v: merged reports %v, the union reports %v", q, gotValue, wantValue)
		}
	}
}

func TestMergeIntoAnEmptySketch(t *testing.T) {
	var source Sketch
	for _, value := range []float64{5, 10, 15} {
		if err := source.Add(value); err != nil {
			t.Fatalf("adding: %v", err)
		}
	}

	var target Sketch
	target.Merge(&source)
	assertSameShape(t, &target, &source)

	// And the other direction: merging an empty sketch changes nothing.
	before, _ := target.MarshalBinary()
	target.Merge(&Sketch{})
	target.Merge(nil)
	after, _ := target.MarshalBinary()
	if !bytes.Equal(before, after) {
		t.Error("merging an empty sketch changed the sketch")
	}
}

func TestRoundTripThroughBinary(t *testing.T) {
	for _, dist := range syntheticDistributions(t) {
		t.Run(dist.name, func(t *testing.T) {
			var original Sketch
			for _, value := range dist.values {
				if err := original.Add(value); err != nil {
					t.Fatalf("adding: %v", err)
				}
			}

			encoded, err := original.MarshalBinary()
			if err != nil {
				t.Fatalf("marshalling: %v", err)
			}

			var decoded Sketch
			if err := decoded.UnmarshalBinary(encoded); err != nil {
				t.Fatalf("unmarshalling: %v", err)
			}
			assertSameShape(t, &decoded, &original)
			if decoded.sum != original.sum {
				t.Errorf("sum survived as %v, want %v exactly", decoded.sum, original.sum)
			}

			// The encoding is canonical: the same sketch encodes to the same
			// bytes, so a stored blob can be compared without decoding it.
			reencoded, err := decoded.MarshalBinary()
			if err != nil {
				t.Fatalf("re-marshalling: %v", err)
			}
			if !bytes.Equal(reencoded, encoded) {
				t.Error("re-encoding a decoded sketch produced different bytes")
			}

			t.Logf("%s: %d values, %d occupied buckets, %d bytes encoded (%.3f bytes/value)",
				dist.name, original.count, len(original.buckets), len(encoded),
				float64(len(encoded))/float64(original.count))
		})
	}
}

func TestRoundTripOfAnEmptySketch(t *testing.T) {
	var empty Sketch
	encoded, err := empty.MarshalBinary()
	if err != nil {
		t.Fatalf("marshalling an empty sketch: %v", err)
	}

	var decoded Sketch
	if err := decoded.UnmarshalBinary(encoded); err != nil {
		t.Fatalf("unmarshalling an empty sketch: %v", err)
	}
	if decoded.Count() != 0 {
		t.Errorf("count %d, want 0", decoded.Count())
	}
	if _, ok := decoded.Quantile(0.95); ok {
		t.Error("an empty sketch reported a quantile")
	}
}

func TestZerosAreRecordedRatherThanRefused(t *testing.T) {
	var s Sketch
	for range 10 {
		if err := s.Add(0); err != nil {
			t.Fatalf("adding zero: %v", err)
		}
	}
	if err := s.Add(100); err != nil {
		t.Fatalf("adding: %v", err)
	}

	if s.Count() != 11 {
		t.Errorf("count %d, want 11", s.Count())
	}
	if s.Min() != 0 {
		t.Errorf("min %v, want 0", s.Min())
	}
	if got, _ := s.Quantile(0.5); got != 0 {
		t.Errorf("p50 of ten zeros and one hundred is %v, want 0", got)
	}
	if got, _ := s.Quantile(1); relativeError(got, 100) > errorBudget {
		t.Errorf("p100 is %v, want 100 within alpha", got)
	}

	encoded, err := s.MarshalBinary()
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	var decoded Sketch
	if err := decoded.UnmarshalBinary(encoded); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}
	if decoded.zeros != 10 {
		t.Errorf("zeros survived as %d, want 10", decoded.zeros)
	}
}

func TestAddRefusesValuesThatAreNotDurations(t *testing.T) {
	for _, value := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		var s Sketch
		if err := s.Add(value); err == nil {
			t.Errorf("Add(%v) was accepted; a duration is never that", value)
		}
		if s.Count() != 0 {
			t.Errorf("Add(%v) was refused but still counted", value)
		}
	}
}

func TestAddNRecordsTheSameValueManyTimes(t *testing.T) {
	var repeated Sketch
	if err := repeated.AddN(42, 1000); err != nil {
		t.Fatalf("AddN: %v", err)
	}

	var oneByOne Sketch
	for range 1000 {
		if err := oneByOne.Add(42); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if repeated.count != oneByOne.count || !reflect.DeepEqual(repeated.buckets, oneByOne.buckets) {
		t.Error("AddN and a thousand Adds produced different sketches")
	}
	if err := repeated.AddN(99, 0); err != nil {
		t.Fatalf("AddN with a count of zero: %v", err)
	}
	if repeated.count != 1000 {
		t.Errorf("AddN with a count of zero recorded something: count %d", repeated.count)
	}
}

func TestSummaryStatisticsAreExact(t *testing.T) {
	var s Sketch
	values := []float64{3, 1, 4, 1, 5, 9, 2, 6}
	for _, value := range values {
		if err := s.Add(value); err != nil {
			t.Fatalf("adding: %v", err)
		}
	}

	if s.Min() != 1 || s.Max() != 9 {
		t.Errorf("range [%v, %v], want [1, 9]", s.Min(), s.Max())
	}
	if s.Sum() != 31 {
		t.Errorf("sum %v, want 31", s.Sum())
	}
	if s.Mean() != 31.0/8 {
		t.Errorf("mean %v, want %v", s.Mean(), 31.0/8)
	}
	var empty Sketch
	if empty.Mean() != 0 {
		t.Errorf("the mean of nothing is %v, want 0", empty.Mean())
	}
}

func TestQuantileRefusesRanksThatAreNotRanks(t *testing.T) {
	var s Sketch
	if err := s.Add(10); err != nil {
		t.Fatalf("adding: %v", err)
	}
	for _, q := range []float64{-0.1, 1.1, math.NaN()} {
		if _, ok := s.Quantile(q); ok {
			t.Errorf("Quantile(%v) answered; that is not a rank", q)
		}
	}
}

// TestTheMappingHoldsAcrossTheWholeRange checks the estimator directly rather
// than through a sample: every bucket's reported value has to be within Alpha
// of both of its ends, or the guarantee is only true for the values that
// happened to be drawn.
func TestTheMappingHoldsAcrossTheWholeRange(t *testing.T) {
	for _, value := range []float64{1e-6, 1e-3, 0.5, 1, 1.5, 10, 1000, 1e6, 1e9} {
		index := indexOf(value)
		reported := valueOf(index)
		if relativeError(reported, value) > errorBudget {
			t.Errorf("value %v landed in bucket %d, which reports %v: relative error %.5f",
				value, index, reported, relativeError(reported, value))
		}
	}
}

func TestExtremeValuesAreClampedRatherThanLost(t *testing.T) {
	var s Sketch
	for _, value := range []float64{1e-320, math.MaxFloat64} {
		if err := s.Add(value); err != nil {
			t.Fatalf("adding %v: %v", value, err)
		}
	}
	if s.Count() != 2 {
		t.Errorf("count %d, want 2: an extreme value is clamped, never dropped", s.Count())
	}
	// Clamped into the representable window, and still readable: the exact
	// ends survive in min and max, which is where a reader would look.
	if s.Min() != 1e-320 || s.Max() != math.MaxFloat64 {
		t.Errorf("range [%v, %v], want the exact values", s.Min(), s.Max())
	}
	for _, q := range quantiles {
		got, ok := s.Quantile(q)
		if !ok || math.IsInf(got, 0) || math.IsNaN(got) {
			t.Errorf("q=%v reported %v, which is not a number a chart can draw", q, got)
		}
	}
}

func TestUnmarshalRejectsCorruption(t *testing.T) {
	var s Sketch
	for _, value := range []float64{1, 2, 3, 100} {
		if err := s.Add(value); err != nil {
			t.Fatalf("adding: %v", err)
		}
	}
	valid, err := s.MarshalBinary()
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	cases := []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"a version this build does not write", append([]byte{format + 1}, valid[1:]...)},
		{"truncated after the version", valid[:1]},
		{"truncated mid-header", valid[:6]},
		{"truncated mid-bucket", valid[:len(valid)-1]},
		{"trailing bytes", append(append([]byte(nil), valid...), 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var decoded Sketch
			if err := decoded.UnmarshalBinary(tc.data); err == nil {
				t.Error("accepted")
			}
		})
	}
}

// TestUnmarshalRejectsASketchThatDisagreesWithItself is the check that is not
// about the encoding.
//
// A blob whose header says a thousand values while its buckets hold three
// decodes perfectly well and then reports a p95 from one distribution beside a
// count from another. Nothing downstream could notice, which is why it is
// refused here.
func TestUnmarshalRejectsASketchThatDisagreesWithItself(t *testing.T) {
	build := func(mutate func(*Sketch)) []byte {
		var s Sketch
		for _, value := range []float64{1, 2, 3} {
			if err := s.Add(value); err != nil {
				t.Fatalf("adding: %v", err)
			}
		}
		mutate(&s)
		encoded, err := s.MarshalBinary()
		if err != nil {
			t.Fatalf("marshalling: %v", err)
		}
		return encoded
	}

	cases := map[string][]byte{
		"a count the buckets do not add up to": build(func(s *Sketch) { s.count = 1000 }),
		"a minimum above the maximum":          build(func(s *Sketch) { s.min, s.max = 10, 1 }),
		"a negative minimum":                   build(func(s *Sketch) { s.min = -1 }),
		"a sum that is not a number":           build(func(s *Sketch) { s.sum = math.NaN() }),
		"an empty sketch carrying a range": build(func(s *Sketch) {
			s.count, s.buckets, s.zeros = 0, nil, 0
		}),
	}
	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			var decoded Sketch
			if err := decoded.UnmarshalBinary(encoded); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func TestUnmarshalRejectsAnImpossibleBucketCount(t *testing.T) {
	var s Sketch
	if err := s.Add(1); err != nil {
		t.Fatalf("adding: %v", err)
	}
	encoded, err := s.MarshalBinary()
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	// The bucket count is the last uvarint before the pairs. Rewriting the
	// whole tail is simpler and more honest than patching one byte: what is
	// under test is that a claimed count larger than the remaining bytes is
	// refused before anything is allocated for it.
	header := encoded[:len(encoded)-3]
	crafted := append(append([]byte(nil), header...), 0xFF, 0xFF, 0xFF, 0x7F)

	var decoded Sketch
	if err := decoded.UnmarshalBinary(crafted); err == nil {
		t.Error("a bucket count that cannot fit in the remaining bytes was accepted")
	}
}

// TestUnmarshalOverwritesWhateverWasThere: a sketch reused across rows must
// not accumulate the previous one, which is the shape of bug that turns a
// range query into nonsense only when it is read in a particular order.
func TestUnmarshalOverwritesWhateverWasThere(t *testing.T) {
	var first Sketch
	if err := first.Add(1000); err != nil {
		t.Fatalf("adding: %v", err)
	}
	encoded, err := first.MarshalBinary()
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	reused := &Sketch{}
	for _, value := range []float64{1, 2, 3, 4, 5} {
		if err := reused.Add(value); err != nil {
			t.Fatalf("adding: %v", err)
		}
	}
	if err := reused.UnmarshalBinary(encoded); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}
	if reused.Count() != 1 {
		t.Errorf("count %d after decoding over an existing sketch, want 1", reused.Count())
	}
}

func BenchmarkAdd(b *testing.B) {
	var s Sketch
	for i := 0; b.Loop(); i++ {
		_ = s.Add(float64(i%1000) + 0.5)
	}
}

func BenchmarkMergeAnHourOfMinutes(b *testing.B) {
	minutes := make([]*Sketch, 60)
	for i := range minutes {
		minutes[i] = &Sketch{}
		for j := range 500 {
			_ = minutes[i].Add(float64(j%400) + 1)
		}
	}
	b.ResetTimer()
	for b.Loop() {
		merged := &Sketch{}
		for _, minute := range minutes {
			merged.Merge(minute)
		}
		_, _ = merged.Quantile(0.95)
	}
}

// header renders the fixed part of the encoding, so a test can craft a body
// that MarshalBinary would never produce.
//
// Hand-built rather than mutated from a real encoding: the branches below are
// exactly the ones a corrupted row or a hostile blob reaches and a valid
// encoding never does, so there is nothing valid to start from.
func header(count, zeros uint64, sum, minimum, maximum float64) []byte {
	encoded := []byte{format}
	encoded = binary.AppendUvarint(encoded, count)
	encoded = binary.AppendUvarint(encoded, zeros)
	encoded = appendFloat(encoded, sum)
	encoded = appendFloat(encoded, minimum)
	encoded = appendFloat(encoded, maximum)
	return encoded
}

// pair renders one bucket the way MarshalBinary does.
func pair(delta int64, count uint64) []byte {
	encoded := binary.AppendVarint(nil, delta)
	return binary.AppendUvarint(encoded, count)
}

// unterminated is a varint that never ends: every byte sets the continuation
// bit, so a decoder that trusted its length would read past the buffer.
var unterminated = []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}

func TestUnmarshalRejectsEveryMalformedShape(t *testing.T) {
	full := header(1, 1, 5, 5, 5)

	cases := map[string][]byte{
		"the count runs out mid-varint": append([]byte{format}, unterminated...),
		"nothing after the count":       full[:2],
		"the sum is cut short":          full[:4],
		"the minimum is cut short":      full[:12],
		"the maximum is cut short":      full[:20],
		"no bucket count at all":        full,

		"more buckets than this build will allocate": append(
			append([]byte(nil), full...), binary.AppendUvarint(nil, maxBuckets+1)...),

		"more buckets than the remaining bytes could hold": append(
			append(append([]byte(nil), full...), binary.AppendUvarint(nil, 10)...), 1, 2, 3),

		"a bucket index that never ends": append(
			append(append([]byte(nil), full...), binary.AppendUvarint(nil, 1)...), unterminated...),

		"a bucket index outside the representable window": append(
			append(append([]byte(nil), full...), binary.AppendUvarint(nil, 1)...),
			pair(maxIndex+1, 1)...),

		"bucket indices that do not ascend": append(
			append(append(append([]byte(nil), header(2, 0, 10, 5, 5)...),
				binary.AppendUvarint(nil, 2)...), pair(5, 1)...), pair(0, 1)...),

		"a bucket count that never ends": append(
			append(append(append([]byte(nil), full...), binary.AppendUvarint(nil, 1)...),
				binary.AppendVarint(nil, 1)...), unterminated...),

		"an empty bucket, which is never written": append(
			append(append([]byte(nil), full...), binary.AppendUvarint(nil, 1)...), pair(1, 0)...),

		"bucket counts that overflow the total": append(
			append(append([]byte(nil), header(1, math.MaxUint64, 5, 5, 5)...),
				binary.AppendUvarint(nil, 1)...), pair(1, 1)...),

		"a minimum that is not a number": append(
			append([]byte(nil), header(1, 1, 1, math.NaN(), 1)...), binary.AppendUvarint(nil, 0)...),
	}

	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			var decoded Sketch
			err := decoded.UnmarshalBinary(encoded)
			if err == nil {
				t.Fatal("accepted")
			}
			if !errors.Is(err, ErrMalformed) {
				t.Errorf("error is %v, want one a caller can match with ErrMalformed", err)
			}
			if decoded.Count() != 0 {
				t.Errorf("a rejected decode still left %d values behind", decoded.Count())
			}
		})
	}
}
