package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ryannmicua/agent-inbox/internal/inbox"
)

// docSigner is a second client runtime written only from docs/protocol.md. It
// deliberately avoids the inbox package's signing and client code, so the
// walkthrough proves the documented contract is enough to join.
type docSigner struct {
	server string
	agent  string
	keyID  string
	key    ed25519.PrivateKey
}

func loadDocKey(t *testing.T, path string) ed25519.PrivateKey {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(raw) != 64 {
		t.Fatalf("key file is not a base64 64-byte private key: %v", err)
	}
	// The documented key file is seed || public key; derive from the seed.
	return ed25519.NewKeyFromSeed(raw[:32])
}

func docRequestBytes(agent, keyID, method, path, timestamp, nonce string, body []byte) []byte {
	digest := sha256.Sum256(body)
	lines := []string{"agent-inbox-request-v1", agent, keyID, method, path, timestamp, nonce, hex.EncodeToString(digest[:])}
	return []byte(strings.Join(lines, "\n"))
}

func (d docSigner) do(t *testing.T, method, path string, body []byte) (int, map[string]any) {
	t.Helper()
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonceBytes := make([]byte, 24)
	if _, err := rand.Read(nonceBytes); err != nil {
		t.Fatal(err)
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	signature := ed25519.Sign(d.key, docRequestBytes(d.agent, d.keyID, method, path, timestamp, nonce, body))
	req, err := http.NewRequestWithContext(context.Background(), method, d.server+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Agent-ID", d.agent)
	req.Header.Set("X-Key-ID", d.keyID)
	req.Header.Set("X-Request-Timestamp", timestamp)
	req.Header.Set("X-Request-Nonce", nonce)
	req.Header.Set("X-Request-Signature", base64.StdEncoding.EncodeToString(signature))
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("HTTP %d returned non-JSON %q", response.StatusCode, data)
	}
	return response.StatusCode, decoded
}

// docString escapes a string by the documented rules.
func docString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20, r == '<', r == '>', r == '&', r == 0x2028, r == 0x2029:
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// docValue encodes a value decoded with UseNumber: sorted object keys, kept
// array order, and numbers copied as written.
func docValue(v any) string {
	switch typed := v.(type) {
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(typed)
	case json.Number:
		return typed.String()
	case string:
		return docString(typed)
	case []any:
		parts := make([]string, len(typed))
		for i, item := range typed {
			parts[i] = docValue(item)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, key := range keys {
			parts[i] = docString(key) + ":" + docValue(typed[key])
		}
		return "{" + strings.Join(parts, ",") + "}"
	default:
		panic(fmt.Sprintf("unexpected JSON value %T", v))
	}
}

// docCanonical builds the canonical envelope bytes from a decoded envelope
// object, following the documented member order and omission rules.
func docCanonical(env map[string]any) []byte {
	str := func(name string) string { s, _ := env[name].(string); return s }
	var parts []string
	add := func(name, encoded string) { parts = append(parts, docString(name)+":"+encoded) }
	for _, name := range []string{"type", "id", "task_id", "thread_id"} {
		add(name, docString(str(name)))
	}
	if str("reply_to") != "" {
		add("reply_to", docString(str("reply_to")))
	}
	for _, name := range []string{"sender_id", "recipient_id"} {
		add(name, docString(str(name)))
	}
	if str("space_id") != "" {
		add("space_id", docString(str("space_id")))
	}
	for _, name := range []string{"key_id", "kind", "created_at"} {
		add(name, docString(str(name)))
	}
	if str("asserted_authority") != "" {
		add("asserted_authority", docString(str("asserted_authority")))
	}
	add("content_is_data", docValue(env["content_is_data"]))
	payload, ok := env["payload"]
	if !ok || payload == nil {
		payload = map[string]any{}
	}
	add("payload", docValue(payload))
	artifacts, _ := env["artifacts"].([]any)
	encoded := make([]string, len(artifacts))
	for i, item := range artifacts {
		a := item.(map[string]any)
		encoded[i] = "{" + strings.Join([]string{
			`"uri":` + docValue(a["uri"]), `"media_type":` + docValue(a["media_type"]),
			`"sha256":` + docValue(a["sha256"]), `"size":` + docValue(a["size"]),
		}, ",") + "}"
	}
	add("artifacts", "["+strings.Join(encoded, ",")+"]")
	if provenance, ok := env["provenance"].(map[string]any); ok && len(provenance) > 0 {
		add("provenance", docValue(provenance))
	}
	return []byte("agent-inbox-envelope-v1\n{" + strings.Join(parts, ",") + "}")
}

// docNoteCanonical builds the canonical note revision bytes from a decoded
// revision object, following the documented member order.
func docNoteCanonical(rev map[string]any) []byte {
	var parts []string
	for _, name := range []string{"type", "revision_id", "space_id", "note_id", "base_revision", "author_id", "key_id", "created_at", "content_is_data", "media_type", "sha256", "body"} {
		parts = append(parts, docString(name)+":"+docValue(rev[name]))
	}
	return []byte("agent-inbox-note-v1\n{" + strings.Join(parts, ",") + "}")
}

func decodeJSON(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var out map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// send signs and submits a new envelope. The wire body is the canonical
// object plus the signature, which is one valid encoding of it.
func (d docSigner) send(t *testing.T, env map[string]any) (int, map[string]any) {
	t.Helper()
	canonical := docCanonical(env)
	signature := base64.StdEncoding.EncodeToString(ed25519.Sign(d.key, canonical))
	object := strings.TrimPrefix(string(canonical), "agent-inbox-envelope-v1\n")
	body := strings.TrimSuffix(object, "}") + `,"signature":` + docString(signature) + "}"
	return d.do(t, http.MethodPost, "/v1/messages", []byte(body))
}

func TestDocumentedSignerMatchesVectors(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "inbox", "testdata", "signing-vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	vectors := decodeJSON(t, data)
	seed, err := base64.StdEncoding.DecodeString(vectors["seed_base64"].(string))
	if err != nil {
		t.Fatal(err)
	}
	key := ed25519.NewKeyFromSeed(seed)
	if got := base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)); got != vectors["public_key_base64"] {
		t.Fatalf("seed derives public key %s", got)
	}
	for _, item := range vectors["envelopes"].([]any) {
		v := item.(map[string]any)
		env := v["envelope"].(map[string]any)
		canonical := docCanonical(env)
		if string(canonical) != v["canonical"] {
			t.Fatalf("%s: documented canonical form differs\n got %q\nwant %q", v["name"], canonical, v["canonical"])
		}
		if got := base64.StdEncoding.EncodeToString(ed25519.Sign(key, canonical)); got != v["signature"] {
			t.Fatalf("%s: signature differs", v["name"])
		}
	}
	for _, item := range vectors["requests"].([]any) {
		r := item.(map[string]any)
		canonical := docRequestBytes(r["agent_id"].(string), r["key_id"].(string), r["method"].(string), r["path"].(string), r["timestamp"].(string), r["nonce"].(string), []byte(r["body"].(string)))
		if string(canonical) != r["canonical"] || base64.StdEncoding.EncodeToString(ed25519.Sign(key, canonical)) != r["signature"] {
			t.Fatalf("%s: documented request signing differs", r["name"])
		}
	}
	notes, _ := vectors["notes"].([]any)
	if len(notes) == 0 {
		t.Fatal("vector file has no note revisions")
	}
	for _, item := range notes {
		v := item.(map[string]any)
		rev := v["revision"].(map[string]any)
		canonical := docNoteCanonical(rev)
		digest := sha256.Sum256([]byte(rev["body"].(string)))
		if string(canonical) != v["canonical"] || base64.StdEncoding.EncodeToString(ed25519.Sign(key, canonical)) != v["signature"] || hex.EncodeToString(digest[:]) != rev["sha256"] {
			t.Fatalf("%s: documented note signing differs\n got %q\nwant %q", v["name"], canonical, v["canonical"])
		}
	}
}

type quietNotifier struct{}

func (quietNotifier) Notify(context.Context, inbox.Notification) error { return nil }

// TestWalkthroughTwoRuntimes replays the walkthrough exchange between a bot
// using this CLI and a bot using only the documented protocol.
func TestWalkthroughTwoRuntimes(t *testing.T) {
	dir := t.TempDir()
	keys := map[string]string{}
	for _, id := range []string{"cli-bot", "raw-bot"} {
		keys[id] = filepath.Join(dir, id+".key")
		if err := run([]string{"keygen", "--private-key", keys[id], "--public-key", filepath.Join(dir, id+".pub")}, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	publicKey := func(id string) string {
		data, err := os.ReadFile(filepath.Join(dir, id+".pub"))
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(data))
	}
	registryPath := filepath.Join(dir, "registry.json")
	writeRegistry := func(rawDisabled bool) {
		registry := inbox.Registry{Version: 2, Agents: []inbox.RegistryAgent{
			{ID: "cli-bot", TenantID: "walkthrough", PublicKeys: []inbox.RegistryKey{{ID: "primary", PublicKey: publicKey("cli-bot")}}, AllowedRecipients: []string{"raw-bot"}, AllowedKinds: []string{"instruction", "result"}},
			{ID: "raw-bot", TenantID: "walkthrough", Disabled: rawDisabled, PublicKeys: []inbox.RegistryKey{{ID: "primary", PublicKey: publicKey("raw-bot")}}, AllowedRecipients: []string{"cli-bot"}, AllowedKinds: []string{"instruction", "result"}},
		}, Spaces: []inbox.RegistrySpace{{ID: "release-notes", TenantID: "walkthrough", Members: []inbox.RegistrySpaceMember{
			{Agent: "cli-bot", Grants: []string{"read", "post"}},
			{Agent: "raw-bot", Grants: []string{"read", "post"}},
		}}}}
		data, err := json.Marshal(registry)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(registryPath+".tmp", data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(registryPath+".tmp", registryPath); err != nil {
			t.Fatal(err)
		}
	}
	writeRegistry(false)
	store, err := inbox.OpenSQLite(filepath.Join(dir, "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := inbox.NewServer(store, inbox.FileRegistry{Path: registryPath}, quietNotifier{}, inbox.DefaultServerConfig())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(service)
	t.Cleanup(func() { server.Close(); store.Close() })

	cli := func(command string, args ...string) (string, error) {
		var out bytes.Buffer
		full := append([]string{command, "--server", server.URL, "--agent", "cli-bot", "--key", keys["cli-bot"]}, args...)
		err := run(full, &out, io.Discard)
		return out.String(), err
	}
	raw := docSigner{server: server.URL, agent: "raw-bot", keyID: "primary", key: loadDocKey(t, keys["raw-bot"])}
	messages := func(body map[string]any) []any {
		list, _ := body["messages"].([]any)
		return list
	}

	// Step 1: the CLI bot describes a file and sends an instruction that
	// references it while the other bot is offline.
	draft := filepath.Join(dir, "draft.md")
	if err := os.WriteFile(draft, []byte("draft spec v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var refOut bytes.Buffer
	if err := run([]string{"artifact-ref", "--file", draft, "--uri", "https://files.example.com/draft.md", "--media-type", "text/markdown"}, &refOut, io.Discard); err != nil {
		t.Fatal(err)
	}
	ref := strings.TrimSpace(refOut.String())
	sent, err := cli("send", "--to", "raw-bot", "--kind", "instruction", "--payload", `{"task":"review <draft> & reply","lang":"français"}`, "--artifact", ref)
	if err != nil {
		t.Fatal(err)
	}
	instruction := decodeJSON(t, []byte(sent))
	instructionID := instruction["id"].(string)

	// Step 2: the raw bot comes online, polls, and checks the sender's
	// signature with the documented canonical form.
	status, polled := raw.do(t, http.MethodGet, "/v1/messages?limit=100", nil)
	if status != http.StatusOK || len(messages(polled)) != 1 {
		t.Fatalf("raw poll returned HTTP %d %v", status, polled)
	}
	received := messages(polled)[0].(map[string]any)
	if received["id"] != instructionID || received["tenant_id"] != "walkthrough" {
		t.Fatalf("raw bot received %v", received)
	}
	artifact := received["artifacts"].([]any)[0].(map[string]any)
	if fmt.Sprintf("%s|%s|%s|%s", artifact["uri"], artifact["media_type"], artifact["sha256"], artifact["size"]) != ref {
		t.Fatalf("artifact reference changed in transit: %v", artifact)
	}
	cliPublic, _ := base64.StdEncoding.DecodeString(publicKey("cli-bot"))
	signature, _ := base64.StdEncoding.DecodeString(received["signature"].(string))
	if !ed25519.Verify(ed25519.PublicKey(cliPublic), docCanonical(received), signature) {
		t.Fatal("documented canonical form does not verify a CLI-signed envelope")
	}

	// Step 3: acknowledge after processing; a repeat is a duplicate and the
	// inbox is then empty.
	for _, wantDuplicate := range []bool{false, true} {
		status, acked := raw.do(t, http.MethodPost, "/v1/messages/"+instructionID+"/ack", []byte(`{"processed":true}`))
		if status != http.StatusOK || acked["duplicate"] != wantDuplicate {
			t.Fatalf("raw ack returned HTTP %d %v", status, acked)
		}
	}
	if _, polled := raw.do(t, http.MethodGet, "/v1/messages?limit=100", nil); len(messages(polled)) != 0 {
		t.Fatalf("acknowledged message is still delivered: %v", polled)
	}

	// Step 4: the raw bot replies with a correlated result whose payload
	// needs every escaping rule.
	resultID := "0192f0c4-7d2e-7a10-8c3b-5d6e7f809b01"
	status, accepted := raw.send(t, map[string]any{
		"type": "message", "id": resultID, "task_id": instruction["task_id"], "thread_id": instruction["thread_id"],
		"reply_to": instructionID, "sender_id": "raw-bot", "recipient_id": "cli-bot", "key_id": "primary", "kind": "result",
		"created_at": time.Now().UTC().Format(time.RFC3339), "content_is_data": true,
		"payload":   decodeJSON(t, []byte(`{"summary":"x < y & z > w","line":"a\u2028b","price":1.50,"notes":["naïve","tab\there"]}`)),
		"artifacts": []any{},
	})
	if status != http.StatusCreated || accepted["sequence"] == nil {
		t.Fatalf("raw result was rejected: HTTP %d %v", status, accepted)
	}

	// Step 5: the CLI bot waits for its inbox and gets the result at once.
	start := time.Now()
	out, err := cli("poll", "--wait", "10s")
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("poll --wait waited although the result was ready")
	}
	result := messages(decodeJSON(t, []byte(out)))
	if len(result) != 1 || result[0].(map[string]any)["reply_to"] != instructionID {
		t.Fatalf("CLI bot did not receive the correlated result: %s", out)
	}
	if !utf8.ValidString(out) || !strings.Contains(out, "naïve") {
		t.Fatalf("result payload lost its text: %s", out)
	}
	if _, err := cli("ack", "--message", resultID); err != nil {
		t.Fatal(err)
	}
	if out, err := cli("poll", "--wait", "200ms"); err != nil || len(messages(decodeJSON(t, []byte(out)))) != 0 {
		t.Fatalf("empty inbox poll --wait returned %q, %v", out, err)
	}

	// Step 6: the CLI bot posts to the shared space; the raw bot verifies,
	// acknowledges its own delivery, and answers in the space.
	sent, err = cli("send", "--space", "release-notes", "--kind", "instruction", "--payload", `{"ask":"post your section"}`)
	if err != nil {
		t.Fatal(err)
	}
	group := decodeJSON(t, []byte(sent))
	_, polled = raw.do(t, http.MethodGet, "/v1/messages?limit=100", nil)
	if len(messages(polled)) != 1 {
		t.Fatalf("raw bot did not receive the group message: %v", polled)
	}
	received = messages(polled)[0].(map[string]any)
	signature, _ = base64.StdEncoding.DecodeString(received["signature"].(string))
	if received["id"] != group["id"] || received["space_id"] != "release-notes" || !ed25519.Verify(ed25519.PublicKey(cliPublic), docCanonical(received), signature) {
		t.Fatalf("group message did not verify with the documented canonical form: %v", received)
	}
	if status, acked := raw.do(t, http.MethodPost, "/v1/messages/"+group["id"].(string)+"/ack", []byte(`{"processed":true}`)); status != http.StatusOK || acked["duplicate"] != false {
		t.Fatalf("raw group ack returned HTTP %d %v", status, acked)
	}
	status, accepted = raw.send(t, map[string]any{
		"type": "message", "id": "0192f0c4-7d2e-7a10-8c3b-5d6e7f809b02", "task_id": group["task_id"], "thread_id": group["thread_id"],
		"reply_to": group["id"], "sender_id": "raw-bot", "recipient_id": "", "space_id": "release-notes", "key_id": "primary", "kind": "result",
		"created_at": time.Now().UTC().Format(time.RFC3339), "content_is_data": true, "payload": map[string]any{"section": "done"}, "artifacts": []any{},
	})
	if status != http.StatusCreated || accepted["space_id"] != "release-notes" {
		t.Fatalf("raw group result was rejected: HTTP %d %v", status, accepted)
	}
	out, err = cli("poll")
	if err != nil {
		t.Fatal(err)
	}
	if result := messages(decodeJSON(t, []byte(out))); len(result) != 1 || result[0].(map[string]any)["space_id"] != "release-notes" {
		t.Fatalf("CLI bot did not receive the group result: %s", out)
	}
	if _, err := cli("ack", "--message", "0192f0c4-7d2e-7a10-8c3b-5d6e7f809b02"); err != nil {
		t.Fatal(err)
	}

	// Step 7: the operator disables the raw bot. Its poll, ack, and doorbell
	// stream are refused, and messages to it are rejected.
	writeRegistry(true)
	for _, request := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/messages?limit=100", ""},
		{http.MethodPost, "/v1/messages/" + resultID + "/ack", `{"processed":true}`},
		{http.MethodGet, "/v1/events", ""},
	} {
		status, refused := raw.do(t, request.method, request.path, []byte(request.body))
		code, _ := refused["error"].(map[string]any)["code"].(string)
		if status != http.StatusUnauthorized || code != "authentication_failed" {
			t.Fatalf("revoked bot %s %s returned HTTP %d %v", request.method, request.path, status, refused)
		}
	}
	if _, err := cli("send", "--to", "raw-bot", "--kind", "instruction", "--payload", `{"task":"anything"}`); err == nil || !strings.Contains(err.Error(), "recipient_not_allowed") {
		t.Fatalf("send to a revoked bot returned %v", err)
	}
}
