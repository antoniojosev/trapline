package domain

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Origin is the scheme and host that SDKs will send events to — the public
// address of this installation.
//
// It is configuration, not something the server can infer: behind a reverse
// proxy the request it sees says "http://127.0.0.1:9000" while the DSN a user
// must copy says "https://errors.example.com". Guessing from request headers
// would mean the DSN shown in the panel depends on how the panel was reached,
// which is a support ticket waiting to happen.
type Origin struct {
	Scheme string
	Host   string
}

// ParseOrigin reads an origin from a configuration string such as
// "https://errors.example.com" or "http://localhost:9000".
func ParseOrigin(raw string) (Origin, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Origin{}, fmt.Errorf("%w: origin is required", ErrInvalidOrigin)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return Origin{}, fmt.Errorf("%w: %w", ErrInvalidOrigin, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return Origin{}, fmt.Errorf("%w: scheme must be http or https, got %q", ErrInvalidOrigin, parsed.Scheme)
	}
	if parsed.Host == "" {
		return Origin{}, fmt.Errorf("%w: missing host in %q", ErrInvalidOrigin, raw)
	}
	if trimmed := strings.Trim(parsed.Path, "/"); trimmed != "" {
		// A path here would end up inside every DSN, and SDKs build the
		// ingest URL from the host, so the path would be silently dropped.
		// Better to reject it than to hand out a DSN that quietly ignores it.
		return Origin{}, fmt.Errorf("%w: must not include a path, got %q", ErrInvalidOrigin, parsed.Path)
	}
	return Origin{Scheme: parsed.Scheme, Host: parsed.Host}, nil
}

// String renders the origin as it would appear in configuration.
func (o Origin) String() string {
	return o.Scheme + "://" + o.Host
}

// IssueURL is the panel's address for one issue.
//
// It lives here, next to DSNFor, because it is the same kind of fact: a public
// address this installation hands to somebody outside it, derivable only from
// the configured origin and never from a request. It is what turns a
// notification into something actionable — the difference between being told
// something broke and being able to look at it (ADR 015).
func (o Origin) IssueURL(projectID, issueID int64) string {
	return o.String() + "/projects/" + strconv.FormatInt(projectID, 10) +
		"/issues/" + strconv.FormatInt(issueID, 10)
}

// MonitorURL is the panel's address for one monitor, of either family.
//
// Beside IssueURL and for the same reason: an alert that says a backup has
// not run, or that a service is down, is only actionable if it also says
// where to go and look — otherwise it is an interruption rather than a
// notification (ADR 015, ADR 016).
//
// The kind is part of the path and not only of the id because the two
// families number their monitors from separate tables: without it the link in
// a cron alert and the link in an uptime alert can be the same URL pointing
// at two different things.
func (o Origin) MonitorURL(projectID int64, kind MonitorKind, monitorID int64) string {
	return o.String() + "/projects/" + strconv.FormatInt(projectID, 10) +
		"/monitors/" + string(kind) + "/" + strconv.FormatInt(monitorID, 10)
}

// IsLocal reports whether the origin still points at this machine, which
// almost always means it was never configured.
//
// It matters wherever this product hands somebody a URL to use elsewhere: a
// DSN pasted into an SDK on another host, a link inside an email that leaves
// the building. Both are unreachable, and both fail in the same quiet way —
// everything looks configured and nothing arrives. The rule lives here, on
// the type, so the three callers that need it cannot each keep their own list
// of what "local" means.
func (o Origin) IsLocal() bool {
	host := o.Host
	// Strip the port, taking care not to cut an IPv6 literal in half.
	if colon := strings.LastIndex(host, ":"); colon > 0 && !strings.Contains(host[colon:], "]") {
		host = host[:colon]
	}
	host = strings.Trim(host, "[]")
	switch host {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0", "":
		return true
	default:
		return false
	}
}

// DSNFor builds the DSN a user copies into an SDK for one key.
func (o Origin) DSNFor(key Key) DSN {
	return DSN{
		Scheme:    o.Scheme,
		PublicKey: key.PublicKey,
		Host:      o.Host,
		ProjectID: key.ProjectID,
	}
}
