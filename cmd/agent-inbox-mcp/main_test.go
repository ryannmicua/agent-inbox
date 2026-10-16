package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ryannmicua/agent-inbox/internal/inbox"
)

const (
	alpha = "demo-alpha"
	beta  = "demo-beta"
	space = "release-notes"
	// opsSpace holds only demo-alpha, so demo-beta must not see into it.
	opsSpace = "alpha-ops"
)

// walkthroughServer serves the walkthrough's cast: demo-alpha and demo-beta
// may send each other instructions and results, both hold read, post and
// note.write in release-notes, and only demo-alpha is in alpha-ops.
func walkthroughServer(t *testing.T) (*httptest.Server, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	store, err := inbox.OpenSQLite(filepath.Join(dir, "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	registry := inbox.Registry{Version: 2}
	keys := map[string]string{}
	for _, pair := range [][2]string{{alpha, beta}, {beta, alpha}} {
		public, private, err := inbox.GenerateKeyPair()
		if err != nil {
			t.Fatal(err)
		}
		keys[pair[0]] = filepath.Join(dir, pair[0]+".key")
		if err := os.WriteFile(keys[pair[0]], []byte(base64.StdEncoding.EncodeToString(private)), 0o600); err != nil {
			t.Fatal(err)
		}
		registry.Agents = append(registry.Agents, inbox.RegistryAgent{ID: pair[0], TenantID: "tenant-one",
			PublicKeys:        []inbox.RegistryKey{{ID: "primary", PublicKey: base64.StdEncoding.EncodeToString(public)}},
			AllowedRecipients: []string{pair[1]}, AllowedKinds: []string{"instruction", "result"}})
	}
	grants := []string{"read", "post", "note.write"}
	registry.Spaces = []inbox.RegistrySpace{
		{ID: space, TenantID: "tenant-one", Members: []inbox.RegistrySpaceMember{{Agent: alpha, Grants: grants}, {Agent: beta, Grants: grants}}},
		{ID: opsSpace, TenantID: "tenant-one", Members: []inbox.RegistrySpaceMember{{Agent: alpha, Grants: grants}}},
	}
	registryPath := filepath.Join(dir, "registry.json")
	data, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registryPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	notifier := notifierFunc(func(context.Context, inbox.Notification) error { return nil })
	service, err := inbox.NewServer(store, inbox.FileRegistry{Path: registryPath}, notifier, inbox.DefaultServerConfig())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(service)
	t.Cleanup(server.Close)
	return server, keys
}

type notifierFunc func(context.Context, inbox.Notification) error

func (f notifierFunc) Notify(ctx context.Context, n inbox.Notification) error { return f(ctx, n) }

// mcpClient is a minimal in-process MCP client: it runs the adapter's run()
// on pipes and matches responses to requests by id.
type mcpClient struct {
	t       *testing.T
	stdin   *io.PipeWriter
	done    chan error
	mu      sync.Mutex
	nextID  int
	pending map[string]chan reply
	// stray receives responses no request is waiting for.
	stray chan reply
}

type reply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type callResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

func (r callResult) text() string {
	var parts []string
	for _, c := range r.Content {
		parts = append(parts, c.Text)
	}
	return strings.Join(parts, "\n")
}

func startMCP(t *testing.T, args ...string) *mcpClient {
	t.Helper()
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	c := &mcpClient{t: t, stdin: stdinW, done: make(chan error, 1), pending: map[string]chan reply{}, stray: make(chan reply, 16)}
	var stderr bytes.Buffer
	go func() {
		err := run(context.Background(), args, stdinR, stdoutW, &stderr)
		stdoutW.Close()
		c.done <- err
	}()
	go func() {
		scanner := bufio.NewScanner(stdoutR)
		scanner.Buffer(make([]byte, 64*1024), 4<<20)
		for scanner.Scan() {
			var r reply
			if err := json.Unmarshal(scanner.Bytes(), &r); err != nil || r.JSONRPC != "2.0" {
				r = reply{Error: &rpcError{Message: "stdout carried a line that is not JSON-RPC: " + scanner.Text()}}
			}
			c.mu.Lock()
			waiter := c.pending[idKey(r.ID)]
			delete(c.pending, idKey(r.ID))
			c.mu.Unlock()
			if waiter != nil {
				waiter <- r
			} else {
				c.stray <- r
			}
		}
	}()
	t.Cleanup(func() { c.close() })
	return c
}

func (c *mcpClient) write(message any) {
	c.t.Helper()
	data, err := json.Marshal(message)
	if err != nil {
		c.t.Fatal(err)
	}
	c.writeLine(string(data))
}

func (c *mcpClient) writeLine(line string) {
	c.t.Helper()
	if _, err := io.WriteString(c.stdin, line+"\n"); err != nil {
		c.t.Fatalf("write to adapter: %v", err)
	}
}

// start sends a request and returns its id and a channel for the reply.
func (c *mcpClient) start(method string, params any) (json.RawMessage, chan reply) {
	c.t.Helper()
	c.mu.Lock()
	c.nextID++
	id := json.RawMessage(fmt.Sprintf("%d", c.nextID))
	waiter := make(chan reply, 1)
	c.pending[idKey(id)] = waiter
	c.mu.Unlock()
	c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	return id, waiter
}

func (c *mcpClient) await(method string, waiter chan reply) reply {
	c.t.Helper()
	select {
	case r := <-waiter:
		return r
	case <-time.After(15 * time.Second):
		c.t.Fatalf("no response to %s", method)
		return reply{}
	}
}

func (c *mcpClient) request(method string, params any) reply {
	c.t.Helper()
	_, waiter := c.start(method, params)
	return c.await(method, waiter)
}

func (c *mcpClient) initialize(version string) map[string]any {
	c.t.Helper()
	r := c.request("initialize", map[string]any{"protocolVersion": version, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test-client", "version": "1"}})
	if r.Error != nil {
		c.t.Fatalf("initialize: %+v", r.Error)
	}
	c.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	var result map[string]any
	if err := json.Unmarshal(r.Result, &result); err != nil {
		c.t.Fatal(err)
	}
	return result
}

func (c *mcpClient) callTool(name string, args any) callResult {
	c.t.Helper()
	return c.awaitTool(name, c.startTool(name, args))
}

func (c *mcpClient) startTool(name string, args any) chan reply {
	c.t.Helper()
	_, waiter := c.start("tools/call", map[string]any{"name": name, "arguments": args})
	return waiter
}

func (c *mcpClient) awaitTool(name string, waiter chan reply) callResult {
	c.t.Helper()
	r := c.await(name, waiter)
	if r.Error != nil {
		c.t.Fatalf("%s: protocol error %+v", name, r.Error)
	}
	var result callResult
	if err := json.Unmarshal(r.Result, &result); err != nil {
		c.t.Fatalf("%s: %v", name, err)
	}
	if len(result.Content) == 0 || result.Content[0].Type != "text" {
		c.t.Fatalf("%s: result has no text content: %s", name, r.Result)
	}
	return result
}

// ok calls a tool that must succeed and decodes its structured content.
func (c *mcpClient) ok(name string, args any, into any) {
	c.t.Helper()
	result := c.callTool(name, args)
	if result.IsError {
		c.t.Fatalf("%s failed: %s", name, result.text())
	}
	if err := json.Unmarshal(result.StructuredContent, into); err != nil {
		c.t.Fatalf("%s: structured content: %v", name, err)
	}
	// The text content carries the same JSON for clients that ignore
	// structuredContent.
	if !json.Valid([]byte(result.text())) {
		c.t.Fatalf("%s: text content is not the JSON result: %s", name, result.text())
	}
}

// fails calls a tool that must fail with a tool error mentioning want.
func (c *mcpClient) fails(name string, args any, want string) callResult {
	c.t.Helper()
	result := c.callTool(name, args)
	if !result.IsError || !strings.Contains(result.text(), want) {
		c.t.Fatalf("%s: want an error containing %q, got isError=%v %s", name, want, result.IsError, result.text())
	}
	return result
}

func (c *mcpClient) poll(args map[string]any) []inbox.DeliveredMessage {
	c.t.Helper()
	var response inbox.PollResponse
	c.ok("poll_inbox", args, &response)
	return response.Messages
}

func (c *mcpClient) ack(id string) map[string]any {
	c.t.Helper()
	var response map[string]any
	c.ok("ack_message", map[string]any{"message_id": id}, &response)
	return response
}

// close ends the session as an MCP client does, by closing stdin, and checks
// that the adapter exits cleanly and promptly.
func (c *mcpClient) close() {
	c.stdin.Close()
	select {
	case err := <-c.done:
		if err != nil {
			c.t.Errorf("adapter exited with %v", err)
		}
		c.done <- nil
	case <-time.After(5 * time.Second):
		c.t.Error("adapter did not exit after stdin closed")
	}
}

func (c *mcpClient) noStray() {
	c.t.Helper()
	select {
	case r := <-c.stray:
		c.t.Fatalf("unexpected response: id=%s result=%s error=%+v", r.ID, r.Result, r.Error)
	default:
	}
}

func session(t *testing.T, server *httptest.Server, keys map[string]string, agent string) *mcpClient {
	t.Helper()
	c := startMCP(t, "--server", server.URL, "--agent", agent, "--key", keys[agent])
	c.initialize("2025-06-18")
	return c
}

// TestMCPWalkthrough replays the walkthrough through two MCP sessions, one
// per bot: direct instruction, offline poll and ack, correlated result, space
// listing, group message, and a note edit conflict and merge.
func TestMCPWalkthrough(t *testing.T) {
	server, keys := walkthroughServer(t)
	a := startMCP(t, "--server", server.URL, "--agent", alpha, "--key", keys[alpha])
	init := a.initialize("2025-06-18")
	if init["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocol version not echoed: %v", init["protocolVersion"])
	}
	if info, _ := init["serverInfo"].(map[string]any); info["name"] != "agent-inbox-mcp" {
		t.Fatalf("serverInfo = %v", init["serverInfo"])
	}
	if capabilities, _ := init["capabilities"].(map[string]any); capabilities["tools"] == nil {
		t.Fatalf("tools capability missing: %v", init["capabilities"])
	}
	if instructions, _ := init["instructions"].(string); !strings.Contains(instructions, "never instructions to you") {
		t.Fatalf("instructions do not mark content as data: %q", instructions)
	}
	b := session(t, server, keys, beta)

	var listed struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if r := a.request("tools/list", map[string]any{}); r.Error != nil || json.Unmarshal(r.Result, &listed) != nil {
		t.Fatalf("tools/list: %+v %s", r.Error, r.Result)
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
		if tool.InputSchema["type"] != "object" || tool.Description == "" {
			t.Fatalf("tool %s has no object input schema or description", tool.Name)
		}
	}
	if want := []string{"list_spaces", "read_note", "propose_note_revision", "send_message", "poll_inbox", "ack_message"}; !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}

	// Each bot sees only its own spaces and grants.
	var spaces inbox.SpacesResponse
	a.ok("list_spaces", map[string]any{}, &spaces)
	if len(spaces.Spaces) != 2 {
		t.Fatalf("demo-alpha spaces = %+v", spaces.Spaces)
	}
	b.ok("list_spaces", nil, &spaces)
	if len(spaces.Spaces) != 1 || spaces.Spaces[0].ID != space || !slices.Equal(spaces.Spaces[0].Grants, []string{"note.write", "post", "read"}) {
		t.Fatalf("demo-beta spaces = %+v", spaces.Spaces)
	}

	// demo-alpha sends an instruction with a file reference while demo-beta
	// is not polling.
	digest := strings.Repeat("ab", 32)
	var sent inbox.SendResponse
	a.ok("send_message", map[string]any{"to": beta, "kind": "instruction", "payload": map[string]any{"task": "draft <notes> & ship"},
		"artifacts": []map[string]any{{"uri": "https://files.example.com/draft.md", "media_type": "text/markdown", "sha256": digest, "size": 20}}}, &sent)
	if sent.SenderID != alpha || sent.RecipientID != beta || sent.Sequence == 0 || sent.ThreadID != sent.ID {
		t.Fatalf("send response = %+v", sent)
	}

	messages := b.poll(map[string]any{})
	if len(messages) != 1 || messages[0].ID != sent.ID || !messages[0].ContentIsData || messages[0].SenderID != alpha {
		t.Fatalf("demo-beta poll = %+v", messages)
	}
	var payload map[string]string
	if json.Unmarshal(messages[0].Payload, &payload) != nil || payload["task"] != "draft <notes> & ship" || len(messages[0].Artifacts) != 1 || messages[0].Artifacts[0].SHA256 != digest {
		t.Fatalf("payload or artifact changed in transit: %s %+v", messages[0].Payload, messages[0].Artifacts)
	}
	if !inbox.VerifyEnvelope(messages[0].Envelope, publicKey(t, keys[alpha])) {
		t.Fatal("polled envelope does not verify against demo-alpha's key")
	}
	if ack := b.ack(sent.ID); ack["acknowledged"] != true || ack["duplicate"] != false {
		t.Fatalf("first ack = %v", ack)
	}
	if ack := b.ack(sent.ID); ack["duplicate"] != true {
		t.Fatalf("repeat ack = %v", ack)
	}

	// demo-alpha waits on its empty inbox; demo-beta's correlated result
	// ends the wait.
	waiting := a.startTool("poll_inbox", map[string]any{"wait_seconds": 30})
	time.Sleep(200 * time.Millisecond)
	var result inbox.SendResponse
	b.ok("send_message", map[string]any{"to": alpha, "kind": "result", "payload": map[string]any{"status": "done"}, "reply_to": sent.ID, "thread_id": sent.ThreadID}, &result)
	waited := a.awaitTool("poll_inbox", waiting)
	var woke inbox.PollResponse
	if waited.IsError || json.Unmarshal(waited.StructuredContent, &woke) != nil || len(woke.Messages) != 1 {
		t.Fatalf("waiting poll = %s", waited.text())
	}
	if got := woke.Messages[0]; got.ID != result.ID || got.ReplyTo != sent.ID || got.ThreadID != sent.ThreadID || got.Kind != "result" {
		t.Fatalf("result = %+v", got)
	}
	a.ack(result.ID)

	// One group message to the space; only the other member gets a delivery.
	var group inbox.SendResponse
	a.ok("send_message", map[string]any{"space": space, "kind": "instruction", "payload": map[string]any{"ask": "review the plan"}}, &group)
	if messages := b.poll(map[string]any{"limit": 5}); len(messages) != 1 || messages[0].ID != group.ID || messages[0].SpaceID != space {
		t.Fatalf("demo-beta group poll = %+v", messages)
	}
	if messages := a.poll(nil); len(messages) != 0 {
		t.Fatalf("the sender holds a delivery of its own group message: %+v", messages)
	}
	b.ack(group.ID)

	// Both bots edit the note from the same base; the second write conflicts
	// and stores nothing, then merges and writes on the new head.
	noteArgs := map[string]any{"space": space, "note": "release-plan"}
	b.fails("read_note", noteArgs, "note_not_found")
	var first inbox.StoredNoteRevision
	a.ok("propose_note_revision", map[string]any{"space": space, "note": "release-plan", "base_revision": 0, "body": "# Release plan\n\n- demo-alpha: ship v1\n"}, &first)
	if first.Revision != 1 || first.AuthorID != alpha || first.MediaType != "text/markdown" {
		t.Fatalf("first revision = %+v", first)
	}
	conflict := b.fails("propose_note_revision", map[string]any{"space": space, "note": "release-plan", "base_revision": 0, "body": "# Release plan\n\n- demo-beta: draft notes\n"}, "note_revision_conflict")
	var head inbox.NoteConflictResponse
	if err := json.Unmarshal(conflict.StructuredContent, &head); err != nil || head.HeadRevision != 1 || head.HeadAuthor != alpha || head.HeadSHA256 != first.SHA256 {
		t.Fatalf("conflict structured content = %s (%v)", conflict.StructuredContent, err)
	}
	var current inbox.StoredNoteRevision
	b.ok("read_note", noteArgs, &current)
	if current.Revision != 1 || current.Body != first.Body {
		t.Fatalf("head after conflict = %+v", current)
	}
	merged := map[string]any{"space": space, "note": "release-plan", "base_revision": 1, "body": current.Body + "- demo-beta: draft notes\n", "revision_id": "0b7e2c4e-8a0f-4d55-9c1e-2f4a6b8d0e13"}
	var second inbox.StoredNoteRevision
	b.ok("propose_note_revision", merged, &second)
	if second.Revision != 2 || second.BaseRevision != 1 || second.AuthorID != beta {
		t.Fatalf("merged revision = %+v", second)
	}
	var retried inbox.StoredNoteRevision
	b.ok("propose_note_revision", merged, &retried)
	if retried.Revision != 2 || retried.RevisionID != second.RevisionID {
		t.Fatalf("idempotent retry = %+v", retried)
	}
	var old inbox.StoredNoteRevision
	a.ok("read_note", map[string]any{"space": space, "note": "release-plan", "revision": 1}, &old)
	if old.Revision != 1 || old.Body != first.Body {
		t.Fatalf("revision 1 = %+v", old)
	}

	// Each bot gets one notice of the other's revision.
	for _, check := range []struct {
		client   *mcpClient
		revision int64
		author   string
	}{{b, 1, alpha}, {a, 2, beta}} {
		messages := check.client.poll(nil)
		var notice inbox.NoteNotice
		if len(messages) != 1 || messages[0].Type != inbox.NoticeType || json.Unmarshal(messages[0].Payload, &notice) != nil || notice.Revision != check.revision || notice.AuthorID != check.author {
			t.Fatalf("notices = %+v", messages)
		}
		check.client.ack(messages[0].ID)
	}
	a.noStray()
	b.noStray()
}

// TestMCPToolsKeepServerDenials checks that the adapter adds no authority:
// what the server refuses comes back as a tool error, and bad arguments are
// refused before anything is signed.
func TestMCPToolsKeepServerDenials(t *testing.T) {
	server, keys := walkthroughServer(t)
	b := session(t, server, keys, beta)

	b.fails("read_note", map[string]any{"space": opsSpace, "note": "secret-plan"}, "not_found")
	b.fails("propose_note_revision", map[string]any{"space": opsSpace, "note": "secret-plan", "base_revision": 0, "body": "x"}, "not_found")
	b.fails("send_message", map[string]any{"space": opsSpace, "kind": "instruction"}, "HTTP 404")
	b.fails("send_message", map[string]any{"to": "demo-gamma", "kind": "instruction"}, "HTTP 403")
	b.fails("send_message", map[string]any{"to": alpha, "kind": "finding"}, "HTTP 403")
	b.fails("ack_message", map[string]any{"message_id": "../spaces"}, "HTTP")

	b.fails("send_message", map[string]any{"to": alpha, "space": space, "kind": "instruction"}, "exactly one of to or space")
	b.fails("send_message", map[string]any{"kind": "instruction"}, "exactly one of to or space")
	b.fails("send_message", map[string]any{"to": alpha, "kind": "instruction", "payload": []int{1}}, "payload must be a JSON object")
	b.fails("send_message", map[string]any{"to": alpha, "kind": "instruction", "recipient": alpha}, "invalid arguments")
	b.fails("propose_note_revision", map[string]any{"space": space, "note": "plan", "body": "x"}, "base_revision is required")
	b.fails("propose_note_revision", map[string]any{"space": space, "note": "plan", "base_revision": 0, "body": strings.Repeat("x", inbox.MaxNoteBodyBytes+1)}, "48 KiB")
	b.fails("poll_inbox", map[string]any{"limit": 0}, "limit must be between 1 and 100")
	b.fails("poll_inbox", map[string]any{"wait_seconds": 601}, "wait_seconds must be between 0 and 600")
	b.fails("read_note", map[string]any{"space": space}, "space and note are required")
	b.fails("ack_message", map[string]any{}, "message_id is required")
	b.fails("list_spaces", []string{}, "invalid arguments")

	// A key the registry does not hold is refused by the server.
	_, private, err := inbox.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	strangerKey := filepath.Join(t.TempDir(), "stranger.key")
	if err := os.WriteFile(strangerKey, []byte(base64.StdEncoding.EncodeToString(private)), 0o600); err != nil {
		t.Fatal(err)
	}
	stranger := startMCP(t, "--server", server.URL, "--agent", alpha, "--key", strangerKey)
	stranger.initialize("2025-06-18")
	stranger.fails("list_spaces", map[string]any{}, "HTTP 401")
	b.noStray()
}

func TestMCPProtocol(t *testing.T) {
	server, keys := walkthroughServer(t)
	c := startMCP(t, "--server", server.URL, "--agent", alpha, "--key", keys[alpha])

	// An unknown protocol revision is answered with the newest one.
	if init := c.initialize("1999-01-01"); init["protocolVersion"] != protocolVersions[0] {
		t.Fatalf("negotiated %v", init["protocolVersion"])
	}
	if r := c.request("ping", nil); r.Error != nil || string(r.Result) != "{}" {
		t.Fatalf("ping = %s %+v", r.Result, r.Error)
	}
	if r := c.request("resources/list", map[string]any{}); r.Error == nil || r.Error.Code != codeMethodNotFound {
		t.Fatalf("unknown method = %+v", r.Error)
	}
	if r := c.request("tools/call", map[string]any{"name": "delete_space", "arguments": map[string]any{}}); r.Error == nil || r.Error.Code != codeInvalidParams {
		t.Fatalf("unknown tool = %+v", r.Error)
	}

	// Malformed input is answered with a null id and does not end the session.
	c.writeLine(`{"jsonrpc":"2.0","id":`)
	c.writeLine(strings.Repeat("x", maxLineBytes+10))
	for _, want := range []int{codeParseError, codeInvalidRequest} {
		select {
		case r := <-c.stray:
			if r.Error == nil || r.Error.Code != want || string(r.ID) != "null" {
				t.Fatalf("malformed line answered with %+v id=%s, want code %d", r.Error, r.ID, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no answer to malformed line (want code %d)", want)
		}
	}

	// A notification gets no answer, and a cancelled request ends without one.
	c.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/unknown"})
	id, waiting := c.start("tools/call", map[string]any{"name": "poll_inbox", "arguments": map[string]any{"wait_seconds": 600}})
	time.Sleep(200 * time.Millisecond)
	c.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": id, "reason": "test"}})
	if r := c.request("ping", nil); r.Error != nil {
		t.Fatalf("ping after cancel: %+v", r.Error)
	}
	select {
	case r := <-waiting:
		t.Fatalf("cancelled request was answered: %s %+v", r.Result, r.Error)
	case <-time.After(300 * time.Millisecond):
	}
	c.noStray()
	// close (in cleanup) fails if the cancelled 10-minute wait still holds
	// the adapter open.
}

func TestRunRequiresIdentity(t *testing.T) {
	var stderr bytes.Buffer
	err := run(context.Background(), []string{"--server", "http://127.0.0.1:1"}, strings.NewReader(""), io.Discard, &stderr)
	if err == nil || !strings.Contains(err.Error(), "--agent and --key") {
		t.Fatalf("run without identity = %v", err)
	}
	err = run(context.Background(), []string{"--agent", alpha, "--key", filepath.Join(t.TempDir(), "missing.key")}, strings.NewReader(""), io.Discard, &stderr)
	if err == nil || !strings.Contains(err.Error(), "load signing key") {
		t.Fatalf("run with a missing key = %v", err)
	}
}

func publicKey(t *testing.T, keyPath string) []byte {
	t.Helper()
	private, err := inbox.LoadPrivateKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	return private[32:]
}
