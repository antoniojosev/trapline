package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/antoniojosev/trapline/internal/domain"
)

func telegramConfig() domain.ChannelConfig {
	return domain.ChannelConfig{BotToken: "123:abc", ChatID: "-100200"}
}

func webhookConfig() domain.ChannelConfig {
	return domain.ChannelConfig{URL: "https://example.test/hook", Secret: "a-secret-long-enough"}
}

func TestChannelTypesAreClosed(t *testing.T) {
	for _, valid := range domain.AllChannelTypes() {
		if !valid.Valid() {
			t.Errorf("%q is listed but does not validate", valid)
		}
	}
	if domain.ChannelType("sms").Valid() {
		t.Error("an unimplemented channel type validated; a channel nothing can deliver is worse than a rejected one")
	}
}

func TestChannelConfigIsValidatedAgainstItsOwnType(t *testing.T) {
	cases := []struct {
		name        string
		channelType domain.ChannelType
		config      domain.ChannelConfig
		wantErr     bool
		mentions    string
	}{
		{name: "telegram needs a token", channelType: domain.ChannelTelegram,
			config: domain.ChannelConfig{ChatID: "1"}, wantErr: true, mentions: "bot_token"},
		{name: "telegram needs a chat", channelType: domain.ChannelTelegram,
			config: domain.ChannelConfig{BotToken: "t"}, wantErr: true, mentions: "chat_id"},
		{name: "telegram complete", channelType: domain.ChannelTelegram, config: telegramConfig()},
		{name: "telegram with a bad base url", channelType: domain.ChannelTelegram,
			config:  domain.ChannelConfig{BotToken: "t", ChatID: "1", BaseURL: "ftp://x/"},
			wantErr: true, mentions: "base_url"},

		{name: "slack needs a webhook", channelType: domain.ChannelSlack,
			config: domain.ChannelConfig{}, wantErr: true, mentions: "url"},
		{name: "slack rejects a non-http scheme", channelType: domain.ChannelSlack,
			config: domain.ChannelConfig{URL: "file:///etc/passwd"}, wantErr: true, mentions: "http"},
		{name: "slack ok", channelType: domain.ChannelSlack,
			config: domain.ChannelConfig{URL: "https://hooks.slack.test/services/A/B/C"}},
		{name: "discord ok", channelType: domain.ChannelDiscord,
			config: domain.ChannelConfig{URL: "https://discord.test/api/webhooks/1/x"}},

		{name: "webhook needs a url", channelType: domain.ChannelWebhook,
			config: domain.ChannelConfig{Secret: "a-secret-long-enough"}, wantErr: true, mentions: "url"},
		// The rule that matters most here: an unsigned webhook is an endpoint
		// anybody who learns the URL can post anything to.
		{name: "webhook refuses a short secret", channelType: domain.ChannelWebhook,
			config:  domain.ChannelConfig{URL: "https://example.test/h", Secret: "short"},
			wantErr: true, mentions: "secret"},
		{name: "webhook refuses no secret at all", channelType: domain.ChannelWebhook,
			config: domain.ChannelConfig{URL: "https://example.test/h"}, wantErr: true, mentions: "secret"},
		{name: "webhook ok", channelType: domain.ChannelWebhook, config: webhookConfig()},

		{name: "email needs a host", channelType: domain.ChannelEmail,
			config:  domain.ChannelConfig{Port: 25, From: "a@b.test", To: []string{"c@d.test"}},
			wantErr: true, mentions: "host"},
		{name: "email refuses a port that is not one", channelType: domain.ChannelEmail,
			config:  domain.ChannelConfig{Host: "mail", Port: 70000, From: "a@b.test", To: []string{"c@d.test"}},
			wantErr: true, mentions: "port"},
		{name: "email needs a sender", channelType: domain.ChannelEmail,
			config:  domain.ChannelConfig{Host: "mail", Port: 25, To: []string{"c@d.test"}},
			wantErr: true, mentions: "from"},
		{name: "email needs a recipient", channelType: domain.ChannelEmail,
			config:  domain.ChannelConfig{Host: "mail", Port: 25, From: "a@b.test"},
			wantErr: true, mentions: "recipient"},
		{name: "email checks the recipients", channelType: domain.ChannelEmail,
			config:  domain.ChannelConfig{Host: "mail", Port: 25, From: "a@b.test", To: []string{"not-an-address"}},
			wantErr: true, mentions: "not-an-address"},
		{name: "email ok", channelType: domain.ChannelEmail,
			config: domain.ChannelConfig{Host: "mail", Port: 1025, From: "a@b.test", To: []string{"c@d.test"}}},

		{name: "an unknown type", channelType: domain.ChannelType("carrier-pigeon"),
			config: domain.ChannelConfig{}, wantErr: true, mentions: "carrier-pigeon"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.config.Validate(testCase.channelType)
			switch {
			case testCase.wantErr && err == nil:
				t.Fatal("the configuration was accepted; it should not have been")
			case !testCase.wantErr && err != nil:
				t.Fatalf("the configuration was rejected: %v", err)
			case err == nil:
				return
			}
			if !errors.Is(err, domain.ErrInvalidAlert) {
				t.Errorf("error %v does not wrap ErrInvalidAlert, so the API cannot map it to a 400", err)
			}
			if testCase.mentions != "" && !strings.Contains(err.Error(), testCase.mentions) {
				t.Errorf("error %q does not mention %q, so it does not say what to fix", err, testCase.mentions)
			}
		})
	}
}

