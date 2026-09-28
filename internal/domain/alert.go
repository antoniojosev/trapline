package domain

import (
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// A channel is where a notification goes, and the five kinds v1 speaks.
//
// The list is closed on purpose. Every one of them is a different wire format
// and a different failure mode, and "any HTTP endpoint" is already covered by
// the webhook — with a signature, which is the part a generic integration
// actually needs.
const (
	// ChannelTelegram posts through a bot to a chat.
	ChannelTelegram ChannelType = "telegram"
	// ChannelSlack posts to an incoming webhook.
	ChannelSlack ChannelType = "slack"
	// ChannelDiscord posts to a channel webhook.
	ChannelDiscord ChannelType = "discord"
	// ChannelWebhook posts a signed JSON body to an arbitrary URL.
	ChannelWebhook ChannelType = "webhook"
	// ChannelEmail sends mail through an SMTP relay.
	ChannelEmail ChannelType = "email"
)

// ChannelType names one delivery mechanism.
type ChannelType string

// AllChannelTypes lists every channel type, for validation and for telling
// somebody who mistyped one what the alternatives are.
func AllChannelTypes() []ChannelType {
	return []ChannelType{ChannelTelegram, ChannelSlack, ChannelDiscord, ChannelWebhook, ChannelEmail}
}

// Valid reports whether t is a channel type this build can deliver through.
func (t ChannelType) Valid() bool { return slices.Contains(AllChannelTypes(), t) }

// MaxChannelName bounds a channel's display name.
const MaxChannelName = 80

// ChannelConfig is everything needed to deliver through one channel.
//
// One struct with the union of the five shapes rather than five types behind
// an interface. The alternative was a `map[string]any` — which cannot be
// validated at the edge and turns every adapter into a cast — or a sealed
// interface, which buys type safety at the cost of a JSON encoding that has
// to name its own variant anyway. This is validated per type by Validate, so
// a Slack channel carrying a bot token is rejected at the door instead of
// discovered at delivery time.
//
// The whole struct is encrypted at rest (ADR 015): several of these fields
// are credentials, and a bot token in a plain column is a bot token in every
// backup and every `.dump`.
type ChannelConfig struct {
	// BotToken authenticates a Telegram bot.
	BotToken string `json:"bot_token,omitempty"`
	// ChatID is the Telegram chat the bot posts into.
	ChatID string `json:"chat_id,omitempty"`
	// URL is where the message is posted: the incoming-webhook URL for Slack
	// and Discord, the endpoint for a signed webhook. One field rather than
	// two named for the same thing, because "where does this post" has one
	// answer per channel and a second field would only be a second place to
	// put it in.
	URL string `json:"url,omitempty"`
	// Secret signs the webhook body (see SignWebhook).
	Secret string `json:"secret,omitempty"`
	// BaseURL replaces the provider's API root. It exists so the delivery
	// gate can drive Telegram, Slack and Discord against a local receiver
	// that imitates their routes, and it is documented as a testing seam
	// rather than a feature (ADR 015). Production leaves it empty.
	BaseURL string `json:"base_url,omitempty"`

	// Host and Port address the SMTP relay.
	Host string `json:"host,omitempty"`
	// Port is the SMTP port.
	Port int `json:"port,omitempty"`
	// Username and Password authenticate to the relay. Both optional: a relay
	// on localhost usually wants neither.
	Username string `json:"username,omitempty"`
	// Password is the SMTP password.
	Password string `json:"password,omitempty"`
	// From is the envelope sender and the From header.
	From string `json:"from,omitempty"`
	// To is who receives the mail. At least one.
	To []string `json:"to,omitempty"`
	// StartTLS upgrades the SMTP session. Opt-in rather than automatic: a
	// relay advertising STARTTLS with a certificate this machine cannot
	// verify would otherwise turn every notification into a delivery failure
	// at the moment one is most needed.
	StartTLS bool `json:"starttls,omitempty"`
}

// Validate checks a configuration against the rules of its own type.
//
// Fields belonging to another type are rejected rather than ignored: a Slack
// channel that carries a bot token was configured by somebody who believed
// something false, and silently dropping the field would leave them believing
// it.
func (c *ChannelConfig) Validate(channelType ChannelType) error {
	switch channelType {
	case ChannelTelegram:
		if strings.TrimSpace(c.BotToken) == "" {
			return fmt.Errorf("%w: a telegram channel needs bot_token", ErrInvalidAlert)
		}
		if strings.TrimSpace(c.ChatID) == "" {
			return fmt.Errorf("%w: a telegram channel needs chat_id", ErrInvalidAlert)
		}
		return validateOptionalHTTPURL("base_url", c.BaseURL)

	case ChannelSlack, ChannelDiscord:
		if err := validateHTTPURL("url", c.URL); err != nil {
			return err
		}
		return validateOptionalHTTPURL("base_url", c.BaseURL)

	case ChannelWebhook:
		if err := validateHTTPURL("url", c.URL); err != nil {
			return err
		}
		if len(c.Secret) < MinWebhookSecret {
			return fmt.Errorf("%w: a webhook needs a secret of at least %d characters; "+
				"an unsigned webhook is an endpoint anyone can post to",
				ErrInvalidAlert, MinWebhookSecret)
		}
		return nil

	case ChannelEmail:
		if strings.TrimSpace(c.Host) == "" {
			return fmt.Errorf("%w: an email channel needs host", ErrInvalidAlert)
		}
		if c.Port <= 0 || c.Port > 65535 {
			return fmt.Errorf("%w: smtp port %d is not a port", ErrInvalidAlert, c.Port)
		}
		if strings.TrimSpace(c.From) == "" {
			return fmt.Errorf("%w: an email channel needs from", ErrInvalidAlert)
		}
		if len(c.To) == 0 {
			return fmt.Errorf("%w: an email channel needs at least one recipient", ErrInvalidAlert)
		}
		for _, recipient := range c.To {
			if !strings.Contains(recipient, "@") {
				return fmt.Errorf("%w: %q is not an email address", ErrInvalidAlert, recipient)
			}
		}
		return nil

	default:
		return unknownChannelType(string(channelType))
	}
}

// MinWebhookSecret is the shortest signing secret a webhook channel accepts.
// Short enough not to be ceremony, long enough that the HMAC is not the weak
// part of the arrangement.
const MinWebhookSecret = 16

func unknownChannelType(name string) error {
	known := make([]string, 0, len(AllChannelTypes()))
	for _, channelType := range AllChannelTypes() {
		known = append(known, string(channelType))
	}
	return fmt.Errorf("%w: unknown channel type %q, expected one of %s",
		ErrInvalidAlert, name, strings.Join(known, ", "))
}

func validateHTTPURL(field, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("%w: %s is required", ErrInvalidAlert, field)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: %s is not a URL: %w", ErrInvalidAlert, field, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("%w: %s must be http or https, got %q", ErrInvalidAlert, field, parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("%w: %s has no host", ErrInvalidAlert, field)
	}
	return nil
}

