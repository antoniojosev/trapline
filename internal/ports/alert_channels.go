package ports

import "context"

// AlertChannel is one configured delivery destination, described in the terms
// anything outside the notification subsystem is allowed to know.
//
// Not the channel's configuration: no bot token, no webhook secret, no SMTP
// password. What a digest needs is "who asked for it" and what `doctor` needs
// is "where does it go and did the key open it", and neither is a reason to
// hand a credential to a report generator (ADR 015 keeps them encrypted at
// rest; a type that carried them through three packages would undo that in
// the one place nobody would look).
type AlertChannel struct {
	ID   int64
	Type string
	Name string
	// Digest is whether this channel asked for the weekly report. It is the
	// whole of the digest job's start condition: no channel with this set
	// means no job at all (ADR 014).
	Digest bool
	// Endpoint is where delivery goes, in a form something can connect to: a
	// URL for the four HTTP-shaped channels, host:port for SMTP. It is empty
	// when the configuration could not be decrypted, in which case
	// SecretError says why.
	Endpoint string
	// SecretError is why this channel's stored configuration could not be
	// read back — a missing key file, a key that is not the one it was
	// encrypted with, a truncated blob. Empty means the key decrypted what it
	// claims to decrypt, which is the half of `doctor`'s job that no
	// connectivity check can stand in for: a channel whose secret does not
	// open is a channel that will fail at the exact moment it is needed, and
	// nothing else in the product would notice until then.
	SecretError string
}

// AlertChannels is the seam between two features — the weekly digest and
// `doctor`'s channel checks — and the notification subsystem that owns
// channels, rules and the outbox (ADR 015).
//
// It is deliberately three methods wide. Everything about how a channel is
// configured, encrypted, formatted for and delivered to stays behind it; what
// crosses is a list, a probe target and a way to hand over a finished
// message. A server assembled without an implementation has no digest job and
// reports no channels, which is the truthful description of an installation
// that has not configured any (ADR 035).
type AlertChannels interface {
	// Channels lists every configured channel, decrypting each one's
	// configuration far enough to say where it delivers — and reporting,
	// rather than failing, when it cannot.
	Channels(ctx context.Context) ([]AlertChannel, error)
	// EnqueueDigest files a rendered digest for delivery to one channel.
	//
	// Enqueue and not send: delivery is the outbox's job, with its retries
	// and its backoff, and a weekly report that was lost because a chat API
	// was down for a moment would be noticed a week later by nobody (ADR 015).
	EnqueueDigest(ctx context.Context, channelID int64, subject, body string) error
}

// ChannelProber answers whether an endpoint is reachable, without sending
// anything to it.
//
// A port because reaching the network is an adapter's job, and a separate one
// from AlertChannels because the two answer to different owners: where a
// channel points is the notification subsystem's business, and whether this
// machine can get there is the same question for all five types.
type ChannelProber interface {
	// Probe connects to endpoint and disconnects. It must not send a message,
	// authenticate, or make any request a person would see: the whole value
	// of a channel check is that an operator can run it whenever they like,
	// and one that posted "test" to a team's chat would be run once.
	Probe(ctx context.Context, endpoint string) error
}
