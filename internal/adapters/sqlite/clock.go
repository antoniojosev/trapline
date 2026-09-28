package sqlite

import "time"

// nowUTC is the adapter's own clock, used only where a timestamp is a
// storage detail rather than a domain fact — the moment a revocation was
// recorded, for instance. Domain timestamps always arrive from a
// ports.Clock, so they stay assertable in tests.
func nowUTC() time.Time { return time.Now().UTC() }
