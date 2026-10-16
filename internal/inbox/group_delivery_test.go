package inbox

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// newGroupSystem adds agent-d to tenant-one and a release-notes space where
// agent-a and agent-b hold read and post and agent-d holds read only.
// agent-revoked is a disabled member holding read.
func newGroupSystem(t *testing.T) *testSystem {
	t.Helper()
	s := newTestSystem(t)
	s.addAgent(t, "agent-d", "tenant-one")
	s.setRegistry(t, func(r *Registry) {
		r.Version = 2
		r.Spaces = []RegistrySpace{groupSpace(
			RegistrySpaceMember{Agent: "agent-a", Grants: []string{"read", "post"}},
			RegistrySpaceMember{Agent: "agent-b", Grants: []string{"read", "post"}},
			RegistrySpaceMember{Agent: "agent-d", Grants: []string{"read"}},
			RegistrySpaceMember{Agent: "agent-revoked", Grants: []string{"read"}},
		)}
	})
	return s
}

func groupSpace(members ...RegistrySpaceMember) RegistrySpace {
	return RegistrySpace{ID: "release-notes", TenantID: "tenant-one", Members: members}
}

// setMembers replaces the release-notes members.
func (s *testSystem) setMembers(t *testing.T, members ...RegistrySpaceMember) {
	t.Helper()
	s.setRegistry(t, func(r *Registry) { r.Spaces = []RegistrySpace{groupSpace(members...)} })
}

// addAgent generates a key for a new agent and lists it in s.registry; the
// caller writes the registry.
func (s *testSystem) addAgent(t *testing.T, id, tenant string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s.agents[id] = testAgent{public: public, private: private}
	s.clients[id] = &Client{Server: s.server.URL, Agent: id, KeyID: "primary", Private: private}
	s.registry.Agents = append(s.registry.Agents, testRegistryAgent(id, tenant, public, []string{}, []string{"instruction", "result"}, false))
}

func (s *testSystem) spaceEnvelope(t *testing.T, sender, space, kind, payload string) Envelope {
	t.Helper()
	e := s.envelope(t, sender, "", kind, payload)
	e.SpaceID = space
	if err := SignEnvelope(&e, s.agents[sender].private); err != nil {
		t.Fatal(err)
	}
	return e
}

