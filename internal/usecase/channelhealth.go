package usecase

import (
	"context"

	"github.com/antoniojosev/trapline/internal/ports"
)

// ChannelHealth answers the question an operator only ever asks too late:
// will the alarm ring.
//
// A misconfigured alert channel is invisible. Everything else about the
// installation looks healthy, the rules exist, the panel shows them — and the
// first delivery attempt happens on the day something is already on fire,
// which is the worst possible moment to discover that the bot token was
// rotated in March or that the SMTP host no longer resolves.
//
// So this checks both halves, and reports them separately because they fail
// for different reasons and are fixed by different people: whether the
// secrets key still opens the stored configuration, and whether this machine
// can reach where that configuration points. Neither check sends a message.
type ChannelHealth struct {
	channels ports.AlertChannels
	prober   ports.ChannelProber
}

// NewChannelHealth wires the checker. Either dependency may be nil, and an
// installation with no notification subsystem assembled is exactly that case:
// it reports that nothing is configured rather than failing, because "no
// channels" is a legitimate state and not an error.
func NewChannelHealth(channels ports.AlertChannels, prober ports.ChannelProber) *ChannelHealth {
	return &ChannelHealth{channels: channels, prober: prober}
}

// ChannelStatus is one channel's diagnosis.
type ChannelStatus struct {
	ID   int64
	Type string
	Name string
	// Digest is whether this channel asked for the weekly report. Reported
	// here because "the digest job is not running" and "no channel asked for
	// it" are the same fact seen from two ends, and an operator looking for
	// the missing job should find the answer in the same output.
	Digest bool
	// Endpoint is where delivery goes, empty when the configuration could not
	// be decrypted.
	Endpoint string
	// Secret is whether the secrets key decrypted this channel's stored
	// configuration — that is, whether the key is the key it claims to be.
	Secret bool
	// Reachable is whether a connection to Endpoint could be opened. False
	// when Secret is false, because there was nowhere to try.
	Reachable bool
	// Detail is the one sentence explaining the first thing that is wrong, or
	// what was verified when nothing is.
	Detail string
}

// OK reports whether this channel would work if something needed it now, as
// far as anything short of sending can tell.
func (s ChannelStatus) OK() bool { return s.Secret && s.Reachable }

// Configured reports whether this installation has a notification subsystem
// at all.
//
// Distinct from "has no channels": a server assembled without the subsystem
// and a server with the subsystem and nothing in it are different situations,
// and only the second is something an operator can fix from the panel.
func (h *ChannelHealth) Configured() bool { return h.channels != nil }

// Check diagnoses every configured channel.
func (h *ChannelHealth) Check(ctx context.Context) ([]ChannelStatus, error) {
	if h.channels == nil {
		return nil, nil
	}
	channels, err := h.channels.Channels(ctx)
	if err != nil {
		return nil, err
	}

	statuses := make([]ChannelStatus, 0, len(channels))
	for _, channel := range channels {
		statuses = append(statuses, h.check(ctx, channel))
	}
	return statuses, nil
}

// check is one channel.
//
// The secret first, and it short-circuits. A channel whose configuration
// cannot be decrypted has no endpoint to probe, and reporting "unreachable"
// for it would send whoever reads it looking at their firewall for a problem
// that is a missing key file.
func (h *ChannelHealth) check(ctx context.Context, channel ports.AlertChannel) ChannelStatus {
	status := ChannelStatus{
		ID:       channel.ID,
		Type:     channel.Type,
		Name:     channel.Name,
		Digest:   channel.Digest,
		Endpoint: channel.Endpoint,
	}

	if channel.SecretError != "" {
		status.Detail = "its stored configuration could not be decrypted: " + channel.SecretError
		return status
	}
	status.Secret = true

	if h.prober == nil {
		// Nothing to probe with. Said plainly rather than reported as
		// reachable: a check that claims a verification it did not perform is
		// worse than one that admits it was skipped.
		status.Detail = "the secret decrypts; connectivity was not checked, because this build has no prober"
		return status
	}
	if err := h.prober.Probe(ctx, channel.Endpoint); err != nil {
		status.Detail = err.Error()
		return status
	}

	status.Reachable = true
	status.Detail = "the secret decrypts and " + channel.Endpoint + " accepts connections; nothing was sent"
	return status
}