func TestNewAlertChannelValidates(t *testing.T) {
	now := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)

	if _, err := domain.NewAlertChannel(domain.ChannelTelegram, "   ", telegramConfig(), false, now); err == nil {
		t.Error("a channel with no name was accepted")
	}
	if _, err := domain.NewAlertChannel(domain.ChannelTelegram,
		strings.Repeat("x", domain.MaxChannelName+1), telegramConfig(), false, now); err == nil {
		t.Error("an over-long name was accepted")
	}
	if _, err := domain.NewAlertChannel(domain.ChannelType("nope"), "n", telegramConfig(), false, now); err == nil {
		t.Error("an unknown type was accepted")
	}

	channel, err := domain.NewAlertChannel(domain.ChannelTelegram, "  ops  ", telegramConfig(), true, now.Local())
	if err != nil {
		t.Fatalf("a valid channel was rejected: %v", err)
	}
	if channel.Name != "ops" {
		t.Errorf("the name is %q, want it trimmed to \"ops\"", channel.Name)
	}
	if !channel.Digest {
		t.Error("the digest flag was dropped")
	}
	if channel.CreatedAt.Location() != time.UTC {
		t.Error("the timestamp was not normalised to UTC")
	}
}

// TestRedactedNeverEchoesACredential is the test that keeps encryption at rest
// meaningful. A token that goes in and comes back out through the API would
// leak through the front door instead of the file.
func TestRedactedNeverEchoesACredential(t *testing.T) {
	secrets := []struct {
		name        string
		channelType domain.ChannelType
		config      domain.ChannelConfig
		secret      string
	}{
		{"telegram bot token", domain.ChannelTelegram, telegramConfig(), "123:abc"},
		{"slack webhook path", domain.ChannelSlack,
			domain.ChannelConfig{URL: "https://hooks.slack.test/services/A/B/SECRETPATH"}, "SECRETPATH"},
		{"discord webhook path", domain.ChannelDiscord,
			domain.ChannelConfig{URL: "https://discord.test/api/webhooks/1/SECRETPATH"}, "SECRETPATH"},
		{"webhook signing secret", domain.ChannelWebhook, webhookConfig(), "a-secret-long-enough"},
		{"smtp password", domain.ChannelEmail, domain.ChannelConfig{
			Host: "mail", Port: 25, From: "a@b.test", To: []string{"c@d.test"},
			Username: "alerts", Password: "hunter2",
		}, "hunter2"},
	}

	for _, testCase := range secrets {
		t.Run(testCase.name, func(t *testing.T) {
			channel := domain.AlertChannel{Type: testCase.channelType, Config: testCase.config}
			redacted := channel.Redacted()
			if strings.Contains(rendered(redacted), testCase.secret) {
				t.Errorf("the redacted configuration still contains %q", testCase.secret)
			}
		})
	}
}

// rendered is every string field of a configuration, concatenated, so a new
// field that leaks a secret fails this test instead of being forgotten.
func rendered(config domain.ChannelConfig) string {
	return strings.Join([]string{
		config.BotToken, config.ChatID, config.URL, config.URL, config.Secret,
		config.BaseURL, config.Host, config.Username, config.Password, config.From,
		strings.Join(config.To, ","),
	}, "|")
}

func TestRedactedKeepsWhatAnOperatorNeedsToSee(t *testing.T) {
	channel := domain.AlertChannel{Type: domain.ChannelEmail, Config: domain.ChannelConfig{
		Host: "mail.example.test", Port: 587, From: "alerts@example.test",
		To: []string{"ops@example.test"}, Username: "alerts", Password: "hunter2", StartTLS: true,
	}}
	redacted := channel.Redacted()
	if redacted.Host != "mail.example.test" || redacted.Port != 587 {
		t.Error("the relay was redacted; an operator cannot tell what they configured")
	}
	if len(redacted.To) != 1 || redacted.To[0] != "ops@example.test" {
		t.Error("the recipients were redacted; they are the point of the channel")
	}
	if !redacted.StartTLS {
		t.Error("the TLS setting was dropped")
	}
}

func TestRedactedHandlesAMalformedWebhookURL(t *testing.T) {
	channel := domain.AlertChannel{Type: domain.ChannelSlack, Config: domain.ChannelConfig{URL: "::not a url"}}
	if origin := channel.Redacted().URL; origin != "" {
		t.Errorf("a URL that does not parse redacted to %q, want the empty string", origin)
	}
}

func TestValidateRejectsAURLThatDoesNotParse(t *testing.T) {
	config := domain.ChannelConfig{URL: "http://[::1"}
	err := config.Validate(domain.ChannelSlack)
	if err == nil {
		t.Fatal("a url that is not a URL was accepted")
	}
	if !strings.Contains(err.Error(), "url") {
		t.Errorf("error %q does not name the field", err)
	}
}

