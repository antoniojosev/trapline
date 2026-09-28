package domain

import "errors"

// Sentinel errors the domain returns. Adapters map these onto their own
// vocabulary (HTTP status, CLI exit code) — they never leak storage or
// transport errors back into the domain.
var (
	// ErrInvalidProject means the project fails a domain rule (empty or
	// over-long name, bad id).
	ErrInvalidProject = errors.New("invalid project")
	// ErrInvalidDSN means a DSN string is not parseable or is missing a
	// required component.
	ErrInvalidDSN = errors.New("invalid dsn")
	// ErrInvalidOrigin means the configured public origin is unusable.
	ErrInvalidOrigin = errors.New("invalid origin")
	// ErrInvalidAdmin means an admin account fails a domain rule.
	ErrInvalidAdmin = errors.New("invalid admin")
	// ErrWeakPassword means a password does not meet the policy.
	ErrWeakPassword = errors.New("weak password")
	// ErrAdminNotFound means no admin matches the given identity.
	ErrAdminNotFound = errors.New("admin not found")
	// ErrInvalidCredentials means a username or password did not match. It
	// is deliberately one error for both cases: telling a caller which half
	// was wrong turns a login form into a username oracle.
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrSessionNotFound means no live session matches the token.
	ErrSessionNotFound = errors.New("session not found")
	// ErrSetupComplete means the one-time setup flow has already run.
	ErrSetupComplete = errors.New("setup already complete")
	// ErrInvalidToken means an API token fails a domain rule.
	ErrInvalidToken = errors.New("invalid token")
	// ErrTokenNotFound means no live token matches.
	ErrTokenNotFound = errors.New("token not found")
	// ErrForbidden means the credential is valid but lacks the scope.
	ErrForbidden = errors.New("forbidden")
	// ErrInvalidIssue means an issue fails a domain rule.
	ErrInvalidIssue = errors.New("invalid issue")
	// ErrIssueNotFound means no issue matches.
	ErrIssueNotFound = errors.New("issue not found")
	// ErrProjectNotFound means no project matches the given id.
	ErrProjectNotFound = errors.New("project not found")
	// ErrKeyNotFound means no active key matches the given public key.
	ErrKeyNotFound = errors.New("key not found")
	// ErrTooManyActiveKeys means a rotation would leave more active keys
	// than MaxActiveKeys allows.
	ErrTooManyActiveKeys = errors.New("too many active keys")
	// ErrInvalidRange means a stats query asks for a time range, a window or
	// a breakdown dimension that is not one the aggregates can answer.
	ErrInvalidRange = errors.New("invalid range")
	// ErrInvalidSearch means a search cannot be run as asked — every term was
	// too short for the index to look up. It is deliberately not "no results":
	// a query that never ran and a query that found nothing are different
	// answers, and returning the second for the first is how somebody spends
	// an afternoon looking for a bug in their own data.
	ErrInvalidSearch = errors.New("invalid search")
	// ErrInvalidRelease means a release, commit set or deploy fails a domain
	// rule.
	ErrInvalidRelease = errors.New("invalid release")
	// ErrReleaseNotFound means no release matches the given version.
	ErrReleaseNotFound = errors.New("release not found")
	// ErrInvalidAlert means an alert channel, rule or trigger fails a domain
	// rule.
	ErrInvalidAlert = errors.New("invalid alert")
	// ErrAlertNotFound means no channel, rule or notification matches.
	ErrAlertNotFound = errors.New("alert not found")
	// ErrInvalidSignature means a webhook signature does not verify. It is a
	// domain error because the verification is a domain function: the gate's
	// receiver and anyone following docs/alerts/webhooks.md run the same code.
	ErrInvalidSignature = errors.New("invalid signature")
	// ErrInvalidDigest means a weekly digest schedule names a day or an hour
	// that does not exist.
	ErrInvalidDigest = errors.New("invalid digest schedule")
	// ErrSettingNotFound means an installation setting has never been
	// written. Distinct from a setting written to nothing, because the caller
	// applies its default for one and must not for the other.
	ErrSettingNotFound = errors.New("setting not found")
	// ErrInvalidSetting means a setting's value is not the shape its key
	// requires.
	ErrInvalidSetting = errors.New("invalid setting")
	// ErrInvalidMonitor means a monitor of either family fails a domain
	// rule — a cron monitor's schedule, margin or check-in, or an uptime
	// monitor's name, method or interval. One sentinel for both because an
	// adapter maps it to one status and one exit code either way.
	ErrInvalidMonitor = errors.New("invalid monitor")
	// ErrMonitorNotFound means no monitor matches the given id, slug or ping
	// key.
	ErrMonitorNotFound = errors.New("monitor not found")
	// ErrInvalidTransaction means a transaction fails a domain rule — an
	// unusable duration, a sample rate that is not one, a resolution or a
	// ranking that does not exist.
	ErrInvalidTransaction = errors.New("invalid transaction")
	// ErrTraceNotFound means no sampled trace matches the given id.
	//
	// Distinct from "the transaction has no traces" on purpose: with sampling
	// on, most traces were never stored, and a reader who followed a trace id
	// from a log line needs to be able to tell "we did not keep this one"
	// from "this id is wrong" (ADR 021).
	ErrTraceNotFound = errors.New("trace not found")
	// ErrInvalidArtifact means an uploaded artefact fails a domain rule — no
	// url, an unknown kind, a name past the limit.
	ErrInvalidArtifact = errors.New("invalid artifact")
	// ErrArtifactNotFound means no stored artefact matches the debug id or
	// the URL a frame pointed at.
	//
	// It is an ordinary outcome on the ingest path and not a failure: most
	// events name a bundle nobody uploaded a map for, and symbolication that
	// treated that as an error would turn every such event into a dropped
	// one (ADR 018).
	ErrArtifactNotFound = errors.New("artifact not found")
	// ErrArtifactBudgetExceeded means storing this would put the project past
	// `artifacts_max_mb`.
	//
	// A sentinel of its own rather than a generic refusal, because the two
	// surfaces that raise it have to answer differently and only the error
	// knows which condition it is: the native API and the legacy file upload
	// answer 413, while the chunked path has to report it inside the assemble
	// body as {"state":"error","detail":…} — a 413 there reaches the user as
	// "unknown error" (from the recording).
	ErrArtifactBudgetExceeded = errors.New("artifact budget exceeded")

	// ErrInvalidSessionUpdate means a session update cannot be counted — no
	// session id, no project, no release, or no start time.
	//
	// Named for the update rather than the session, and kept apart from
	// ErrSessionNotFound above, because the two words mean unrelated things in
	// this product: that one is a panel login, this one is a run of somebody
	// else's application reporting its own health (ADR 008).
	ErrInvalidSessionUpdate = errors.New("invalid session update")
	// ErrMonitorDisabled means the monitor exists but is switched off.
	//
	// Distinct from not-found on purpose, and it is the whole reason a ping
	// to a disabled monitor answers 410 rather than 404: a backup script with
	// `&& curl .../ping/abc` at the end of its crontab line has no other way
	// to learn that somebody switched its monitor off, and a 404 would read
	// as a typo in a key that has not changed in a year (ADR 016).
	ErrMonitorDisabled = errors.New("monitor disabled")
)
