package main

import (
	"io"
	"strings"
	"testing"

	"github.com/ryannmicua/agent-inbox/internal/inbox"
)

func TestServerStartupRequiresWebhook(t *testing.T) {
	t.Setenv("INBOX_WEBHOOK_URL", "")
	t.Setenv("INBOX_NOTIFICATIONS_DISABLED", "")
	err := run(nil, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "set INBOX_WEBHOOK_URL or explicitly set INBOX_NOTIFICATIONS_DISABLED=true") {
		t.Fatalf("startup without a notification webhook returned %v", err)
	}
}

func TestNotificationModeMustBeExplicitAndMutuallyExclusive(t *testing.T) {
	t.Setenv("INBOX_WEBHOOK_URL", "")
	t.Setenv("INBOX_NOTIFICATIONS_DISABLED", "true")
	notifier, err := configuredNotifier()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := notifier.(inbox.NoopNotifier); !ok {
		t.Fatalf("explicit disabled mode selected %T, want inbox.NoopNotifier", notifier)
	}

	t.Setenv("INBOX_WEBHOOK_URL", "https://notify.example.com/hook")
	if _, err := configuredNotifier(); err == nil || !strings.Contains(err.Error(), "choose either") {
		t.Fatalf("simultaneous webhook and disabled mode returned %v", err)
	}
	for _, invalid := range []string{"yes", "not-a-bool"} {
		t.Setenv("INBOX_WEBHOOK_URL", "")
		t.Setenv("INBOX_NOTIFICATIONS_DISABLED", invalid)
		if _, err := configuredNotifier(); err == nil || !strings.Contains(err.Error(), "must be true or false") {
			t.Fatalf("invalid disabled value %q returned %v", invalid, err)
		}
	}
}