func TestValidateRejectsAURLWithNoHost(t *testing.T) {
	config := domain.ChannelConfig{URL: "https:///hook", Secret: "a-secret-long-enough"}
	err := config.Validate(domain.ChannelWebhook)
	if err == nil || !strings.Contains(err.Error(), "no host") {
		t.Fatalf("a URL with no host was accepted or misreported: %v", err)
	}
}

// TestIssueURLIsTheLinkInEveryNotification pins the shape the panel routes on:
// the address is derived from the configured origin, never from a request, for
// the same reason a DSN is.
func TestIssueURLIsTheLinkInEveryNotification(t *testing.T) {
	origin, err := domain.ParseOrigin("https://errors.example.test")
	if err != nil {
		t.Fatalf("the fixture origin does not parse: %v", err)
	}
	if got := origin.IssueURL(2, 14); got != "https://errors.example.test/projects/2/issues/14" {
		t.Errorf("issue URL is %q", got)
	}
}

// TestEndpointIsAConnectTargetAndNeverACredential is the security half of
// ADR 035's seam.
//
// `Endpoint` is read back by `GET /system/channels`, which is guarded by
// projects:read — weaker than the alerts:read that guards the channel listing
// — and for three of the five types the delivery URL *is* the credential:
// Telegram puts the bot token in the path, and the path of a Slack or Discord
// incoming webhook is the whole of its authentication. What a probe needs is
// a scheme, a host and a port, and that is all this may hand out.
func TestEndpointIsAConnectTargetAndNeverACredential(t *testing.T) {
	for name, one := range map[string]struct {
		channel domain.AlertChannel
		want    string
		leaks   []string
	}{
		"telegram uses the api root, without the bot token": {
			channel: domain.AlertChannel{
				Type:   domain.ChannelTelegram,
				Config: domain.ChannelConfig{BotToken: "8100:AAH-secret", ChatID: "-1"},
			},
			want:  "https://api.telegram.org",
			leaks: []string{"8100:AAH-secret", "sendMessage"},
		},
		"telegram honours a base url, because that is where it would deliver": {
			channel: domain.AlertChannel{
				Type: domain.ChannelTelegram,
				Config: domain.ChannelConfig{
					BotToken: "8100:AAH-secret", ChatID: "-1",
					BaseURL: "http://127.0.0.1:9604",
				},
			},
			want:  "http://127.0.0.1:9604",
			leaks: []string{"8100:AAH-secret"},
		},
		"slack keeps only the origin": {
			channel: domain.AlertChannel{
				Type: domain.ChannelSlack,
				Config: domain.ChannelConfig{
					URL: "https://hooks.slack.com/services/T0/B0/secretpath",
				},
			},
			want:  "https://hooks.slack.com",
			leaks: []string{"secretpath", "/services/"},
		},
		"discord keeps only the origin": {
			channel: domain.AlertChannel{
				Type: domain.ChannelDiscord,
				Config: domain.ChannelConfig{
					URL: "https://discord.com/api/webhooks/1/secrettoken",
				},
			},
			want:  "https://discord.com",
			leaks: []string{"secrettoken"},
		},
		"a signed webhook keeps its whole url, which is not a credential": {
			channel: domain.AlertChannel{
				Type: domain.ChannelWebhook,
				Config: domain.ChannelConfig{
					URL: "https://pager.example.test/trapline?team=core", Secret: "a-secret-long-enough",
				},
			},
			want:  "https://pager.example.test/trapline?team=core",
			leaks: []string{"a-secret-long-enough"},
		},
		"smtp is a host and a port, because that is what it is configured as": {
			channel: domain.AlertChannel{
				Type: domain.ChannelEmail,
				Config: domain.ChannelConfig{
					Host: "smtp.example.test", Port: 587, Password: "hunter2hunter2",
				},
			},
			want:  "smtp.example.test:587",
			leaks: []string{"hunter2hunter2"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			endpoint := one.channel.Endpoint()
			if endpoint != one.want {
				t.Errorf("endpoint = %q, want %q", endpoint, one.want)
			}
			for _, secret := range one.leaks {
				if strings.Contains(endpoint, secret) {
					t.Errorf("the endpoint carries %q: %q", secret, endpoint)
				}
			}
		})
	}
}

// TestEndpointOfAChannelWithNothingToConnectTo: an empty answer rather than a
// guess. `doctor` reports "the channel has no endpoint to probe", which is
// true; a guessed port would report a healthy channel for a relay listening
// on another one.
func TestEndpointOfAChannelWithNothingToConnectTo(t *testing.T) {
	for name, channel := range map[string]domain.AlertChannel{
		"an unknown type": {Type: domain.ChannelType("carrier-pigeon")},
		"smtp with no host": {
			Type: domain.ChannelEmail, Config: domain.ChannelConfig{Port: 25},
		},
		"a url that is not one": {
			Type: domain.ChannelSlack, Config: domain.ChannelConfig{URL: "not a url"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if endpoint := channel.Endpoint(); endpoint != "" {
				t.Errorf("endpoint = %q, want empty", endpoint)
			}
		})
	}
}
