package domain

// The per-IP budgets, derived rather than picked.
//
// They live in the domain, next to the design volume they come from, for the
// same reason DefaultRateLimitPerMinute does: the last time a limit was chosen
// on its own it contradicted ADR 001's published throughput and nobody noticed
// until a benchmark did. Every number here is either an arithmetic consequence
// of a design constant or a budget with its reasoning attached, and a test
// pins the relationship so it cannot drift back apart.

// IPProjectHeadroom is how many projects one machine may report to before its
// address, rather than any single project, becomes the thing that limits it.
//
// The per-project ceiling is spike protection: it answers "is this project
// misbehaving". The per-IP ceiling answers a different question — "is one host
// able to make this server pay for decoding" — and if it were set to the same
// figure it would silently become the binding limit for anyone running a few
// services on one box, which is the ordinary case this product is for. Four is
// a deliberate guess at "a few", and it is configurable precisely because it
// is a guess.
const IPProjectHeadroom = 4

// DefaultIngestIPRateLimitPerMinute is the per-address ceiling on ingest
// requests.
//
// Generous on purpose. All the legitimate traffic from a backend arrives from
// one address, so a tight limit here does not stop an attack, it stops the
// customer. What it does buy is a ceiling on how many times a single host can
// make this server read and decompress a body before being refused — which is
// the cost the limit exists to avoid paying, and the reason it is applied
// before the body is touched at all (ADR 023).
const DefaultIngestIPRateLimitPerMinute = DefaultRateLimitPerMinute * IPProjectHeadroom

// DefaultAuthRateLimitPerMinute is the per-address ceiling on attempts against
// the endpoints that hash a password.
//
// Strict on purpose, and the asymmetry with the figure above is the whole
// point: what is being rationed here is not bandwidth but 19 MiB of Argon2id
// working set per attempt (ADR 005). Ten a minute is generous for the only
// human who can log in — this product has exactly one admin account, so there
// is no fleet of users to accommodate — and it caps an unauthenticated
// stranger's ability to allocate memory on this server at a rate the process
// can absorb without leaving its published footprint.
const DefaultAuthRateLimitPerMinute = 10

// The bounds on the limiters' own memory.
//
// A map from address to counter is itself an attack surface: an attacker who
// rotates addresses makes it grow, and inside a single IPv6 /64 rotating is
// free. Bounding the map is what stops the defence from becoming the vector.
// When full, entries are evicted and precision is lost — never stability, the
// same trade ADR 008 makes for the sessions window.
//
// The ingest bound is the larger of the two because a browser SDK legitimately
// reports from as many addresses as it has visitors, and 16 384 entries is a
// few hundred kilobytes against a 30 MB footprint. Authentication has one
// legitimate user, so its map only ever needs to be big enough to hold the
// attackers.
const (
	MaxIngestIPEntries = 16384
	MaxAuthIPEntries   = 4096
)
