package domain

import "testing"

func TestThePerIPCeilingIsDerivedFromTheDesignVolume(t *testing.T) {
	// The same failure mode as the per-project default, one layer out: a
	// per-IP limit picked in isolation would eventually sit below the
	// throughput ADR 001 publishes, and the first symptom would be a customer
	// whose backend is refused at exactly the volume the README promised.
	if DefaultIngestIPRateLimitPerMinute%DefaultRateLimitPerMinute != 0 {
		t.Fatalf("the per-IP ceiling (%d/min) is not a multiple of the per-project one (%d/min), "+
			"so it was picked rather than derived",
			DefaultIngestIPRateLimitPerMinute, DefaultRateLimitPerMinute)
	}

	perSecond := DefaultIngestIPRateLimitPerMinute / 60
	if perSecond < DesignEventsPerSecond*SpikeHeadroom {
		t.Errorf("the per-IP ceiling allows %d events/s, below the %d events/s a single project "+
			"is already allowed to burst to; one host reporting one project would hit the address "+
			"limit before its own spike protection",
			perSecond, DesignEventsPerSecond*SpikeHeadroom)
	}

	// A host running several services must not be limited by its address
	// before any of its projects is limited by its own ceiling.
	if DefaultIngestIPRateLimitPerMinute < DefaultRateLimitPerMinute*IPProjectHeadroom {
		t.Errorf("the per-IP ceiling (%d/min) is below %d projects at the per-project ceiling (%d/min)",
			DefaultIngestIPRateLimitPerMinute, IPProjectHeadroom, DefaultRateLimitPerMinute)
	}
}

func TestAuthenticationIsRationedFarMoreTightlyThanIngest(t *testing.T) {
	// The asymmetry is the decision, so it is the thing worth asserting. If
	// these two ever converge, someone has either loosened the endpoint that
	// allocates 19 MiB per attempt or crippled the one that must absorb a
	// backend's whole error volume.
	if DefaultAuthRateLimitPerMinute >= DefaultIngestIPRateLimitPerMinute/100 {
		t.Errorf("auth allows %d/min against ingest's %d/min: an endpoint that spends Argon2id "+
			"per attempt must be rationed in a different order of magnitude",
			DefaultAuthRateLimitPerMinute, DefaultIngestIPRateLimitPerMinute)
	}
	// And still enough for the one human who can use it to mistype a password
	// a few times without locking themselves out of their own installation.
	if DefaultAuthRateLimitPerMinute < 5 {
		t.Errorf("auth allows only %d attempts a minute, which locks out the admin, "+
			"not the attacker", DefaultAuthRateLimitPerMinute)
	}
}

func TestTheLimitersOwnMemoryIsBounded(t *testing.T) {
	// An unbounded map keyed by address is the defence becoming the vector.
	// These are the numbers the eviction test in the limiter enforces, so a
	// zero or a negative here would quietly disable it.
	if MaxIngestIPEntries <= 0 || MaxAuthIPEntries <= 0 {
		t.Fatalf("entry bounds must be positive: ingest %d, auth %d", MaxIngestIPEntries, MaxAuthIPEntries)
	}
	if MaxAuthIPEntries > MaxIngestIPEntries {
		t.Errorf("the auth map (%d) is allowed to grow larger than the ingest one (%d), "+
			"though it has exactly one legitimate user", MaxAuthIPEntries, MaxIngestIPEntries)
	}
	// Roughly 56 bytes per entry between the key, the counter and the map's
	// own overhead. Both maps together must stay a rounding error against the
	// 30 MB footprint the README publishes.
	const bytesPerEntry = 56
	if total := (MaxIngestIPEntries + MaxAuthIPEntries) * bytesPerEntry; total > 2<<20 {
		t.Errorf("the limiters' maps can reach ~%d bytes, which is no longer a rounding error "+
			"against the published footprint", total)
	}
}
