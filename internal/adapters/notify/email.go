package notify

import (
	"context"
	"crypto/tls"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

// DialFunc opens a connection to the SMTP relay.
//
// It exists because net/smtp takes a net.Conn and knows nothing about
// contexts, and because a test that needs a real mail server to prove a
// message was formatted correctly is a test nobody runs.
type DialFunc func(ctx context.Context, address string) (net.Conn, error)

// smtpDeadline bounds the whole SMTP conversation. net/smtp has no context, so
// the deadline goes on the connection, which is the only place it can go.
const smtpDeadline = DefaultTimeout

// sendEmail delivers through an SMTP relay.
//
// net/smtp rather than smtp.SendMail, for one reason: SendMail upgrades to TLS
// whenever the server advertises STARTTLS, and fails the delivery when the
// certificate does not verify. That turns a relay on localhost with a
// self-signed certificate — which is most of them — into a channel that never
// works, and the failure arrives during an incident. Here the upgrade is the
// operator's explicit choice, and when they ask for it, it is verified
// properly.
func (d *Dispatcher) sendEmail(ctx context.Context, config *domain.ChannelConfig, payload *domain.AlertPayload) error {
	address := net.JoinHostPort(config.Host, strconv.Itoa(config.Port))

	dial := d.dial
	if dial == nil {
		dial = func(ctx context.Context, address string) (net.Conn, error) {
			var dialer net.Dialer
			conn, err := dialer.DialContext(ctx, "tcp", address)
			if err != nil {
				return nil, fmt.Errorf("dialing %s: %w", address, err)
			}
			return conn, nil
		}
	}

	conn, err := dial(ctx, address)
	if err != nil {
		return fmt.Errorf("email: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(smtpDeadline))
	}

	client, err := smtp.NewClient(conn, config.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("email: greeting %s: %w", address, err)
	}
	// Close, not Quit: Quit is attempted below on the success path, and this
	// is the one that has to run when something went wrong halfway through.
	defer func() { _ = client.Close() }()

	if config.StartTLS {
		supported, _ := client.Extension("STARTTLS")
		if !supported {
			return fmt.Errorf("email: %s does not offer STARTTLS, and this channel requires it", address)
		}
		if err := client.StartTLS(&tls.Config{ServerName: config.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("email: starting TLS with %s: %w", address, err)
		}
	}

	if config.Username != "" {
		auth := smtp.PlainAuth("", config.Username, config.Password, config.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("email: authenticating to %s: %w", address, err)
		}
	}

	if err := client.Mail(config.From); err != nil {
		return fmt.Errorf("email: MAIL FROM %s: %w", config.From, err)
	}
	for _, recipient := range config.To {
		if err := client.Rcpt(recipient); err != nil {
			return fmt.Errorf("email: RCPT TO %s: %w", recipient, err)
		}
	}

	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("email: DATA: %w", err)
	}
	if _, err := writer.Write([]byte(renderMail(config, payload, d.now()))); err != nil {
		return fmt.Errorf("email: writing the message: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("email: finishing the message: %w", err)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("email: closing the session: %w", err)
	}
	return nil
}

// renderMail builds the message.
//
// Plain text, one part, no HTML alternative. An HTML mail would need a second
// renderer, a second thing to test and a set of rules about what a mail client
// will do to it; the content is four lines and a link.
func renderMail(config *domain.ChannelConfig, payload *domain.AlertPayload, now time.Time) string {
	var message strings.Builder

	message.WriteString("From: " + config.From + "\r\n")
	message.WriteString("To: " + strings.Join(config.To, ", ") + "\r\n")
	// Encoded because an issue title is whatever an exception said, in
	// whatever language the application speaks, and a raw non-ASCII byte in a
	// header is a message some relays reject and others mangle.
	message.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", payload.Headline()) + "\r\n")
	message.WriteString("Date: " + now.Format(time.RFC1123Z) + "\r\n")
	message.WriteString("MIME-Version: 1.0\r\n")
	message.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	// Declared, because the body genuinely is 8-bit: an issue title is
	// whatever an exception said, in whatever language the application speaks.
	message.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	// A stable header a mail client can thread on, so five alerts about one
	// issue land in one conversation instead of five.
	message.WriteString("X-Trapline-Event: " + string(payload.Event) + "\r\n")
	message.WriteString("\r\n")

	// SMTP ends a message with a lone dot on a line, so a body line that is
	// one is doubled. net/smtp's writer does this itself; it is repeated in
	// the line endings below because the headers above are ours to get right.
	message.WriteString(strings.ReplaceAll(payload.Text(), "\n", "\r\n"))
	message.WriteString("\r\n")
	return message.String()
}
