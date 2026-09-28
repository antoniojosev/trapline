package sketch

import (
	"math/rand/v2"
	"testing"
)

// FuzzUnmarshalBinary drives the one function in this package that reads bytes
// it did not write.
//
// A sketch blob comes off disk. That is a narrower threat model than the
// envelope parser's — nobody posts a sketch — but not a trivial one: a
// truncated write, a restored backup from a build with a different layout, a
// column somebody edited with the sqlite3 shell. This runs on the read path of
// a dashboard, so a panic there takes down the endpoint an operator opened
// during an incident.
//
// Two properties, and the second is the one that catches the interesting bugs:
//
//   - it never panics, whatever the bytes say;
//   - anything it accepts is canonical — re-encoding what was decoded gives
//     back the identical bytes, and decoding those gives back the identical
//     sketch. Without that, two different blobs could mean one sketch, and the
//     round-trip test would be checking a weaker claim than it looks like.
func FuzzUnmarshalBinary(f *testing.F) {
	f.Add([]byte(nil))
	f.Add([]byte{format})
	f.Add([]byte{format + 1})

	// Real encodings as seeds, so the fuzzer starts from valid shapes and
	// mutates outwards rather than spending its budget rediscovering the
	// header.
	random := rand.New(rand.NewPCG(1, 2))
	for _, size := range []int{0, 1, 10, 1000} {
		var s Sketch
		for range size {
			_ = s.Add(random.Float64() * 1000)
		}
		encoded, err := s.MarshalBinary()
		if err != nil {
			f.Fatalf("seeding: %v", err)
		}
		f.Add(encoded)
	}
	// One with zeros in it, which is the branch a purely random sample never
	// reaches.
	var withZeros Sketch
	_ = withZeros.AddN(0, 7)
	_ = withZeros.Add(12.5)
	zeroed, err := withZeros.MarshalBinary()
	if err != nil {
		f.Fatalf("seeding the zero case: %v", err)
	}
	f.Add(zeroed)

	f.Fuzz(func(t *testing.T, data []byte) {
		var decoded Sketch
		if err := decoded.UnmarshalBinary(data); err != nil {
			return
		}

		// Accepted. Everything it now reports has to be usable, because the
		// caller is about to put these numbers in a response.
		for _, q := range []float64{0, 0.5, 0.95, 0.99, 1} {
			value, ok := decoded.Quantile(q)
			if !ok {
				continue
			}
			if value < decoded.Min() || value > decoded.Max() {
				t.Fatalf("q=%v reported %v, outside the recorded range [%v, %v]",
					q, value, decoded.Min(), decoded.Max())
			}
		}

		// Canonical: one sketch, one encoding.
		reencoded, err := decoded.MarshalBinary()
		if err != nil {
			t.Fatalf("re-encoding what was just decoded: %v", err)
		}
		var again Sketch
		if err := again.UnmarshalBinary(reencoded); err != nil {
			t.Fatalf("the re-encoding of an accepted sketch was rejected: %v", err)
		}
		if again.Count() != decoded.Count() || again.Sum() != decoded.Sum() {
			t.Fatalf("a round trip changed the sketch: %d/%v became %d/%v",
				decoded.Count(), decoded.Sum(), again.Count(), again.Sum())
		}

		// And merging it with itself must not produce something unusable,
		// because that is what the downsampling job does to every window it
		// reads.
		merged := &Sketch{}
		merged.Merge(&decoded)
		merged.Merge(&decoded)
		if merged.Count() != 2*decoded.Count() {
			t.Fatalf("merging a sketch with itself gave %d values, want %d",
				merged.Count(), 2*decoded.Count())
		}
	})
}
