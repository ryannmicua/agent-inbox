package inbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
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

type NoopNotifier struct{}

func (NoopNotifier) Notify(context.Context, Notification) error { return nil }

type WebhookNotifier struct {
	URL    string
	Client *http.Client
}

func (w WebhookNotifier) Notify(ctx context.Context, event Notification) error {
	if w.URL == "" {
		return nil
	}
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := w.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("webhook returned a non-success status")
	}
	return nil
}
