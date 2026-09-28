package engine

import "errors"

// Errors the engine returns. Consumers match on these with errors.Is rather
// than on strings.
var (
	// ErrInvalidEvent means an event fails an invariant the storage layer
	// depends on.
	ErrInvalidEvent = errors.New("invalid event")
	// ErrUnknownCategory means an event carries a category the engine does
	// not know.
	ErrUnknownCategory = errors.New("unknown category")
	// ErrRateLimited means a category is off or over its limit for a
	// project. Ingest adapters translate this into the protocol's own
	// backpressure signal so SDKs stop sending that category (ADR 005).
	ErrRateLimited = errors.New("rate limited")
)
