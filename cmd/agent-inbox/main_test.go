package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryannmicua/agent-inbox/internal/inbox"
)

func TestReceiveAliasIsNotAccepted(t *testing.T) {
	err := run([]string{"receive"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), `unknown command "receive"`) {
		t.Fatalf("receive command returned %v", err)
	}
}

func TestRemovedPollCursorAndAckNoteFlagsAreNotAccepted(t *testing.T) {
	for _, args := range [][]string{{"poll", "--after-seq", "0"}, {"ack", "--note", "processed"}} {
		if err := run(args, io.Discard, io.Discard); err == nil {
			t.Fatalf("removed CLI flags were accepted: %v", args)
		}
	}
}

func TestSendRejectsOversizedPayloadSources(t *testing.T) {
	dir := t.TempDir()
	privateKey := filepath.Join(dir, "agent.key")
	publicKey := filepath.Join(dir, "agent.pub")
	if err := inbox.WriteKeyPair(privateKey, publicKey); err != nil {
		t.Fatal(err)
	}
	payloadFile := filepath.Join(dir, "payload.json")
	if err := os.WriteFile(payloadFile, []byte(strings.Repeat("x", inbox.MaxPayloadBytes+1024)), 0o600); err != nil {
		t.Fatal(err)
	}
	baseArgs := []string{"send", "--server", "http://127.0.0.1:1", "--agent", "agent-a", "--key", privateKey, "--to", "agent-b", "--kind", "instruction"}
	cases := [][]string{
		append(append([]string{}, baseArgs...), "--payload", strings.Repeat("x", inbox.MaxPayloadBytes+1)),
		append(append([]string{}, baseArgs...), "--payload-file", payloadFile),
	}
	for _, args := range cases {
		err := run(args, io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "payload must be 16 KiB or smaller") {
			t.Fatalf("oversized payload was not rejected before sending: %v", err)
		}
	}
}
