package inbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func (s *testSystem) auditActions(t *testing.T, action string) []AuditEntry {
	t.Helper()
	entries, err := s.store.AuditEntries(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	matched := make([]AuditEntry, 0)
	for i := len(entries) - 1; i >= 0; i-- { // Oldest first.
		if entries[i].Action == action {
			matched = append(matched, entries[i])
		}
	}
	return matched
}

func (s *testSystem) setRegistry(t *testing.T, edit func(*Registry)) {
	t.Helper()
	edit(&s.registry)
	writeRegistry(t, s.registryFile, s.registry)
}

func (s *testSystem) agentIndex(id string) int {
	for i, agent := range s.registry.Agents {
		if agent.ID == id {
			return i
		}
	}
	return -1
}

func registryFileSHA(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func detailList(entry AuditEntry, key string) []string {
	values, _ := entry.Detail[key].([]any)
	out := make([]string, 0, len(values))
	for _, value := range values {
		text, _ := value.(string)
		out = append(out, text)
	}
	return out
}

func requireList(t *testing.T, entry AuditEntry, key string, want ...string) {
	t.Helper()
	got := detailList(entry, key)
	if len(got) != len(want) {
		t.Fatalf("registry.changed %s = %v, want %v (detail %+v)", key, got, want, entry.Detail)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("registry.changed %s = %v, want %v (detail %+v)", key, got, want, entry.Detail)
		}
	}
}

func (s *testSystem) pollStatus(t *testing.T, clientID string) (int, []byte) {
	t.Helper()
	data, status, err := s.clients[clientID].Do(context.Background(), http.MethodGet, "/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	return status, data
}

func TestRegistryChangeIsAuditedOnce(t *testing.T) {
	s := newTestSystem(t)
	ctx := context.Background()
	s.poll(t, "agent-a")
	s.poll(t, "agent-b")
	changes := s.auditActions(t, "registry.changed")
	if len(changes) != 1 {
		t.Fatalf("first observation did not record exactly one baseline: %+v", changes)
	}
	baseline := changes[0]
	firstSHA := registryFileSHA(t, s.registryFile)
	if baseline.Detail["old_sha256"] != "" || baseline.Detail["new_sha256"] != firstSHA || baseline.ActorID != "system" || baseline.Outcome != "recorded" {
		t.Fatalf("baseline registry.changed has wrong hashes or attribution: %+v", baseline)
	}
	requireList(t, baseline, "added", "agent-a", "agent-b", "agent-c", "agent-revoked")

	s.setRegistry(t, func(r *Registry) {
		r.Agents[s.agentIndex("agent-b")].Disabled = true
		r.Agents[s.agentIndex("agent-c")].PublicKeys[0].Disabled = true
		r.Agents[s.agentIndex("agent-a")].AllowedKinds = []string{"instruction"}
		r.Agents[s.agentIndex("agent-revoked")].Disabled = false
		r.Agents = append(r.Agents, testRegistryAgent("agent-d", "tenant-one", s.agents["ghost"].public, nil, []string{"result"}, false))
	})
	for range 3 {
		s.poll(t, "agent-a")
	}
	s.service.dispatchDue(ctx)
	s.service.dispatchDue(ctx)
	changes = s.auditActions(t, "registry.changed")
	if len(changes) != 2 {
		t.Fatalf("one registry edit was not audited exactly once: %+v", changes)
	}
	edit := changes[1]
	secondSHA := registryFileSHA(t, s.registryFile)
	if edit.Detail["old_sha256"] != firstSHA || edit.Detail["new_sha256"] != secondSHA {
		t.Fatalf("registry.changed did not chain hashes: %+v", edit.Detail)
	}
	requireList(t, edit, "added", "agent-d")
	requireList(t, edit, "removed")
	requireList(t, edit, "disabled", "agent-b")
	requireList(t, edit, "enabled", "agent-revoked")
	requireList(t, edit, "keys_changed", "agent-c")
	requireList(t, edit, "policy_changed", "agent-a")

	// A restarted service and a second instance on the same store do not
	// record the same registry again.
	restarted, err := NewServer(s.store, FileRegistry{Path: s.registryFile}, s.notifier, DefaultServerConfig())
	if err != nil {
		t.Fatal(err)
	}
	restarted.observeRegistry(ctx)
	other, err := NewServer(s.store, FileRegistry{Path: s.registryFile}, s.notifier, DefaultServerConfig())
	if err != nil {
		t.Fatal(err)
	}
	s.setRegistry(t, func(r *Registry) { r.Agents = r.Agents[:len(r.Agents)-1] })
	other.observeRegistry(ctx)
	restarted.observeRegistry(ctx)
	s.service.dispatchDue(ctx)
	changes = s.auditActions(t, "registry.changed")
	if len(changes) != 3 {
		t.Fatalf("restart or a second instance duplicated registry.changed: %+v", changes)
	}
	requireList(t, changes[2], "removed", "agent-d")
	if changes[2].Detail["old_sha256"] != secondSHA {
		t.Fatalf("second instance did not chain from the recorded hash: %+v", changes[2].Detail)
	}

	if err := os.WriteFile(s.registryFile, []byte(`{"version":1,"agents":[`), 0o600); err != nil {
		t.Fatal(err)
	}
	s.service.dispatchDue(ctx)
	if got := s.auditActions(t, "registry.changed"); len(got) != 3 {
		t.Fatalf("an unreadable registry was recorded as a change: %+v", got)
	}
}

func TestRevokedCredentialUseIsAuditedAndRateCapped(t *testing.T) {
	s := newTestSystem(t)
	_, unknownBody := s.pollStatus(t, "ghost")
	for range 3 {
		status, body := s.pollStatus(t, "agent-revoked")
		if status != http.StatusUnauthorized || !bytes.Equal(body, unknownBody) {
			t.Fatalf("revoked agent response differed from an unknown agent: HTTP %d %s", status, body)
		}
	}
	rows := s.auditActions(t, "auth.revoked_credential")
	if len(rows) != 1 {
		t.Fatalf("revoked agent use was not audited exactly once per window: %+v", rows)
	}
	if row := rows[0]; row.ActorID != "agent-revoked" || row.TenantID != "tenant-one" || row.Outcome != "rejected" || row.Code != "agent_disabled" || row.Detail["key_id"] != "primary" || row.Detail["method"] != http.MethodGet || row.Detail["path"] != "/v1/messages" {
		t.Fatalf("revoked credential row is missing attribution: %+v", row)
	}
	if changes := s.auditActions(t, "registry.changed"); len(changes) != 1 {
		t.Fatalf("revoked credential row was not preceded by the registry state it was judged against: %+v", changes)
	}

	s.setRegistry(t, func(r *Registry) { r.Agents[s.agentIndex("agent-b")].PublicKeys[0].Disabled = true })
	for range 2 {
		if status, _ := s.pollStatus(t, "agent-b"); status != http.StatusUnauthorized {
			t.Fatalf("disabled key still authenticated: HTTP %d", status)
		}
	}
	rows = s.auditActions(t, "auth.revoked_credential")
	if len(rows) != 2 || rows[1].ActorID != "agent-b" || rows[1].Code != "key_disabled" {
		t.Fatalf("disabled key use was not audited once with key_disabled: %+v", rows)
	}

	s.service.now = func() time.Time { return time.Now().Add(revokedCredentialAuditInterval + time.Second) }
	if status, _ := s.pollStatus(t, "agent-revoked"); status != http.StatusUnauthorized {
		t.Fatalf("revoked agent authenticated after the cap window: HTTP %d", status)
	}
	if rows = s.auditActions(t, "auth.revoked_credential"); len(rows) != 3 || rows[2].ActorID != "agent-revoked" {
		t.Fatalf("revoked credential use was not audited again after the cap window: %+v", rows)
	}
}

func TestUnknownKeyAttemptsStayUnaudited(t *testing.T) {
	s := newTestSystem(t)
	s.setRegistry(t, func(r *Registry) { r.Agents[s.agentIndex("agent-b")].PublicKeys = nil })
	attempts := []*Client{
		s.clients["ghost"], // not listed at all
		{Server: s.server.URL, Agent: "agent-a", KeyID: "missing", Private: s.agents["agent-a"].private},             // unlisted key
		{Server: s.server.URL, Agent: "agent-revoked", KeyID: "primary", Private: s.agents["agent-a"].private},       // disabled key, forged signature
		{Server: s.server.URL, Agent: "agent-revoked", KeyID: "missing", Private: s.agents["agent-revoked"].private}, // disabled agent, unlisted key
		s.clients["agent-b"], // key removed rather than disabled
	}
	for i, client := range attempts {
		_, status, err := client.Do(context.Background(), http.MethodGet, "/v1/messages", nil)
		if err != nil || status != http.StatusUnauthorized {
			t.Fatalf("attempt %d was not rejected: HTTP %d err=%v", i, status, err)
		}
	}
	entries, err := s.store.AuditEntries(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("callers without a listed key wrote durable audit rows: %+v", entries)
	}
}

func TestPendingMessageToRevokedRecipientIsDispositioned(t *testing.T) {
	s := newTestSystem(t)
	ctx := context.Background()
	base := time.Now().UTC()
	s.service.now = func() time.Time { return base }
	s.service.config.RetryInterval = time.Second
	s.service.config.MaxDoorbellAttempts = 2
	pending := s.envelope(t, "agent-a", "agent-b", "instruction", `{"task":"pending"}`)
	acked := s.envelope(t, "agent-a", "agent-b", "instruction", `{"task":"done"}`)
	for _, e := range []Envelope{pending, acked} {
		if _, status, apiErr := s.send(t, e, "agent-a"); apiErr != nil || status != http.StatusCreated {
			t.Fatalf("send failed: HTTP %d %+v", status, apiErr)
		}
	}
	if status, code := s.ack(t, "agent-b", acked.ID, true); status != http.StatusOK {
		t.Fatalf("ack failed: HTTP %d %s", status, code)
	}
	bells, remove := s.service.hub.subscribe("agent-b")
	defer remove()

	s.setRegistry(t, func(r *Registry) { r.Agents[s.agentIndex("agent-b")].Disabled = true })
	for range 4 {
		base = base.Add(2 * time.Second)
		s.service.dispatchDue(ctx)
	}
	select {
	case event := <-bells:
		t.Fatalf("revoked recipient was still rung: %+v", event)
	default:
	}
	if got := s.notifier.count("delivery.recipient_revoked"); got != 1 {
		t.Fatalf("revoked recipient produced %d disposition notifications, want 1", got)
	}
	if got := s.notifier.count("message.unacknowledged_escalation"); got != 0 {
		t.Fatalf("revoked recipient's work was escalated as merely unacknowledged: %d", got)
	}
	s.notifier.mu.Lock()
	for _, event := range s.notifier.events {
		if event.Event == "delivery.recipient_revoked" && (event.MessageID != pending.ID || event.RecipientID != "agent-b" || event.TenantID != "tenant-one" || event.Code != "unknown_recipient") {
			t.Errorf("disposition notification has wrong fields: %+v", event)
		}
	}
	s.notifier.mu.Unlock()
	rows := s.auditActions(t, "delivery.recipient_revoked")
	if len(rows) != 1 || rows[0].SubjectID != pending.ID || rows[0].Code != "unknown_recipient" || rows[0].Detail["recipient_id"] != "agent-b" || rows[0].ActorID != "system" {
		t.Fatalf("disposition was not audited once with the message and recipient: %+v", rows)
	}
	changes := s.auditActions(t, "registry.changed")
	if len(changes) == 0 || changes[len(changes)-1].Sequence > rows[0].Sequence {
		t.Fatalf("disposition was audited before the registry change that caused it: changes=%+v disposition=%+v", changes, rows)
	}

	// Nothing is deleted: re-enabling the recipient lets it collect and ack the
	// work, and the disposition does not restart doorbells or escalate.
	s.setRegistry(t, func(r *Registry) { r.Agents[s.agentIndex("agent-b")].Disabled = false })
	for range 3 {
		base = base.Add(2 * time.Second)
		s.service.dispatchDue(ctx)
	}
	select {
	case event := <-bells:
		t.Fatalf("dispositioned message resumed doorbells: %+v", event)
	default:
	}
	if s.notifier.count("message.unacknowledged_escalation") != 0 || s.notifier.count("delivery.recipient_revoked") != 1 {
		t.Fatal("dispositioned message was escalated or dispositioned again")
	}
	if got := s.poll(t, "agent-b"); len(got.Messages) != 1 || got.Messages[0].ID != pending.ID {
		t.Fatalf("dispositioned message was not kept for its recipient: %+v", got.Messages)
	}
	if status, code := s.ack(t, "agent-b", pending.ID, true); status != http.StatusOK {
		t.Fatalf("dispositioned message could not be acknowledged: HTTP %d %s", status, code)
	}
}

func TestUnreadableRegistryDoesNotDispositionPendingWork(t *testing.T) {
	s := newTestSystem(t)
	base := time.Now().UTC()
	s.service.now = func() time.Time { return base }
	s.service.config.RetryInterval = time.Second
	if _, status, apiErr := s.send(t, s.envelope(t, "agent-a", "agent-b", "instruction", `{"x":1}`), "agent-a"); apiErr != nil || status != http.StatusCreated {
		t.Fatalf("send failed: HTTP %d %+v", status, apiErr)
	}
	if err := os.WriteFile(s.registryFile, []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	base = base.Add(2 * time.Second)
	s.service.dispatchDue(context.Background())
	if got := s.notifier.count("delivery.recipient_revoked"); got != 0 {
		t.Fatalf("an unreadable registry was treated as a revocation: %d", got)
	}
}

func TestRejectedSendAuditCarriesMessageID(t *testing.T) {
	s := newTestSystem(t)
	cases := []struct{ sender, recipient, kind, code string }{
		{"agent-a", "agent-c", "instruction", "wrong_tenant"},
		{"agent-c", "agent-b", "instruction", "recipient_not_allowed"},
		{"agent-a", "agent-b", "status", "kind_not_allowed"},
	}
	for _, test := range cases {
		e := s.envelope(t, test.sender, test.recipient, test.kind, `{"x":1}`)
		if _, status, apiErr := s.send(t, e, test.sender); apiErr == nil || status < 400 {
			t.Fatalf("%s send was accepted: HTTP %d", test.code, status)
		}
		found := false
		for _, row := range s.auditActions(t, "send") {
			if row.Outcome == "rejected" && row.Code == test.code {
				found = true
				if row.SubjectID != e.ID {
					t.Fatalf("rejected %s audit omitted the message ID: %+v", test.code, row)
				}
			}
		}
		if !found {
			t.Fatalf("rejected %s send was not audited", test.code)
		}
	}
}

func TestRevokedAuthorMessagesStayReadableAndFlagged(t *testing.T) {
	s := newTestSystem(t)
	fromB := s.envelope(t, "agent-b", "agent-a", "instruction", `{"from":"b"}`)
	if _, status, apiErr := s.send(t, fromB, "agent-b"); apiErr != nil || status != http.StatusCreated {
		t.Fatalf("send failed: HTTP %d %+v", status, apiErr)
	}
	if status, data := s.pollStatus(t, "agent-a"); status != http.StatusOK || bytes.Contains(data, []byte("author_status")) {
		t.Fatalf("active author was flagged: HTTP %d %s", status, data)
	}
	s.setRegistry(t, func(r *Registry) { r.Agents[s.agentIndex("agent-b")].Disabled = true })
	got := s.poll(t, "agent-a")
	if len(got.Messages) != 1 || got.Messages[0].ID != fromB.ID || got.Messages[0].SenderID != "agent-b" || got.Messages[0].AuthorStatus != AuthorStatusRevoked {
		t.Fatalf("revoked author's message was not kept, attributed and flagged: %+v", got.Messages)
	}
	if !VerifyEnvelope(got.Messages[0].Envelope, s.agents["agent-b"].public) {
		t.Fatal("revoked author's stored signature no longer verifies")
	}
	if status, code := s.ack(t, "agent-a", fromB.ID, true); status != http.StatusOK {
		t.Fatalf("revoked author's message could not be acknowledged: HTTP %d %s", status, code)
	}
}

const preSliceTwoSQLiteSchema = `
CREATE TABLE messages (
	sequence INTEGER PRIMARY KEY AUTOINCREMENT,
	id TEXT NOT NULL UNIQUE,
	sender_id TEXT NOT NULL,
	recipient_id TEXT NOT NULL,
	tenant_id TEXT NOT NULL,
	kind TEXT NOT NULL,
	task_id TEXT NOT NULL,
	thread_id TEXT NOT NULL,
	reply_to TEXT NOT NULL,
	envelope_json TEXT NOT NULL,
	accepted_at TEXT NOT NULL,
	acknowledged_at TEXT
);
CREATE TABLE notification_state (
	message_id TEXT PRIMARY KEY REFERENCES messages(id),
	attempts INTEGER NOT NULL DEFAULT 0,
	last_notified_at TEXT,
	escalated_at TEXT
);
INSERT INTO messages(id, sender_id, recipient_id, tenant_id, kind, task_id, thread_id, reply_to, envelope_json, accepted_at)
	VALUES('pre-slice-message', 'agent-a', 'agent-b', 'tenant-one', 'instruction', 'task', 'thread', '', '{}', '2026-01-01T00:00:00Z');
INSERT INTO notification_state(message_id, attempts) VALUES('pre-slice-message', 1);
`

const preSliceTwoPostgresSchema = `
CREATE TABLE messages (
	sequence BIGSERIAL PRIMARY KEY,
	id TEXT NOT NULL UNIQUE,
	sender_id TEXT NOT NULL,
	recipient_id TEXT NOT NULL,
	tenant_id TEXT NOT NULL,
	kind TEXT NOT NULL,
	task_id TEXT NOT NULL,
	thread_id TEXT NOT NULL,
	reply_to TEXT NOT NULL,
	envelope_json TEXT NOT NULL,
	accepted_at TIMESTAMPTZ NOT NULL,
	acknowledged_at TIMESTAMPTZ
);
CREATE TABLE notification_state (
	message_id TEXT PRIMARY KEY REFERENCES messages(id),
	attempts INTEGER NOT NULL DEFAULT 0,
	last_notified_at TIMESTAMPTZ,
	escalated_at TIMESTAMPTZ
);
INSERT INTO messages(id, sender_id, recipient_id, tenant_id, kind, task_id, thread_id, reply_to, envelope_json, accepted_at)
	VALUES('pre-slice-message', 'agent-a', 'agent-b', 'tenant-one', 'instruction', 'task', 'thread', '', '{}', '2026-01-01T00:00:00Z');
INSERT INTO notification_state(message_id, attempts) VALUES('pre-slice-message', 1);
`

func TestPreSliceDatabaseMigratesToNotificationDisposition(t *testing.T) {
	ctx := context.Background()
	check := func(t *testing.T, open func() (Store, error)) {
		t.Helper()
		var store Store
		for range 2 { // The migration is idempotent across restarts.
			var err error
			if store != nil {
				store.Close()
			}
			if store, err = open(); err != nil {
				t.Fatalf("open pre-slice database: %v", err)
			}
		}
		defer store.Close()
		due, err := store.DueNotifications(ctx, time.Now(), time.Second, 3)
		if err != nil || len(due) != 1 || due[0].MessageID != "pre-slice-message" || due[0].Attempts != 1 {
			t.Fatalf("pre-slice pending message is not due after migration: %+v err=%v", due, err)
		}
		if state, err := store.RegistryState(ctx); err != nil || state.SHA256 != "" {
			t.Fatalf("migrated database has unexpected registry state %q: %v", state.SHA256, err)
		}
		marked, err := store.MarkRecipientRevoked(ctx, "pre-slice-message", "agent-b", "unknown_recipient", time.Now())
		if err != nil || !marked {
			t.Fatalf("pre-slice message could not be dispositioned: marked=%t err=%v", marked, err)
		}
		if due, err := store.DueNotifications(ctx, time.Now(), time.Second, 3); err != nil || len(due) != 0 {
			t.Fatalf("dispositioned message is still due: %+v err=%v", due, err)
		}
		if marked, err := store.MarkEscalated(ctx, "pre-slice-message", "agent-b", time.Now()); err != nil || marked {
			t.Fatalf("dispositioned message was escalated: marked=%t err=%v", marked, err)
		}
	}
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pre-slice.db")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(preSliceTwoSQLiteSchema); err != nil {
			db.Close()
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		check(t, func() (Store, error) { return OpenSQLite(path) })
	})
	t.Run("postgres", func(t *testing.T) {
		databaseURL := os.Getenv("INBOX_TEST_POSTGRES_URL")
		if databaseURL == "" {
			t.Skip("set INBOX_TEST_POSTGRES_URL to run the PostgreSQL migration test")
		}
		isolated := isolatedPostgresURL(t, databaseURL)
		db, err := sql.Open("pgx", isolated)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(preSliceTwoPostgresSchema); err != nil {
			db.Close()
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		check(t, func() (Store, error) { return OpenPostgres(isolated) })
	})
}

func TestRegistryChangeDetailIsJSONStable(t *testing.T) {
	detail := registryChangeDetail("", "abc", RegistrySnapshot{}, RegistrySnapshot{Agents: map[string]RegistryAgent{"b": {}, "a": {}}})
	encoded, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"added":["a","b"],"disabled":[],"enabled":[],"keys_changed":[],"memberships_changed":[],"new_sha256":"abc","old_sha256":"","policy_changed":[],"removed":[],"spaces_added":[],"spaces_changed":[],"spaces_removed":[]}`
	if string(encoded) != want {
		t.Fatalf("registry.changed detail = %s, want %s", encoded, want)
	}
}