func (s *testSystem) mustSend(t *testing.T, e Envelope, clientID string) DeliveredMessage {
	t.Helper()
	message, status, apiErr := s.send(t, e, clientID)
	if apiErr != nil || status != http.StatusCreated {
		t.Fatalf("send %s failed: HTTP %d %+v", e.ID, status, apiErr)
	}
	return message
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func polledIDs(response PollResponse) []string {
	ids := make([]string, 0, len(response.Messages))
	for _, message := range response.Messages {
		ids = append(ids, message.ID)
	}
	return ids
}

func (s *testSystem) requirePolled(t *testing.T, clientID string, want ...string) PollResponse {
	t.Helper()
	got := s.poll(t, clientID)
	if ids := polledIDs(got); !slices.Equal(ids, want) && !(len(ids) == 0 && len(want) == 0) {
		t.Fatalf("%s polled %v, want %v", clientID, ids, want)
	}
	return got
}

func (s *testSystem) sendAudit(t *testing.T, messageID string) AuditEntry {
	t.Helper()
	for _, entry := range s.auditActions(t, "send") {
		if entry.SubjectID == messageID && entry.Outcome == "accepted" {
			return entry
		}
	}
	t.Fatalf("no send accepted audit row for %s", messageID)
	return AuditEntry{}
}

func TestGroupSendSnapshotsAudience(t *testing.T) {
	s := newGroupSystem(t)
	bells := map[string]<-chan DoorbellEvent{}
	for _, id := range []string{"agent-a", "agent-b", "agent-d"} {
		ch, remove := s.service.hub.subscribe(id)
		defer remove()
		bells[id] = ch
	}
	sha := registryFileSHA(t, s.registryFile)
	e := s.spaceEnvelope(t, "agent-a", "release-notes", "instruction", `{"ask":"post your section"}`)
	sent := s.mustSend(t, e, "agent-a")
	if sent.SpaceID != "release-notes" || sent.RecipientID != "" || sent.TenantID != "tenant-one" {
		t.Fatalf("send response does not describe a group message: %+v", sent)
	}

	audit := s.sendAudit(t, e.ID)
	if audit.Detail["space_id"] != "release-notes" || audit.Detail["registry_sha256"] != sha {
		t.Fatalf("send audit does not name the space and registry: %+v", audit.Detail)
	}
	requireList(t, audit, "recipients", "agent-b", "agent-d")
	for _, id := range []string{"agent-b", "agent-d"} {
		select {
		case event := <-bells[id]:
			if event.MessageID != e.ID || event.Recipient != id || event.SpaceID != "release-notes" {
				t.Fatalf("%s doorbell has wrong fields: %+v", id, event)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s was not rung", id)
		}
		got := s.requirePolled(t, id, e.ID)
		if got.Messages[0].SpaceID != "release-notes" || got.Messages[0].RecipientID != "" {
			t.Fatalf("%s polled a message without its space: %+v", id, got.Messages[0])
		}
		if !VerifyEnvelope(got.Messages[0].Envelope, s.agents["agent-a"].public) {
			t.Fatalf("%s polled a group message whose signature no longer verifies", id)
		}
	}
	select {
	case event := <-bells["agent-a"]:
		t.Fatalf("sender was rung for its own group message: %+v", event)
	default:
	}
	s.requirePolled(t, "agent-a")
	if got := s.notifier.count("message.accepted"); got != 1 {
		t.Fatalf("group send produced %d message.accepted events, want 1", got)
	}
	s.notifier.mu.Lock()
	for _, event := range s.notifier.events {
		if event.Event == "message.accepted" && (event.SpaceID != "release-notes" || event.RecipientID != "") {
			t.Errorf("message.accepted does not name the space: %+v", event)
		}
	}
	s.notifier.mu.Unlock()

	// The audience is fixed at send time: later grant edits change neither
	// the recorded audience nor who holds a delivery.
	s.setRegistry(t, func(r *Registry) { r.Agents[s.agentIndex("agent-revoked")].Disabled = false })
	s.requirePolled(t, "agent-revoked")
	requireList(t, s.sendAudit(t, e.ID), "recipients", "agent-b", "agent-d")

	// An identical retry returns the stored message and adds no deliveries.
	retry, status, apiErr := s.send(t, e, "agent-a")
	if apiErr != nil || status != http.StatusOK || retry.Sequence != sent.Sequence {
		t.Fatalf("group retry was not idempotent: HTTP %d %+v %+v", status, apiErr, retry)
	}
	s.requirePolled(t, "agent-b", e.ID)
	s.requirePolled(t, "agent-revoked")
}

func TestGroupResultRepliesToSpace(t *testing.T) {
	s := newGroupSystem(t)
	instruction := s.spaceEnvelope(t, "agent-a", "release-notes", "instruction", `{"ask":"post your section"}`)
	s.mustSend(t, instruction, "agent-a")

	reply := s.spaceEnvelope(t, "agent-b", "release-notes", "result", `{"section":"done"}`)
	reply.ReplyTo, reply.TaskID, reply.ThreadID = instruction.ID, instruction.TaskID, instruction.ThreadID
	if err := SignEnvelope(&reply, s.agents["agent-b"].private); err != nil {
		t.Fatal(err)
	}
	s.mustSend(t, reply, "agent-b")
	requireList(t, s.sendAudit(t, reply.ID), "recipients", "agent-a", "agent-d")
	s.requirePolled(t, "agent-a", reply.ID)
	s.requirePolled(t, "agent-d", instruction.ID, reply.ID)

	// A group message is answered in its space, not privately.
	private := s.envelope(t, "agent-b", "agent-a", "result", `{"section":"done"}`)
	private.ReplyTo, private.TaskID, private.ThreadID = instruction.ID, instruction.TaskID, instruction.ThreadID
	if err := SignEnvelope(&private, s.agents["agent-b"].private); err != nil {
		t.Fatal(err)
	}
	if _, status, apiErr := s.send(t, private, "agent-b"); apiErr == nil || apiErr.Error.Code != "invalid_reply_to" {
		t.Fatalf("direct reply to a group message was accepted: HTTP %d %+v", status, apiErr)
	}
	// The sender holds no delivery of its own message, so it cannot reply to it.
	own := s.spaceEnvelope(t, "agent-a", "release-notes", "result", `{}`)
	own.ReplyTo, own.TaskID, own.ThreadID = instruction.ID, instruction.TaskID, instruction.ThreadID
	if err := SignEnvelope(&own, s.agents["agent-a"].private); err != nil {
		t.Fatal(err)
	}
	if _, status, apiErr := s.send(t, own, "agent-a"); apiErr == nil || apiErr.Error.Code != "invalid_reply_to" {
		t.Fatalf("reply to an undelivered message was accepted: HTTP %d %+v", status, apiErr)
	}
}

func TestGroupAckIsPerMember(t *testing.T) {
	s := newGroupSystem(t)
	e := s.spaceEnvelope(t, "agent-a", "release-notes", "instruction", `{"ask":"review"}`)
	s.mustSend(t, e, "agent-a")

	if status, code := s.ack(t, "agent-b", e.ID, true); status != http.StatusOK {
		t.Fatalf("member ack failed: HTTP %d %s", status, code)
	}
	s.requirePolled(t, "agent-b")
	s.requirePolled(t, "agent-d", e.ID)
	if status, code := s.ack(t, "agent-a", e.ID, true); status != http.StatusNotFound || code != "message_not_found" {
		t.Fatalf("sender acked a group message it holds no delivery for: HTTP %d %s", status, code)
	}
	if status, code := s.ack(t, "agent-c", e.ID, true); status != http.StatusNotFound || code != "message_not_found" {
		t.Fatalf("another tenant acked a group message: HTTP %d %s", status, code)
	}
	data, status, err := s.clients["agent-b"].Do(context.Background(), http.MethodPost, "/v1/messages/"+e.ID+"/ack", []byte(`{"processed":true}`))
	if err != nil || status != http.StatusOK || !strings.Contains(string(data), `"duplicate":true`) {
		t.Fatalf("repeat member ack was not a duplicate: HTTP %d %s %v", status, data, err)
	}
	if status, code := s.ack(t, "agent-d", e.ID, true); status != http.StatusOK {
		t.Fatalf("second member ack failed: HTTP %d %s", status, code)
	}
	s.requirePolled(t, "agent-d")

	accepted := []string{}
	for _, entry := range s.auditActions(t, "ack") {
		if entry.SubjectID == e.ID && entry.Outcome == "accepted" {
			accepted = append(accepted, entry.ActorID)
		}
	}
	if !slices.Equal(accepted, []string{"agent-b", "agent-d"}) {
		t.Fatalf("ack accepted rows for %s by %v, want one per member", e.ID, accepted)
	}
}

func TestLateJoinerDoesNotReceiveEarlierMessage(t *testing.T) {
	s := newGroupSystem(t)
	s.setMembers(t,
		RegistrySpaceMember{Agent: "agent-a", Grants: []string{"read", "post"}},
		RegistrySpaceMember{Agent: "agent-b", Grants: []string{"read", "post"}},
	)
	earlier := s.spaceEnvelope(t, "agent-a", "release-notes", "instruction", `{"n":1}`)
	s.mustSend(t, earlier, "agent-a")
	s.setMembers(t,
		RegistrySpaceMember{Agent: "agent-a", Grants: []string{"read", "post"}},
		RegistrySpaceMember{Agent: "agent-b", Grants: []string{"read", "post"}},
		RegistrySpaceMember{Agent: "agent-d", Grants: []string{"read"}},
	)
	later := s.spaceEnvelope(t, "agent-a", "release-notes", "instruction", `{"n":2}`)
	s.mustSend(t, later, "agent-a")

	s.requirePolled(t, "agent-d", later.ID)
	s.requirePolled(t, "agent-b", earlier.ID, later.ID)
	if status, code := s.ack(t, "agent-d", earlier.ID, true); status != http.StatusNotFound || code != "message_not_found" {
		t.Fatalf("late joiner acked a message sent before it joined: HTTP %d %s", status, code)
	}
	requireList(t, s.sendAudit(t, earlier.ID), "recipients", "agent-b")
}

func TestRemovedMemberStopsSeeingDelivery(t *testing.T) {
	s := newGroupSystem(t)
	base := time.Now().UTC()
	s.service.now = func() time.Time { return base }
	s.service.config.RetryInterval = time.Second
	e := s.spaceEnvelope(t, "agent-a", "release-notes", "instruction", `{"ask":"review"}`)
	s.mustSend(t, e, "agent-a")
	direct := s.mustSend(t, s.envelope(t, "agent-a", "agent-b", "instruction", `{"direct":true}`), "agent-a")
	s.requirePolled(t, "agent-d", e.ID)
	s.requirePolled(t, "agent-b", e.ID, direct.ID)

	// agent-d is removed from the space; agent-b stays but loses read.
	s.setMembers(t,
		RegistrySpaceMember{Agent: "agent-a", Grants: []string{"read", "post"}},
		RegistrySpaceMember{Agent: "agent-b", Grants: []string{"post"}},
	)
	s.requirePolled(t, "agent-d")
	s.requirePolled(t, "agent-b", direct.ID)
	if status, code := s.ack(t, "agent-d", e.ID, true); status != http.StatusNotFound || code != "message_not_found" {
		t.Fatalf("removed member acked a space message: HTTP %d %s", status, code)
	}
	rejected := s.auditActions(t, "ack")
	if last := rejected[len(rejected)-1]; last.ActorID != "agent-d" || last.Outcome != "rejected" || last.Code != "not_member" {
		t.Fatalf("removed member's ack was not audited as not_member: %+v", last)
	}

	// Their pending deliveries stop ringing and are dispositioned once each.
	base = base.Add(2 * time.Second)
	s.service.dispatchDue(context.Background())
	s.service.dispatchDue(context.Background())
	revoked := map[string]string{}
	for _, entry := range s.auditActions(t, "delivery.recipient_revoked") {
		recipient, _ := entry.Detail["recipient_id"].(string)
		if entry.SubjectID == e.ID {
			revoked[recipient] = entry.Code
		}
	}
	if len(revoked) != 2 || revoked["agent-b"] != "not_member" || revoked["agent-d"] != "not_member" {
		t.Fatalf("removed members' deliveries were not dispositioned once each: %v", revoked)
	}

	// Nothing is deleted: restoring read makes the deliveries visible again.
	s.setMembers(t,
		RegistrySpaceMember{Agent: "agent-a", Grants: []string{"read", "post"}},
		RegistrySpaceMember{Agent: "agent-b", Grants: []string{"read", "post"}},
		RegistrySpaceMember{Agent: "agent-d", Grants: []string{"read"}},
	)
	s.requirePolled(t, "agent-d", e.ID)
	if status, code := s.ack(t, "agent-d", e.ID, true); status != http.StatusOK {
		t.Fatalf("restored member could not ack: HTTP %d %s", status, code)
	}
}

func TestGroupEscalationIsPerDelivery(t *testing.T) {
	s := newGroupSystem(t)
	ctx := context.Background()
	base := time.Now().UTC()
	s.service.now = func() time.Time { return base }
	s.service.config.RetryInterval = time.Second
	s.service.config.MaxDoorbellAttempts = 2
	bellB, removeB := s.service.hub.subscribe("agent-b")
	defer removeB()
	bellD, removeD := s.service.hub.subscribe("agent-d")
	defer removeD()
	e := s.spaceEnvelope(t, "agent-a", "release-notes", "instruction", `{"ask":"review"}`)
	s.mustSend(t, e, "agent-a")
	<-bellB
	<-bellD
	if status, code := s.ack(t, "agent-b", e.ID, true); status != http.StatusOK {
		t.Fatalf("ack failed: HTTP %d %s", status, code)
	}

	base = base.Add(2 * time.Second)
	s.service.dispatchDue(ctx)
	select {
	case event := <-bellD:
		if event.MessageID != e.ID || event.SpaceID != "release-notes" {
			t.Fatalf("retry rang the wrong delivery: %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("pending member was not rung again")
	}
	select {
	case event := <-bellB:
		t.Fatalf("member that acknowledged was rung again: %+v", event)
	default:
	}
	for range 3 {
		base = base.Add(2 * time.Second)
		s.service.dispatchDue(ctx)
	}
	if got := s.notifier.count("message.unacknowledged_escalation"); got != 1 {
		t.Fatalf("group message escalated %d times, want once for the pending member", got)
	}
	s.notifier.mu.Lock()
	for _, event := range s.notifier.events {
		if event.Event == "message.unacknowledged_escalation" && (event.RecipientID != "agent-d" || event.SpaceID != "release-notes" || event.MessageID != e.ID) {
			t.Errorf("escalation does not name the pending delivery: %+v", event)
		}
	}
	s.notifier.mu.Unlock()
	rows := s.auditActions(t, "message.unacknowledged_escalation")
	if len(rows) != 1 || rows[0].SubjectID != e.ID || rows[0].Detail["recipient_id"] != "agent-d" {
		t.Fatalf("escalation was not audited once for agent-d: %+v", rows)
	}
	s.requirePolled(t, "agent-d", e.ID)
}

func TestSpaceAndRecipientAreMutuallyExclusive(t *testing.T) {
	s := newGroupSystem(t)
	s.setRegistry(t, func(r *Registry) {
		r.Spaces = append(r.Spaces, RegistrySpace{ID: "other-tenant", TenantID: "tenant-two", Members: []RegistrySpaceMember{{Agent: "agent-c", Grants: []string{"read", "post"}}}})
	})
	both := s.spaceEnvelope(t, "agent-a", "release-notes", "instruction", `{}`)
	both.RecipientID = "agent-b"
	if err := SignEnvelope(&both, s.agents["agent-a"].private); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, sender, code, auditCode string
		envelope                      Envelope
		status                        int
	}{
		{"both", "agent-a", "space_recipient_conflict", "space_recipient_conflict", both, http.StatusBadRequest},
		{"neither", "agent-a", "recipient_required", "recipient_required", s.envelope(t, "agent-a", "", "instruction", `{}`), http.StatusBadRequest},
		{"invalid space id", "agent-a", "invalid_space_id", "invalid_space_id", s.spaceEnvelope(t, "agent-a", "release notes", "instruction", `{}`), http.StatusBadRequest},
		{"unknown space", "agent-a", "space_not_found", "not_member", s.spaceEnvelope(t, "agent-a", "no-such-space", "instruction", `{}`), http.StatusNotFound},
		{"other tenant's space", "agent-a", "space_not_found", "not_member", s.spaceEnvelope(t, "agent-a", "other-tenant", "instruction", `{}`), http.StatusNotFound},
		{"not a member", "agent-c", "space_not_found", "not_member", s.spaceEnvelope(t, "agent-c", "release-notes", "instruction", `{}`), http.StatusNotFound},
		{"read only", "agent-d", "post_not_allowed", "post_not_granted", s.spaceEnvelope(t, "agent-d", "release-notes", "instruction", `{}`), http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, status, apiErr := s.send(t, tc.envelope, tc.sender)
			if apiErr == nil || status != tc.status || apiErr.Error.Code != tc.code {
				t.Fatalf("got HTTP %d %+v, want %d %s", status, apiErr, tc.status, tc.code)
			}
			if _, err := s.store.GetMessage(context.Background(), tc.envelope.ID, "tenant-one"); err == nil {
				t.Fatal("rejected message was stored")
			}
			rows := s.auditActions(t, "send")
			if last := rows[len(rows)-1]; last.Outcome != "rejected" || last.Code != tc.auditCode || last.ActorID != tc.sender {
				t.Fatalf("rejection audited as %+v, want code %s", last, tc.auditCode)
			}
		})
	}
	// Unknown and invisible spaces get byte-identical answers.
	unknown, _, _ := s.clients["agent-c"].Do(context.Background(), http.MethodPost, "/v1/messages", mustJSON(t, s.spaceEnvelope(t, "agent-c", "no-such-space", "instruction", `{}`)))
	hidden, _, _ := s.clients["agent-c"].Do(context.Background(), http.MethodPost, "/v1/messages", mustJSON(t, s.spaceEnvelope(t, "agent-c", "release-notes", "instruction", `{}`)))
	if string(unknown) != string(hidden) {
		t.Fatalf("space existence leaks to non-members:\n%s\n%s", unknown, hidden)
	}
}

func TestSpaceEnvelopeCanonicalPlacement(t *testing.T) {
	e := Envelope{Type: EnvelopeType, ID: "id", TaskID: "task", ThreadID: "thread", SenderID: "agent-a", SpaceID: "release-notes", KeyID: "primary", Kind: "instruction", CreatedAt: "2026-01-01T00:00:00Z", ContentIsData: true}
	canonical, err := EnvelopeSigningBytes(e)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canonical), `"sender_id":"agent-a","recipient_id":"","space_id":"release-notes","key_id":"primary"`) {
		t.Fatalf("space_id is not signed right after recipient_id: %s", canonical)
	}
	e.SpaceID, e.RecipientID = "", "agent-b"
	direct, err := EnvelopeSigningBytes(e)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(direct), "space_id") {
		t.Fatalf("a direct message's canonical bytes mention space_id: %s", direct)
	}
}

