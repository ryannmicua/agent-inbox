package main

import (
	"bytes"
	"io"
	"log"
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
	previousOutput := log.Writer()
	var warning bytes.Buffer
	log.SetOutput(&warning)
	t.Cleanup(func() { log.SetOutput(previousOutput) })
	if _, err := configuredNotifier(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warning.String(), "local testing only") || !strings.Contains(warning.String(), "production must set INBOX_WEBHOOK_URL") {
		t.Fatalf("disabled mode did not emit the production warning: %q", warning.String())
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

func TestConfiguredStoreRejectsPostgresqlAlias(t *testing.T) {
	t.Setenv("INBOX_STORAGE", "postgresql")
	t.Setenv("INBOX_DATABASE_URL", "postgres://example.invalid/inbox")
	if _, err := openConfiguredStore("unused.db"); err == nil || !strings.Contains(err.Error(), "unsupported storage backend") {
		t.Fatalf("configured storage accepted the postgresql alias: %v", err)
	}
}
