package sqlite

import "errors"

// Errors this adapter returns for conditions that are not the caller's
// fault but need to be distinguishable.
var (
	// ErrSchema means the database is not in a state this build can use.
	ErrSchema = errors.New("schema")
)