// preSliceFourSQLiteSchema is the messages and notification state schema
// before deliveries existed: one pending message with two doorbells, one
// acknowledged, and one dispositioned.
const preSliceFourSQLiteSchema = `
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
	escalated_at TEXT,
	disposition TEXT
);
INSERT INTO messages(id, sender_id, recipient_id, tenant_id, kind, task_id, thread_id, reply_to, envelope_json, accepted_at, acknowledged_at) VALUES
	('pending', 'agent-a', 'agent-b', 'tenant-one', 'instruction', 'task', 'thread', '', '{"id":"pending"}', '2026-01-01T00:00:00Z', NULL),
	('acked', 'agent-a', 'agent-b', 'tenant-one', 'instruction', 'task', 'thread', '', '{"id":"acked"}', '2026-01-01T00:00:01Z', '2026-01-01T00:05:00Z'),
	('dispositioned', 'agent-a', 'agent-b', 'tenant-one', 'instruction', 'task', 'thread', '', '{"id":"dispositioned"}', '2026-01-01T00:00:02Z', NULL);
INSERT INTO notification_state(message_id, attempts, last_notified_at, disposition) VALUES
	('pending', 2, '2026-01-01T00:00:00Z', NULL),
	('acked', 1, '2026-01-01T00:00:01Z', NULL),
	('dispositioned', 1, '2026-01-01T00:00:02Z', 'recipient_revoked');
`

