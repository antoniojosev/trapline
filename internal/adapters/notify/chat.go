package notify

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/antoniojosev/trapline/internal/domain"
)

// sendTelegram posts a message as a bot.
//
// The bot token is in the path, which is Telegram's design and not one this
// adapter can improve on. It is why the token is encrypted at rest and why
// redactURL exists: an error message quoting the full URL would put the token
// in the notification log, where it is readable by anyone with alerts:read.
func (d *Dispatcher) sendTelegram(ctx context.Context, config *domain.ChannelConfig, payload *domain.AlertPayload) error {
	endpoint := apiEndpoint(config.BaseURL, domain.TelegramAPIRoot, "/bot"+config.BotToken+"/sendMessage")
	body := map[string]any{
		"chat_id": config.ChatID,
		"text":    payload.Text(),
		// The link in the message is to this installation's own panel, and
		// Telegram fetching it to build a preview would be this server being
		// asked to render itself once per alert.
		"disable_web_page_preview": true,
	}
	if err := d.postJSON(ctx, endpoint, body, nil); err != nil {
		return fmt.Errorf("telegram: %w", err)
	}
	return nil
}

// sendSlack posts to an incoming webhook.
//
// Plain text rather than Block Kit. Blocks would render more prettily and
// would be a second message format to keep in step with the other four; the
// text a person needs — what broke, where, and a link — fits in a paragraph,
// and Slack linkifies the URL on its own.
func (d *Dispatcher) sendSlack(ctx context.Context, config *domain.ChannelConfig, payload *domain.AlertPayload) error {
	endpoint, err := webhookEndpoint(config, "slack")
	if err != nil {
		return err
	}
	if err := d.postJSON(ctx, endpoint, map[string]any{"text": payload.Text()}, nil); err != nil {
		return fmt.Errorf("slack: %w", err)
	}
	return nil
}

// sendDiscord posts to a channel webhook. Same shape as Slack with a
// different field name, which is the entire difference between them.
func (d *Dispatcher) sendDiscord(ctx context.Context, config *domain.ChannelConfig, payload *domain.AlertPayload) error {
	endpoint, err := webhookEndpoint(config, "discord")
	if err != nil {
		return err
	}
	if err := d.postJSON(ctx, endpoint, map[string]any{"content": payload.Text()}, nil); err != nil {
		return fmt.Errorf("discord: %w", err)
	}
	return nil
}

// webhookEndpoint resolves an incoming-webhook URL against an optional base.
//
// When a base URL is configured the path of the real webhook is kept and only
// its origin is replaced, so the request the gate's receiver sees has the same
// shape the provider would have seen — `/services/T000/B000/xxx` for Slack,
// `/api/webhooks/1/xxx` for Discord. A test that hit `/` instead would prove
// that this adapter can POST, which was never in doubt.
func webhookEndpoint(config *domain.ChannelConfig, provider string) (string, error) {
	parsed, err := url.Parse(config.URL)
	if err != nil {
		return "", fmt.Errorf("%s: url is not a URL: %w", provider, err)
	}
	if config.BaseURL == "" {
		return config.URL, nil
	}
	path := parsed.EscapedPath()
	if parsed.RawQuery != "" {
		path += "?" + parsed.RawQuery
	}
	return strings.TrimRight(config.BaseURL, "/") + path, nil
}
