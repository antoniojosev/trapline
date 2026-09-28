package domain

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// DSN is the one string a user copies into their SDK configuration, and the
// entire migration story of this product: point it here instead of at the
// incumbent and nothing else changes.
//
// Wire shape, fixed by the official SDKs (ADR 002):
//
//	{scheme}://{public_key}@{host}/{project_id}
//
// The SDK derives the ingest endpoint from it as
// {scheme}://{host}/api/{project_id}/envelope/.
type DSN struct {
	Scheme    string // http or https
	PublicKey string
	Host      string // host, optionally with :port
	ProjectID int64
}

// String renders the DSN in the form an SDK expects.
func (d DSN) String() string {
	return fmt.Sprintf("%s://%s@%s/%d", d.Scheme, d.PublicKey, d.Host, d.ProjectID)
}

// IngestURL is the envelope endpoint an SDK derives from this DSN. It is
// built here, next to the parser, so both directions of the protocol's one
// hard-coded path live in the same place.
func (d DSN) IngestURL() string {
	return fmt.Sprintf("%s://%s/api/%d/envelope/", d.Scheme, d.Host, d.ProjectID)
}

// ParseDSN reads a DSN string.
//
// This exists for the outbound direction — the CLI's doctor command sending
// a test event, and tests asserting round-trips. Inbound authentication does
// not parse DSNs: SDKs send the public key in an X-Sentry-Auth header and
// the project id in the path, never the assembled DSN.
func ParseDSN(raw string) (DSN, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return DSN{}, fmt.Errorf("%w: %w", ErrInvalidDSN, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return DSN{}, fmt.Errorf("%w: scheme must be http or https", ErrInvalidDSN)
	}
	if parsed.User == nil || parsed.User.Username() == "" {
		return DSN{}, fmt.Errorf("%w: missing public key", ErrInvalidDSN)
	}
	if _, hasPassword := parsed.User.Password(); hasPassword {
		// Ancient DSNs carried a secret key alongside the public one. It was
		// removed from the protocol years ago; accepting one would suggest a
		// secret is being transmitted and honoured, and neither is true.
		return DSN{}, fmt.Errorf("%w: secret keys are not supported", ErrInvalidDSN)
	}
	if parsed.Host == "" {
		return DSN{}, fmt.Errorf("%w: missing host", ErrInvalidDSN)
	}

	rawID := strings.Trim(parsed.Path, "/")
	if rawID == "" {
		return DSN{}, fmt.Errorf("%w: missing project id", ErrInvalidDSN)
	}
	projectID, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || projectID <= 0 {
		return DSN{}, fmt.Errorf("%w: project id must be a positive integer", ErrInvalidDSN)
	}

	return DSN{
		Scheme:    parsed.Scheme,
		PublicKey: parsed.User.Username(),
		Host:      parsed.Host,
		ProjectID: projectID,
	}, nil
}