func TestDeliveryBackfillIsIdempotent(t *testing.T) {
	ctx := context.Background()
	check := func(t *testing.T, open func() (Store, error), legacyAck func(*testing.T, string)) {
		t.Helper()
		reopen := func() Store {
			t.Helper()
			store, err := open()
			if err != nil {
				t.Fatalf("open pre-slice database: %v", err)
			}
			return store
		}
		polled := func(store Store) []string {
			t.Helper()
			messages, err := store.ListMessages(ctx, "agent-b", "tenant-one", nil, 10)
			if err != nil {
				t.Fatal(err)
			}
			return polledIDs(PollResponse{Messages: messages})
		}
		for range 2 { // The backfill runs at every startup without duplicating rows.
			store := reopen()
			if got := polled(store); !slices.Equal(got, []string{"pending", "dispositioned"}) {
				t.Fatalf("backfilled deliveries polled as %v", got)
			}
			due, err := store.DueNotifications(ctx, time.Now(), time.Second, 3)
			if err != nil || len(due) != 1 || due[0].MessageID != "pending" || due[0].Recipient != "agent-b" || due[0].Attempts != 2 || due[0].SpaceID != "" {
				t.Fatalf("backfill lost doorbell state: %+v err=%v", due, err)
			}
			store.Close()
		}

		store := reopen()
		if duplicate, err := store.Acknowledge(ctx, "pending", "agent-b", "tenant-one", nil); err != nil || duplicate {
			t.Fatalf("backfilled delivery could not be acknowledged: duplicate=%t err=%v", duplicate, err)
		}
		store.Close()
		// An older release run after a rollback records acks on the message
		// row only; the next startup carries them onto the delivery.
		legacyAck(t, "dispositioned")
		store = reopen()
		defer store.Close()
		if got := polled(store); len(got) != 0 {
			t.Fatalf("acknowledged deliveries came back after a restart: %v", got)
		}
		if duplicate, err := store.Acknowledge(ctx, "acked", "agent-b", "tenant-one", nil); err != nil || !duplicate {
			t.Fatalf("pre-slice acknowledgement was not carried over: duplicate=%t err=%v", duplicate, err)
		}
	}
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pre-slice.db")
		exec := func(t *testing.T, statement string, args ...any) {
			t.Helper()
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(statement, args...); err != nil {
				t.Fatal(err)
			}
		}
		exec(t, preSliceFourSQLiteSchema)
		check(t, func() (Store, error) { return OpenSQLite(path) }, func(t *testing.T, id string) {
			exec(t, `UPDATE messages SET acknowledged_at = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339Nano), id)
		})
	})
	t.Run("postgres", func(t *testing.T) {
		databaseURL := os.Getenv("INBOX_TEST_POSTGRES_URL")
		if databaseURL == "" {
			t.Skip("set INBOX_TEST_POSTGRES_URL to run the PostgreSQL migration test")
		}
		isolated := isolatedPostgresURL(t, databaseURL)
		exec := func(t *testing.T, statement string, args ...any) {
			t.Helper()
			db, err := sql.Open("pgx", isolated)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(statement, args...); err != nil {
				t.Fatal(err)
			}
		}
		schema := strings.NewReplacer("INTEGER PRIMARY KEY AUTOINCREMENT", "BIGSERIAL PRIMARY KEY", "accepted_at TEXT", "accepted_at TIMESTAMPTZ", "acknowledged_at TEXT", "acknowledged_at TIMESTAMPTZ", "last_notified_at TEXT", "last_notified_at TIMESTAMPTZ", "escalated_at TEXT", "escalated_at TIMESTAMPTZ").Replace(preSliceFourSQLiteSchema)
		exec(t, schema)
		check(t, func() (Store, error) { return OpenPostgres(isolated) }, func(t *testing.T, id string) {
			exec(t, `UPDATE messages SET acknowledged_at = $1 WHERE id = $2`, time.Now().UTC(), id)
		})
	})
}
