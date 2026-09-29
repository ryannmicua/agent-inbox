package main

import (
	"io"
	"strings"
	"testing"
)

func TestServerStartupRequiresWebhook(t *testing.T) {
	t.Setenv("INBOX_WEBHOOK_URL", "")
	err := run(nil, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "INBOX_WEBHOOK_URL is required") {
		t.Fatalf("startup without a notification webhook returned %v", err)
	}
}
