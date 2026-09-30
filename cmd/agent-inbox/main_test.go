package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

func TestRequestJSONReportsRedirectAsError(t *testing.T) {
	forwarded := make(chan struct{}, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	_, private, err := inbox.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	client := &inbox.Client{Server: redirect.URL, Agent: "agent-a", KeyID: "primary", Private: private}
	if err := requestJSON(context.Background(), client, http.MethodPost, "/v1/messages", []byte(`{}`), io.Discard); err == nil {
		t.Fatal("CLI accepted a redirect response as a successful request")
	}
	select {
	case <-forwarded:
		t.Fatal("CLI forwarded signed request data to the redirect target")
	default:
	}
}

func TestWaitReturnsInvalidServerURL(t *testing.T) {
	privateKey := filepath.Join(t.TempDir(), "agent.key")
	if err := inbox.WriteKeyPair(privateKey, filepath.Join(t.TempDir(), "agent.pub")); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"wait", "--server", "not-a-url", "--agent", "agent-a", "--key", privateKey}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "protocol scheme") {
		t.Fatalf("wait did not return the invalid URL error: %v", err)
	}
}

func TestWaitRetriesStreamEOFAndReturnsOutputError(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: bell\n\n")
	}))
	defer server.Close()

	privateKey := filepath.Join(t.TempDir(), "agent.key")
	if err := inbox.WriteKeyPair(privateKey, filepath.Join(t.TempDir(), "agent.pub")); err != nil {
		t.Fatal(err)
	}
	writeErr := errors.New("stdout closed")
	err := run([]string{"wait", "--server", server.URL, "--agent", "agent-a", "--key", privateKey}, errorWriter{err: writeErr}, io.Discard)
	if !errors.Is(err, writeErr) {
		t.Fatalf("wait did not return the stdout failure: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("wait made %d stream requests, want 2", got)
	}
}

type errorWriter struct {
	err error
}

func (w errorWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func TestSendWithoutIDGeneratesUUIDv4(t *testing.T) {
	receivedEnvelope := make(chan inbox.Envelope, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		var received inbox.Envelope
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			http.Error(w, "invalid envelope", http.StatusBadRequest)
			return
		}
		receivedEnvelope <- received
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()

	privateKey := filepath.Join(t.TempDir(), "agent.key")
	publicKey := filepath.Join(t.TempDir(), "agent.pub")
	if err := inbox.WriteKeyPair(privateKey, publicKey); err != nil {
		t.Fatal(err)
	}
	err := run([]string{
		"send", "--server", server.URL, "--agent", "agent-a", "--key", privateKey,
		"--to", "agent-b", "--kind", "instruction", "--payload", `{"task":"check"}`,
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("send without explicit ID failed: %v", err)
	}
	received := <-receivedEnvelope
	if len(received.ID) != 36 || received.ID[14] != '4' || !strings.ContainsRune("89ab", rune(received.ID[19])) {
		t.Fatalf("omitted --id did not produce a UUID v4: %q", received.ID)
	}
}

func TestSendWithULIDDefaultsTaskAndThreadToMessageID(t *testing.T) {
	const ulid = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	store, err := inbox.OpenSQLite(filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	publicA, privateA, err := inbox.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	publicB, _, err := inbox.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	registry := inbox.Registry{Version: 1, Agents: []inbox.RegistryAgent{
		{ID: "agent-a", TenantID: "tenant-one", PublicKeys: []inbox.RegistryKey{{ID: "primary", PublicKey: base64.StdEncoding.EncodeToString(publicA)}}, AllowedRecipients: []string{"agent-b"}, AllowedKinds: []string{"instruction"}},
		{ID: "agent-b", TenantID: "tenant-one", PublicKeys: []inbox.RegistryKey{{ID: "primary", PublicKey: base64.StdEncoding.EncodeToString(publicB)}}, AllowedRecipients: []string{"agent-a"}, AllowedKinds: []string{"instruction", "result"}},
	}}
	registryPath := filepath.Join(t.TempDir(), "registry.json")
	registryData, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registryPath, registryData, 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := inbox.NewServer(store, inbox.FileRegistry{Path: registryPath}, notifierFunc(func(context.Context, inbox.Notification) error { return nil }), inbox.DefaultServerConfig())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(service)
	defer server.Close()

	privateKey := filepath.Join(t.TempDir(), "agent.key")
	if err := os.WriteFile(privateKey, []byte(base64.StdEncoding.EncodeToString(privateA)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{
		"send", "--server", server.URL, "--agent", "agent-a", "--key", privateKey,
		"--to", "agent-b", "--kind", "instruction", "--id", ulid, "--payload", `{"task":"check"}`,
	}, io.Discard, io.Discard); err != nil {
		t.Fatalf("send with ULID and omitted task/thread failed: %v", err)
	}
	messages, err := store.ListMessages(context.Background(), "agent-b", "tenant-one", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].ID != ulid || messages[0].TaskID != ulid || messages[0].ThreadID != ulid {
		t.Fatalf("inbox did not accept the CLI's ULID defaults: %+v", messages)
	}
}

type notifierFunc func(context.Context, inbox.Notification) error

func (f notifierFunc) Notify(ctx context.Context, notification inbox.Notification) error {
	return f(ctx, notification)
}

func TestAckCommandSendsOnlyProcessedAssertion(t *testing.T) {
	requestBody := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages/01ARZ3NDEKTSV4RRFFQ69G5FAV/ack" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		requestBody <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"acknowledged":true}`)
	}))
	defer server.Close()

	privateKey := filepath.Join(t.TempDir(), "agent.key")
	publicKey := filepath.Join(t.TempDir(), "agent.pub")
	if err := inbox.WriteKeyPair(privateKey, publicKey); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{
		"ack", "--server", server.URL, "--agent", "agent-b", "--key", privateKey,
		"--message", "01ARZ3NDEKTSV4RRFFQ69G5FAV",
	}, io.Discard, io.Discard); err != nil {
		t.Fatalf("ack command failed: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(<-requestBody, &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 1 || body["processed"] != true {
		t.Fatalf("ack command did not send only processed=true: %v", body)
	}
}
