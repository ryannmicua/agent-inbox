package inbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

type Notification struct {
	Event       string `json:"event"`
	OccurredAt  string `json:"occurred_at"`
	MessageID   string `json:"message_id,omitempty"`
	SenderID    string `json:"sender_id,omitempty"`
	RecipientID string `json:"recipient_id,omitempty"`
	TenantID    string `json:"tenant_id,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Code        string `json:"code,omitempty"`
}

type Notifier interface {
	Notify(context.Context, Notification) error
}

type WebhookNotifier struct {
	URL    string
	Client *http.Client
}

func (w WebhookNotifier) Notify(ctx context.Context, event Notification) error {
	if w.URL == "" {
		return errors.New("webhook URL is required")
	}
	endpoint := safeWebhookEndpoint(w.URL)
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create webhook request for %s failed", endpoint)
	}
	req.Header.Set("Content-Type", "application/json")
	client := w.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	response, err := noRedirectClient(client).Do(req)
	if err != nil {
		return webhookRequestError(endpoint, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("webhook %s returned HTTP %d", endpoint, response.StatusCode)
	}
	return nil
}

func safeWebhookEndpoint(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "configured webhook"
	}
	return (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host}).String()
}

func webhookRequestError(endpoint string, err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("webhook request to %s: %w", endpoint, context.DeadlineExceeded)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("webhook request to %s: %w", endpoint, context.Canceled)
	default:
		return fmt.Errorf("webhook request to %s failed", endpoint)
	}
}
