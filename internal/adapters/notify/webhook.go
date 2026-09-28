package notify

import (
	"context"
	"fmt"

	"github.com/antoniojosev/trapline/internal/domain"
)

// sendWebhook posts the payload, signed.
//
// This is the channel that exists for everything the other four are not: a
// pager, a ticket system, a script somebody wrote in an afternoon. What makes
// it usable for those is the signature — without it, the receiving endpoint
// is a URL anyone who learns it can post anything to, and the honest
// integration advice would have to be "put it behind a VPN".
//
// The body signed is exactly the body sent, byte for byte. Re-encoding the
// payload on the receiving side and verifying that would fail on any
// difference in key order, which is the classic way a webhook signature ends
// up documented as "does not work, disable the check".
func (d *Dispatcher) sendWebhook(
	ctx context.Context, config *domain.ChannelConfig, payload *domain.AlertPayload, deliveryID string,
) error {
	body, err := payload.Encode()
	if err != nil {
		return err
	}
	headers := map[string]string{
		domain.WebhookSignatureHeader: domain.SignWebhook(config.Secret, d.now(), body),
		domain.WebhookEventHeader:     string(payload.Event),
		domain.WebhookDeliveryHeader:  deliveryID,
	}
	if err := d.post(ctx, config.URL, body, headers); err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	return nil
}
