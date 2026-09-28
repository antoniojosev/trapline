package ports

import "time"

// Clock is the source of time. Injected rather than called directly so tests
// assert on exact timestamps instead of tolerating whatever the wall clock
// happened to say.
type Clock interface {
	Now() time.Time
}
