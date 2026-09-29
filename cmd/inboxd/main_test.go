package main

import (
	"io"
	"strings"
	"testing"

	"github.com/ryannmicua/agent-inbox/internal/inbox"
)

func TestServerStartupRequiresWebhook(t *testing.T) {
	t.Setenv("INBOX_WEBHOOK_URL", "")
	err := run(nil, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "INBOX_WEBHOOK_URL is required") {
		t.Fatalf("startup without a notification webhook returned %v", err)
	}
}

func TestConfiguredNotifierRequiresAbsoluteHTTPURL(t *testing.T) {
	for _, webhook := range []string{"", "not-a-url", "ftp://notify.example.com/hook"} {
		t.Setenv("INBOX_WEBHOOK_URL", webhook)
		if _, err := configuredNotifier(); err == nil {
			t.Fatalf("configured notifier accepted URL %q", webhook)
		}
	}
	t.Setenv("INBOX_WEBHOOK_URL", "http://notification-sink:8081/notifications")
	notifier, err := configuredNotifier()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := notifier.(inbox.WebhookNotifier); !ok {
		t.Fatalf("configured notifier selected %T, want inbox.WebhookNotifier", notifier)
	}
}

func TestConfiguredStoreRejectsPostgresqlAlias(t *testing.T) {
	t.Setenv("INBOX_STORAGE", "postgresql")
	t.Setenv("INBOX_DATABASE_URL", "postgres://example.invalid/inbox")
	if _, err := openConfiguredStore("unused.db"); err == nil || !strings.Contains(err.Error(), "unsupported storage backend") {
		t.Fatalf("configured storage accepted the postgresql alias: %v", err)
	}
}