func validateOptionalHTTPURL(field, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return validateHTTPURL(field, raw)
}

// AlertChannel is a configured destination.
type AlertChannel struct {
	// ID is the storage identity, zero before it is saved.
	ID int64
	// Type decides which adapter delivers it.
	Type ChannelType
	// Name is what an operator recognises it by.
	Name string
	// Config carries the credentials, and is encrypted at rest.
	Config ChannelConfig
	// Digest marks a channel that also receives the weekly summary.
	Digest bool
	// CreatedAt is when it was configured.
	CreatedAt time.Time
}

// NewAlertChannel validates a channel before it can be stored.
func NewAlertChannel(
	channelType ChannelType, name string, config ChannelConfig, digest bool, now time.Time,
) (AlertChannel, error) {
	if !channelType.Valid() {
		return AlertChannel{}, unknownChannelType(string(channelType))
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return AlertChannel{}, fmt.Errorf("%w: a channel needs a name", ErrInvalidAlert)
	}
	if len(name) > MaxChannelName {
		return AlertChannel{}, fmt.Errorf("%w: name exceeds %d characters", ErrInvalidAlert, MaxChannelName)
	}
	if err := config.Validate(channelType); err != nil {
		return AlertChannel{}, err
	}
	return AlertChannel{
		Type:      channelType,
		Name:      name,
		Config:    config,
		Digest:    digest,
		CreatedAt: now.UTC(),
	}, nil
}

