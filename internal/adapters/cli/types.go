package cli

import "time"

// These mirror the API's response shapes. They are declared here rather than
// imported from the HTTP adapter on purpose: the CLI is a client of a
// versioned REST contract, and sharing structs with the server would let a
// change ripple through without anyone noticing the contract moved.
const (
	apiVersion = "v1"
	csrfHeader = "X-Trapline-Request"
)

type projectPayload struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Slug is what SENTRY_PROJECT takes (ADR 013).
	Slug      string       `json:"slug"`
	CreatedAt time.Time    `json:"created_at"`
	DSN       string       `json:"dsn"`
	Keys      []keyPayload `json:"keys"`
}

type keyPayload struct {
	PublicKey string    `json:"public_key"`
	DSN       string    `json:"dsn"`
	CreatedAt time.Time `json:"created_at"`
}

type createProjectPayload struct {
	Name string `json:"name"`
}
