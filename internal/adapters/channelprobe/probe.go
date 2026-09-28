// Package channelprobe answers one question about an alert channel: can this
// machine reach it.
//
// It answers it without sending anything. That constraint is the whole design:
// a check that posted a test message to a team's chat would be run once and
// then avoided, which makes it worthless on the day it matters — and the day
// it matters is the day something else is already broken and somebody needs to
// know the alarm will actually ring.
//
// So the probe stops at the layer below the message: resolve the name, open
// the connection, complete the TLS handshake if the endpoint is TLS, close.
// That covers the failures a misconfigured channel actually has — a hostname
// that does not resolve, a port nobody is listening on, a firewall, an expired
// or untrusted certificate, a proxy that refuses — and none of them can be
// discovered any other way until delivery is attempted for real.
//
// What it cannot tell you is whether the credential is right; a bot token is
// only proven by using it. `doctor` says which of the two it checked, because
// a check that overstates itself is worse than one that is absent.
package channelprobe

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/antoniojosev/trapline/internal/ports"
)

var _ ports.ChannelProber = (*Prober)(nil)

// DefaultTimeout bounds one probe.
//
// Five seconds, and it is a ceiling rather than a target: `doctor` probes
// every channel an installation has, one after another, and a person is
// waiting at a terminal for the result. A channel that needs longer than this
// to accept a TCP connection is a channel that will time out under an alert
// storm too.
const DefaultTimeout = 5 * time.Second

// Prober opens and closes connections.
type Prober struct {
	timeout time.Duration
	// dialer is held rather than built per probe so a test can point every
	// lookup at a listener it controls.
	dialer *net.Dialer
}

// New builds a prober with the default timeout.
func New() *Prober {
	return &Prober{timeout: DefaultTimeout, dialer: &net.Dialer{Timeout: DefaultTimeout}}
}

// WithTimeout returns a prober that gives up sooner.
func (p *Prober) WithTimeout(timeout time.Duration) *Prober {
	if timeout <= 0 {
		return p
	}
	return &Prober{timeout: timeout, dialer: &net.Dialer{Timeout: timeout}}
}

// Probe connects to an endpoint and disconnects.
//
// The endpoint is either a URL — which is what the four HTTP-shaped channels
// deliver to — or a host and port, which is what SMTP is configured as. Both
// are accepted because both are what the channel actually stores, and asking
// the notification subsystem to normalise them into one shape would be asking
// it to invent a scheme for a mail server.
func (p *Prober) Probe(ctx context.Context, endpoint string) error {
	address, serverName, err := target(endpoint)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	connection, err := p.dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", address, err)
	}
	defer func() { _ = connection.Close() }()

	if serverName == "" {
		return nil
	}

	// The handshake, and nothing after it. It proves the certificate chain
	// and the name, which is the failure a channel configured a year ago
	// actually hits, and it sends no application data of any kind.
	secured := tls.Client(connection, &tls.Config{
		ServerName: serverName,
		MinVersion: tls.VersionTLS12,
	})
	defer func() { _ = secured.Close() }()

	if err := secured.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("cannot complete the TLS handshake with %s: %w", serverName, err)
	}
	return nil
}

// target reduces an endpoint to an address to dial and, when the connection
// is TLS, the name the certificate has to match.
func target(endpoint string) (address, serverName string, err error) {
	trimmed := strings.TrimSpace(endpoint)
	if trimmed == "" {
		return "", "", errors.New("the channel has no endpoint to probe")
	}

	if !strings.Contains(trimmed, "://") {
		// A bare host and port: how an SMTP server is configured. The port is
		// required rather than guessed, because guessing between 25, 465 and
		// 587 would report a healthy channel for a server listening on the
		// other one.
		host, port, splitErr := net.SplitHostPort(trimmed)
		if splitErr != nil {
			return "", "", fmt.Errorf(
				"%q is neither a URL nor a host and port: %w", endpoint, splitErr)
		}
		if host == "" {
			return "", "", fmt.Errorf("%q names a port but no host", endpoint)
		}
		return net.JoinHostPort(host, port), "", nil
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", "", fmt.Errorf("%q is not a URL: %w", endpoint, err)
	}
	if parsed.Hostname() == "" {
		return "", "", fmt.Errorf("%q has no host", endpoint)
	}

	port := parsed.Port()
	switch {
	case port != "":
	case parsed.Scheme == "https":
		port = "443"
	case parsed.Scheme == "http":
		port = "80"
	default:
		return "", "", fmt.Errorf(
			"%q uses the scheme %q, which has no default port; give the endpoint a port",
			endpoint, parsed.Scheme)
	}

	address = net.JoinHostPort(parsed.Hostname(), port)
	if parsed.Scheme == "https" {
		serverName = parsed.Hostname()
	}
	return address, serverName, nil
}