// Redacted is the channel as the API shows it: everything except the secrets.
//
// A channel is readable by anyone holding alerts:read, and a listing that
// echoed back bot tokens would make the encryption at rest pointless — the
// credential would simply leak through the front door instead.
func (c *AlertChannel) Redacted() ChannelConfig {
	visible := ChannelConfig{
		ChatID:   c.Config.ChatID,
		BaseURL:  c.Config.BaseURL,
		Host:     c.Config.Host,
		Port:     c.Config.Port,
		Username: c.Config.Username,
		From:     c.Config.From,
		To:       slices.Clone(c.Config.To),
		StartTLS: c.Config.StartTLS,
	}
	// A URL is not a secret in the same way a token is — an operator has to be
	// able to see which endpoint a channel points at to know what they
	// configured — but for Slack and Discord the *path* of an incoming webhook
	// IS the credential, so only its origin survives. The endpoint of a signed
	// webhook is not a credential: it is authenticated by the signature, which
	// is exactly the difference the signing requirement buys.
	switch c.Type {
	case ChannelSlack, ChannelDiscord:
		visible.URL = originOf(c.Config.URL)
	default:
		visible.URL = c.Config.URL
	}
	return visible
}

// TelegramAPIRoot is Telegram's Bot API, and the endpoint a telegram channel
// connects to when it has no BaseURL of its own.
//
// Here rather than in the delivery adapter because two things now need it:
// the adapter that posts a message, and the description of where a channel
// delivers that `doctor` probes. A constant duplicated in both is a constant
// that will disagree with itself the day Telegram moves.
const TelegramAPIRoot = "https://api.telegram.org"

// Endpoint is where this channel delivers, in a form something can connect to
// — and with nothing secret left in it.
//
// The two halves of that sentence pull in opposite directions and the second
// one wins. Telegram puts the bot token in the path and an incoming webhook's
// path *is* the credential for Slack and Discord, so the full delivery URL of
// three of the five channel types is a secret. This is read by
// `GET /system/channels`, which is guarded by projects:read — a weaker scope
// than the alerts:read that guards the channel listing — so returning the
// whole URL here would hand a dashboard token the credential that the listing
// deliberately redacts (Redacted).
//
// What survives is exactly what a connectivity probe needs: a scheme, a host
// and a port. That is the whole of "does this name resolve, does that port
// accept, is the certificate good for it", which is all channelprobe claims
// to check.
func (c *AlertChannel) Endpoint() string {
	switch c.Type {
	case ChannelTelegram:
		root := c.Config.BaseURL
		if root == "" {
			root = TelegramAPIRoot
		}
		return originOnly(root)
	case ChannelSlack, ChannelDiscord:
		// The base URL is what delivery actually resolves against when one is
		// configured (see notify.webhookEndpoint), so probing the provider
		// while sending somewhere else would check the wrong host.
		if c.Config.BaseURL != "" {
			return originOnly(c.Config.BaseURL)
		}
		return originOnly(c.Config.URL)
	case ChannelWebhook:
		// The one URL that is not a credential: a signed webhook is
		// authenticated by its HMAC, which is precisely what the signing
		// requirement buys. It is returned whole because the path is the most
		// useful thing an operator can be shown about their own endpoint.
		return c.Config.URL
	case ChannelEmail:
		if c.Config.Host == "" {
			return ""
		}
		return net.JoinHostPort(c.Config.Host, strconv.Itoa(c.Config.Port))
	default:
		return ""
	}
}

// originOnly keeps the scheme, host and port of a URL and drops everything
// that could carry a secret.
func originOnly(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

// originOf keeps the scheme and host of a URL and drops the rest.
func originOf(raw string) string {
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host + "/…"
}
