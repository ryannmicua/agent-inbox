package inbox

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// newNoteSystem is newGroupSystem where agent-a and agent-b also hold
// note.write, and agent-e is a tenant-one agent outside the space.
func newNoteSystem(t *testing.T) *testSystem {
	t.Helper()
	s := newGroupSystem(t)
	s.addAgent(t, "agent-e", "tenant-one")
	s.setMembers(t,
		RegistrySpaceMember{Agent: "agent-a", Grants: []string{"read", "post", "note.write"}},
		RegistrySpaceMember{Agent: "agent-b", Grants: []string{"read", "post", "note.write"}},
		RegistrySpaceMember{Agent: "agent-d", Grants: []string{"read"}},
		RegistrySpaceMember{Agent: "agent-revoked", Grants: []string{"read"}},
	)
	return s
}

func (s *testSystem) noteRevision(t *testing.T, author, note string, base int64, body string) NoteRevision {
	t.Helper()
	id, err := NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	n := NoteRevision{Type: NoteRevisionType, RevisionID: id, SpaceID: "release-notes", NoteID: note, BaseRevision: base, AuthorID: author, KeyID: "primary",
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), ContentIsData: true, MediaType: "text/markdown", SHA256: NoteBodySHA256(body), Body: body}
	if err := SignNoteRevision(&n, s.agents[author].private); err != nil {
		t.Fatal(err)
	}
	return n
}

// putNote sends n as clientID to the note path n names.
func (s *testSystem) putNote(t *testing.T, clientID string, n NoteRevision) (int, []byte) {
	t.Helper()
	data, status, err := s.clients[clientID].Do(context.Background(), http.MethodPut, "/v1/spaces/"+n.SpaceID+"/notes/"+n.NoteID, mustJSON(t, n))
	if err != nil {
		t.Fatal(err)
	}
	return status, data
}

func (s *testSystem) mustPutNote(t *testing.T, clientID string, n NoteRevision, wantStatus int) StoredNoteRevision {
	t.Helper()
	status, data := s.putNote(t, clientID, n)
	if status != wantStatus {
		t.Fatalf("note write by %s returned HTTP %d, want %d: %s", clientID, status, wantStatus, data)
	}
	var stored StoredNoteRevision
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	return stored
}

func (s *testSystem) get(t *testing.T, clientID, path string) (int, []byte) {
	t.Helper()
	data, status, err := s.clients[clientID].Do(context.Background(), http.MethodGet, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return status, data
}

func (s *testSystem) mustGetNote(t *testing.T, clientID, path string) StoredNoteRevision {
	t.Helper()
	status, data := s.get(t, clientID, path)
	if status != http.StatusOK {
		t.Fatalf("GET %s as %s returned HTTP %d: %s", path, clientID, status, data)
	}
	var stored StoredNoteRevision
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	return stored
}

func (s *testSystem) noteLog(t *testing.T, clientID, path string) NoteLogResponse {
	t.Helper()
	status, data := s.get(t, clientID, path)
	if status != http.StatusOK {
		t.Fatalf("GET %s returned HTTP %d: %s", path, status, data)
	}
	var log NoteLogResponse
	if err := json.Unmarshal(data, &log); err != nil {
		t.Fatal(err)
	}
	return log
}

func requireAPIError(t *testing.T, status int, data []byte, wantStatus int, wantCode string) {
	t.Helper()
	var response ErrorResponse
	_ = json.Unmarshal(data, &response)
	if status != wantStatus || response.Error.Code != wantCode {
		t.Fatalf("got HTTP %d %s, want %d %s", status, data, wantStatus, wantCode)
	}
}

func logRevisions(log NoteLogResponse) []int64 {
	out := []int64{}
	for _, entry := range log.Revisions {
		out = append(out, entry.Revision)
	}
	return out
}

const plan = "/v1/spaces/release-notes/notes/release-plan"

func TestNoteCreateReadAndHistory(t *testing.T) {
	s := newNoteSystem(t)
	first := s.noteRevision(t, "agent-a", "release-plan", 0, "# Release plan\n\n- ship <v1> & tag\n")
	created := s.mustPutNote(t, "agent-a", first, http.StatusCreated)
	if created.Revision != 1 || created.BaseRevision != 0 || created.TenantID != "tenant-one" || created.SHA256 != first.SHA256 || created.Body != first.Body {
		t.Fatalf("create returned %+v", created)
	}
	second := s.noteRevision(t, "agent-b", "release-plan", 1, "# Release plan\n\n- ship v1\n- notes drafted\n")
	if got := s.mustPutNote(t, "agent-b", second, http.StatusCreated); got.Revision != 2 || got.BaseRevision != 1 {
		t.Fatalf("second revision returned %+v", got)
	}

	head := s.mustGetNote(t, "agent-d", plan)
	if head.Revision != 2 || head.AuthorID != "agent-b" || head.Body != second.Body || head.RevisionID != second.RevisionID || head.AuthorStatus != "" {
		t.Fatalf("head is %+v", head)
	}
	if !VerifyNoteRevision(head.NoteRevision, s.agents["agent-b"].public) {
		t.Fatal("a reader cannot verify the head's author signature")
	}
	old := s.mustGetNote(t, "agent-d", plan+"?revision=1")
	if old.Revision != 1 || old.Body != first.Body || !VerifyNoteRevision(old.NoteRevision, s.agents["agent-a"].public) {
		t.Fatalf("revision 1 is %+v", old)
	}

	history := s.noteLog(t, "agent-a", plan+"/revisions")
	if got := logRevisions(history); !slices.Equal(got, []int64{2, 1}) || history.Revisions[0].AuthorID != "agent-b" || history.Revisions[0].BaseRevision != 1 || history.Revisions[1].SHA256 != first.SHA256 {
		t.Fatalf("history is %+v", history)
	}
	if got := logRevisions(s.noteLog(t, "agent-a", plan+"/revisions?limit=1")); !slices.Equal(got, []int64{2}) {
		t.Fatalf("limited history is %v", got)
	}

	for path, code := range map[string]string{
		plan + "?revision=3":                      "note_not_found",
		"/v1/spaces/release-notes/notes/missing":  "note_not_found",
		plan + "/revisions?limit=0":               "invalid_limit",
		plan + "?revision=0":                      "invalid_revision",
		plan + "?cursor=1":                        "unsupported_query_parameter",
		"/v1/spaces/release-notes/notes/bad%20id": "invalid_note_id",
	} {
		status, data := s.get(t, "agent-a", path)
		var response ErrorResponse
		_ = json.Unmarshal(data, &response)
		if response.Error.Code != code || (status != http.StatusNotFound && status != http.StatusBadRequest) {
			t.Errorf("GET %s returned HTTP %d %s, want %s", path, status, data, code)
		}
	}

	accepted := []string{}
	for _, row := range s.auditActions(t, "note.revision") {
		if row.Outcome == "accepted" {
			accepted = append(accepted, row.SubjectID)
			if row.Detail["sha256"] == nil || row.Detail["base_revision"] == nil || row.Detail["revision"] == nil {
				t.Fatalf("accepted revision audit lacks its fields: %+v", row.Detail)
			}
		}
	}
	if !slices.Equal(accepted, []string{"release-notes/release-plan@1", "release-notes/release-plan@2"}) {
		t.Fatalf("accepted revisions audited as %v", accepted)
	}
}

func TestNoteConflictReturnsHeadAndStoresNothing(t *testing.T) {
	s := newNoteSystem(t)
	s.mustPutNote(t, "agent-a", s.noteRevision(t, "agent-a", "release-plan", 0, "v1"), http.StatusCreated)
	s.mustPutNote(t, "agent-a", s.noteRevision(t, "agent-a", "release-plan", 1, "v1 a"), http.StatusCreated)
	notices := len(s.poll(t, "agent-d").Messages)

	draft := s.noteRevision(t, "agent-b", "release-plan", 1, "v1 b")
	status, data := s.putNote(t, "agent-b", draft)
	var conflict NoteConflictResponse
	if err := json.Unmarshal(data, &conflict); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusConflict || conflict.Error.Code != "note_revision_conflict" || conflict.HeadRevision != 2 || conflict.HeadSHA256 != NoteBodySHA256("v1 a") || conflict.HeadAuthor != "agent-a" {
		t.Fatalf("stale write returned HTTP %d %s", status, data)
	}
	if !strings.Contains(conflict.Error.Message, "head is revision 2") {
		t.Fatalf("conflict message does not name the head: %q", conflict.Error.Message)
	}
	if head := s.mustGetNote(t, "agent-b", plan); head.Revision != 2 || head.Body != "v1 a" {
		t.Fatalf("conflicting write changed the head: %+v", head)
	}
	if got := logRevisions(s.noteLog(t, "agent-b", plan+"/revisions")); !slices.Equal(got, []int64{2, 1}) {
		t.Fatalf("conflicting write was stored: %v", got)
	}
	if got := len(s.poll(t, "agent-d").Messages); got != notices {
		t.Fatalf("conflicting write sent a notice: %d notices, want %d", got, notices)
	}
	rows := s.auditActions(t, "note.revision")
	last := rows[len(rows)-1]
	if last.Outcome != "rejected" || last.Code != "revision_conflict" || last.ActorID != "agent-b" || last.SubjectID != "release-notes/release-plan" {
		t.Fatalf("conflict audited as %+v", last)
	}
	if last.Detail["base_revision"] != float64(1) || last.Detail["head_revision"] != float64(2) || last.Detail["sha256"] != draft.SHA256 || last.Detail["body"] != nil {
		t.Fatalf("conflict audit detail is %+v", last.Detail)
	}

	// Creating an existing note and writing ahead of the head conflict too.
	for _, base := range []int64{0, 5} {
		status, data := s.putNote(t, "agent-b", s.noteRevision(t, "agent-b", "release-plan", base, "other"))
		if status != http.StatusConflict || !strings.Contains(string(data), `"head_revision":2`) {
			t.Fatalf("base %d returned HTTP %d %s", base, status, data)
		}
	}
	status, data = s.putNote(t, "agent-b", s.noteRevision(t, "agent-b", "unwritten", 3, "x"))
	if status != http.StatusConflict || !strings.Contains(string(data), `"head_revision":0,"head_sha256":"","head_author":""`) {
		t.Fatalf("write to a missing note with base 3 returned HTTP %d %s", status, data)
	}

	merged := s.mustPutNote(t, "agent-b", s.noteRevision(t, "agent-b", "release-plan", 2, "v1 a b"), http.StatusCreated)
	if merged.Revision != 3 || merged.BaseRevision != 2 {
		t.Fatalf("merged write returned %+v", merged)
	}
}

func TestConcurrentNoteWritersExactlyOneWins(t *testing.T) {
	s := newNoteSystem(t)
	s.mustPutNote(t, "agent-a", s.noteRevision(t, "agent-a", "release-plan", 0, "v1"), http.StatusCreated)
	for round, base := range []int64{1, 2, 3} {
		const writers = 6
		statuses := make([]int, writers)
		var wg sync.WaitGroup
		for i := range writers {
			author := []string{"agent-a", "agent-b"}[i%2]
			n := s.noteRevision(t, author, "release-plan", base, strings.Repeat("x", i+1))
			wg.Add(1)
			go func() {
				defer wg.Done()
				statuses[i], _ = s.putNote(t, author, n)
			}()
		}
		wg.Wait()
		won, lost := 0, 0
		for _, status := range statuses {
			switch status {
			case http.StatusCreated:
				won++
			case http.StatusConflict:
				lost++
			}
		}
		if won != 1 || lost != writers-1 {
			t.Fatalf("round %d: statuses %v, want exactly one 201 and the rest 409", round, statuses)
		}
		if head := s.mustGetNote(t, "agent-d", plan); head.Revision != base+1 || head.BaseRevision != base {
			t.Fatalf("round %d: head is %+v", round, head)
		}
	}
	if got := logRevisions(s.noteLog(t, "agent-d", plan+"/revisions")); !slices.Equal(got, []int64{4, 3, 2, 1}) {
		t.Fatalf("history is %v", got)
	}

	// Concurrent creation of a new note also has exactly one winner.
	statuses := make([]int, 4)
	var wg sync.WaitGroup
	for i := range statuses {
		n := s.noteRevision(t, "agent-a", "fresh", 0, strings.Repeat("y", i+1))
		wg.Add(1)
		go func() { defer wg.Done(); statuses[i], _ = s.putNote(t, "agent-a", n) }()
	}
	wg.Wait()
	slices.Sort(statuses)
	if !slices.Equal(statuses, []int{http.StatusCreated, http.StatusConflict, http.StatusConflict, http.StatusConflict}) {
		t.Fatalf("concurrent creates returned %v", statuses)
	}
}

func TestNoteRetrySameRevisionIDIsIdempotent(t *testing.T) {
	s := newNoteSystem(t)
	n := s.noteRevision(t, "agent-a", "release-plan", 0, "v1")
	first := s.mustPutNote(t, "agent-a", n, http.StatusCreated)

	retry := n
	retry.CreatedAt = time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano)
	if err := SignNoteRevision(&retry, s.agents["agent-a"].private); err != nil {
		t.Fatal(err)
	}
	again := s.mustPutNote(t, "agent-a", retry, http.StatusOK)
	if again.Revision != 1 || again.RevisionID != n.RevisionID || again.CreatedAt != n.CreatedAt || !again.AcceptedAt.Equal(first.AcceptedAt) {
		t.Fatalf("retry returned %+v, want the stored revision %+v", again, first)
	}

	// A retry still answers after another writer moved the head.
	s.mustPutNote(t, "agent-b", s.noteRevision(t, "agent-b", "release-plan", 1, "v2"), http.StatusCreated)
	if late := s.mustPutNote(t, "agent-a", n, http.StatusOK); late.Revision != 1 {
		t.Fatalf("late retry returned %+v", late)
	}
	if got := logRevisions(s.noteLog(t, "agent-a", plan+"/revisions")); !slices.Equal(got, []int64{2, 1}) {
		t.Fatalf("retries added revisions: %v", got)
	}
	notices := 0
	for _, message := range s.poll(t, "agent-d").Messages {
		if message.Type == NoticeType {
			notices++
		}
	}
	if notices != 2 {
		t.Fatalf("agent-d holds %d notices, want one per accepted revision", notices)
	}

	changed := n
	changed.Body, changed.SHA256 = "v1 changed", NoteBodySHA256("v1 changed")
	if err := SignNoteRevision(&changed, s.agents["agent-a"].private); err != nil {
		t.Fatal(err)
	}
	status, data := s.putNote(t, "agent-a", changed)
	requireAPIError(t, status, data, http.StatusConflict, "revision_id_conflict")
	elsewhere := n
	elsewhere.NoteID = "other-note"
	if err := SignNoteRevision(&elsewhere, s.agents["agent-a"].private); err != nil {
		t.Fatal(err)
	}
	status, data = s.putNote(t, "agent-a", elsewhere)
	requireAPIError(t, status, data, http.StatusConflict, "revision_id_conflict")

	duplicates := 0
	for _, row := range s.auditActions(t, "note.revision") {
		if row.Outcome == "duplicate" {
			duplicates++
			if row.SubjectID != "release-notes/release-plan@1" || row.Code != "duplicate_revision" {
				t.Fatalf("duplicate audited as %+v", row)
			}
		}
	}
	if duplicates != 2 {
		t.Fatalf("audited %d duplicate writes, want 2", duplicates)
	}
}

func TestNoteHashAndSignatureVerified(t *testing.T) {
	s := newNoteSystem(t)
	resign := func(n NoteRevision, signer string) NoteRevision {
		if err := SignNoteRevision(&n, s.agents[signer].private); err != nil {
			t.Fatal(err)
		}
		return n
	}
	valid := s.noteRevision(t, "agent-a", "release-plan", 0, "v1")
	tampered := valid
	tampered.Body, tampered.SHA256 = "v1 tampered", NoteBodySHA256("v1 tampered")
	wrongHash := valid
	wrongHash.SHA256 = NoteBodySHA256("something else")
	uppercaseHash := valid
	uppercaseHash.SHA256 = strings.ToUpper(valid.SHA256)
	asOther := valid
	asOther.AuthorID = "agent-b"
	pathMismatch := valid
	pathMismatch.NoteID = "other-note"
	notData := valid
	notData.ContentIsData = false
	badType := valid
	badType.Type = "message"
	secret := valid
	secret.Body = "deploy key -----BEGIN OPENSSH PRIVATE KEY----- abc"
	secret.SHA256 = NoteBodySHA256(secret.Body)
	withNUL := valid
	withNUL.Body = "a\x00b"
	withNUL.SHA256 = NoteBodySHA256(withNUL.Body)
	badMedia := valid
	badMedia.MediaType = "markdown"

	cases := []struct {
		name     string
		n        NoteRevision
		status   int
		code     string
		clientID string
	}{
		{"body changed after signing", tampered, http.StatusBadRequest, "invalid_note_signature", "agent-a"},
		{"signed by another agent's key", resign(valid, "agent-b"), http.StatusBadRequest, "invalid_note_signature", "agent-a"},
		{"hash does not match body", resign(wrongHash, "agent-a"), http.StatusBadRequest, "note_hash_mismatch", "agent-a"},
		{"uppercase hash", resign(uppercaseHash, "agent-a"), http.StatusBadRequest, "note_hash_mismatch", "agent-a"},
		{"author is not the caller", resign(asOther, "agent-a"), http.StatusBadRequest, "author_mismatch", "agent-a"},
		{"path names another note", resign(pathMismatch, "agent-a"), http.StatusBadRequest, "note_path_mismatch", "agent-a"},
		{"content not marked as data", resign(notData, "agent-a"), http.StatusBadRequest, "content_marker_required", "agent-a"},
		{"wrong type", resign(badType, "agent-a"), http.StatusBadRequest, "invalid_type", "agent-a"},
		{"secret in body", resign(secret, "agent-a"), http.StatusForbidden, "secret_detected", "agent-a"},
		{"NUL in body", resign(withNUL, "agent-a"), http.StatusBadRequest, "invalid_note_body", "agent-a"},
		{"media type", resign(badMedia, "agent-a"), http.StatusBadRequest, "invalid_media_type", "agent-a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := "/v1/spaces/release-notes/notes/release-plan"
			data, status, err := s.clients[tc.clientID].Do(context.Background(), http.MethodPut, path, mustJSON(t, tc.n))
			if err != nil {
				t.Fatal(err)
			}
			requireAPIError(t, status, data, tc.status, tc.code)
			rows := s.auditActions(t, "note.revision")
			if last := rows[len(rows)-1]; last.Outcome != "rejected" || last.Code != tc.code || last.SubjectID != "release-notes/release-plan" {
				t.Fatalf("rejection audited as %+v", last)
			}
		})
	}
	status, data := s.get(t, "agent-a", plan)
	requireAPIError(t, status, data, http.StatusNotFound, "note_not_found")
	if _, err := s.store.GetNoteRevision(context.Background(), "tenant-one", "release-notes", "other-note", 0); err != ErrNoteNotFound {
		t.Fatalf("a rejected write was stored: %v", err)
	}
	s.mustPutNote(t, "agent-a", valid, http.StatusCreated)
}

func TestNoteNonMemberGetsNotFound(t *testing.T) {
	s := newNoteSystem(t)
	s.mustPutNote(t, "agent-a", s.noteRevision(t, "agent-a", "release-plan", 0, "v1"), http.StatusCreated)
	_, unknownRoute := s.get(t, "agent-e", "/v1/no-such-route")

	// agent-e (same tenant, not a member) and agent-c (another tenant) cannot
	// tell an existing note from a missing space or an unknown route.
	for _, outsider := range []string{"agent-e", "agent-c"} {
		for _, path := range []string{plan, plan + "?revision=1", plan + "/revisions", "/v1/spaces/release-notes/notes/missing", "/v1/spaces/no-such-space/notes/release-plan"} {
			status, data := s.get(t, outsider, path)
			if status != http.StatusNotFound || string(data) != string(unknownRoute) {
				t.Fatalf("%s GET %s returned HTTP %d %s, want the unknown-route answer %s", outsider, path, status, data, unknownRoute)
			}
		}
		n := s.noteRevision(t, outsider, "release-plan", 1, "intruder")
		status, data := s.putNote(t, outsider, n)
		if status != http.StatusNotFound || string(data) != string(unknownRoute) {
			t.Fatalf("%s PUT returned HTTP %d %s", outsider, status, data)
		}
	}
	reads, writes := 0, 0
	for _, row := range s.auditActions(t, "note.read") {
		if row.Outcome == "rejected" && row.Code == "not_member" {
			reads++
		}
	}
	for _, row := range s.auditActions(t, "note.revision") {
		if row.Outcome == "rejected" && row.Code == "not_member" && row.SubjectID == "release-notes/release-plan" {
			writes++
		}
	}
	if reads != 10 || writes != 2 {
		t.Fatalf("audited %d rejected reads and %d rejected writes, want 10 and 2", reads, writes)
	}

	// A member without note.write may read but not write; one without read
	// may not read.
	status, data := s.putNote(t, "agent-d", s.noteRevision(t, "agent-d", "release-plan", 1, "v2"))
	requireAPIError(t, status, data, http.StatusForbidden, "note_write_not_allowed")
	s.mustGetNote(t, "agent-d", plan)
	s.setMembers(t,
		RegistrySpaceMember{Agent: "agent-a", Grants: []string{"read", "post", "note.write"}},
		RegistrySpaceMember{Agent: "agent-b", Grants: []string{"note.write"}},
	)
	status, data = s.get(t, "agent-b", plan)
	requireAPIError(t, status, data, http.StatusForbidden, "read_not_allowed")

	// A removed member loses access at once (default 1); a late joiner reads
	// the whole history (default 2).
	status, data = s.get(t, "agent-d", plan)
	requireAPIError(t, status, data, http.StatusNotFound, "not_found")
	s.setMembers(t,
		RegistrySpaceMember{Agent: "agent-a", Grants: []string{"read", "post", "note.write"}},
		RegistrySpaceMember{Agent: "agent-e", Grants: []string{"read"}},
	)
	if head := s.mustGetNote(t, "agent-e", plan+"?revision=1"); head.Body != "v1" {
		t.Fatalf("late joiner read %+v", head)
	}
}

func TestNoteReadFlagsRevokedAuthor(t *testing.T) {
	s := newNoteSystem(t)
	s.mustPutNote(t, "agent-a", s.noteRevision(t, "agent-a", "release-plan", 0, "v1"), http.StatusCreated)
	s.mustPutNote(t, "agent-b", s.noteRevision(t, "agent-b", "release-plan", 1, "v2"), http.StatusCreated)
	s.setRegistry(t, func(r *Registry) { r.Agents[s.agentIndex("agent-b")].Disabled = true })

	head := s.mustGetNote(t, "agent-a", plan)
	if head.AuthorID != "agent-b" || head.AuthorStatus != AuthorStatusRevoked || head.Body != "v2" {
		t.Fatalf("a revoked author's revision reads as %+v", head)
	}
	history := s.noteLog(t, "agent-a", plan+"/revisions")
	if history.Revisions[0].AuthorStatus != AuthorStatusRevoked || history.Revisions[1].AuthorStatus != "" {
		t.Fatalf("history flags authors as %+v", history.Revisions)
	}
	if first := s.mustGetNote(t, "agent-a", plan+"?revision=1"); first.AuthorStatus != "" {
		t.Fatalf("an active author's revision is flagged: %+v", first)
	}
}

func TestNoteNoticeDeliveredToMembers(t *testing.T) {
	s := newNoteSystem(t)
	bells := map[string]<-chan DoorbellEvent{}
	for _, id := range []string{"agent-a", "agent-b", "agent-d"} {
		ch, remove := s.service.hub.subscribe(id)
		defer remove()
		bells[id] = ch
	}
	n := s.noteRevision(t, "agent-a", "release-plan", 0, "v1")
	s.mustPutNote(t, "agent-a", n, http.StatusCreated)

	audit := s.auditActions(t, "note.revision")[0]
	requireList(t, audit, "recipients", "agent-b", "agent-d")
	noticeID, _ := audit.Detail["notice_id"].(string)
	if noticeID == "" || audit.Detail["registry_sha256"] != registryFileSHA(t, s.registryFile) {
		t.Fatalf("accepted revision audit does not name its notice and registry: %+v", audit.Detail)
	}
	for _, id := range []string{"agent-b", "agent-d"} {
		select {
		case event := <-bells[id]:
			if event.MessageID != noticeID || event.SpaceID != "release-notes" || event.Recipient != id {
				t.Fatalf("%s doorbell is %+v", id, event)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s was not rung for the notice", id)
		}
		polled := s.requirePolled(t, id, noticeID)
		notice := polled.Messages[0]
		var payload NoteNotice
		if err := json.Unmarshal(notice.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if notice.Type != NoticeType || notice.Kind != NoticeKind || notice.SenderID != "" || notice.Signature != "" || notice.SpaceID != "release-notes" || notice.AuthorStatus != "" || !notice.ContentIsData {
			t.Fatalf("%s polled notice %+v", id, notice)
		}
		if payload != (NoteNotice{SpaceID: "release-notes", NoteID: "release-plan", Revision: 1, AuthorID: "agent-a"}) {
			t.Fatalf("notice payload is %+v", payload)
		}
	}
	select {
	case event := <-bells["agent-a"]:
		t.Fatalf("the author was rung for its own revision: %+v", event)
	default:
	}
	s.requirePolled(t, "agent-a")
	if pending, err := s.store.ListMessages(context.Background(), "agent-revoked", "tenant-one", []string{"release-notes"}, 10); err != nil || len(pending) != 0 {
		t.Fatalf("a disabled member got a notice: %+v (%v)", pending, err)
	}

	if status, code := s.ack(t, "agent-b", noticeID, true); status != http.StatusOK {
		t.Fatalf("notice ack failed: HTTP %d %s", status, code)
	}
	s.requirePolled(t, "agent-b")
	s.requirePolled(t, "agent-d", noticeID)

	// A reader that loses read stops seeing the notice, like any group message.
	s.setMembers(t,
		RegistrySpaceMember{Agent: "agent-a", Grants: []string{"read", "post", "note.write"}},
		RegistrySpaceMember{Agent: "agent-b", Grants: []string{"read", "post", "note.write"}},
	)
	s.requirePolled(t, "agent-d")

	// With no other reader, a revision creates no notice at all.
	s.setMembers(t, RegistrySpaceMember{Agent: "agent-a", Grants: []string{"read", "note.write"}})
	s.mustPutNote(t, "agent-a", s.noteRevision(t, "agent-a", "release-plan", 1, "v2"), http.StatusCreated)
	rows := s.auditActions(t, "note.revision")
	if last := rows[len(rows)-1]; last.Detail["notice_id"] != nil || len(detailList(last, "recipients")) != 0 {
		t.Fatalf("a revision without readers recorded a notice: %+v", last.Detail)
	}
}

func TestNoteBodyLimit(t *testing.T) {
	s := newNoteSystem(t)
	full := s.noteRevision(t, "agent-a", "release-plan", 0, strings.Repeat("a", MaxNoteBodyBytes))
	if got := s.mustPutNote(t, "agent-a", full, http.StatusCreated); len(got.Body) != MaxNoteBodyBytes {
		t.Fatalf("a 48 KiB body was stored as %d bytes", len(got.Body))
	}
	status, data := s.putNote(t, "agent-a", s.noteRevision(t, "agent-a", "release-plan", 1, strings.Repeat("a", MaxNoteBodyBytes+1)))
	requireAPIError(t, status, data, http.StatusRequestEntityTooLarge, "note_too_large")
	// The 64 KiB request limit applies after JSON escaping.
	status, data = s.putNote(t, "agent-a", s.noteRevision(t, "agent-a", "release-plan", 1, strings.Repeat("<", 12*1024)))
	requireAPIError(t, status, data, http.StatusRequestEntityTooLarge, "request_too_large")
	if head := s.mustGetNote(t, "agent-a", plan); head.Revision != 1 {
		t.Fatalf("an oversized write was stored: revision %d", head.Revision)
	}
}

func TestNoteRevisionsAreAppendOnly(t *testing.T) {
	s := newNoteSystem(t)
	s.mustPutNote(t, "agent-a", s.noteRevision(t, "agent-a", "release-plan", 0, "v1"), http.StatusCreated)
	var db *sql.DB
	switch store := s.store.(type) {
	case *SQLiteStore:
		db = store.db
	case *PostgresStore:
		db = store.db
	default:
		t.Fatalf("unexpected store %T", s.store)
	}
	if _, err := db.Exec(`UPDATE note_revisions SET body = 'rewritten'`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("a stored revision was updated: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM note_revisions`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("a stored revision was deleted: %v", err)
	}
}

func TestNoteRevisionSigningPlacement(t *testing.T) {
	n := NoteRevision{Type: NoteRevisionType, RevisionID: "id", SpaceID: "space", NoteID: "note", BaseRevision: 3, AuthorID: "agent-a", KeyID: "primary", CreatedAt: "2026-01-01T00:00:00Z", ContentIsData: true, MediaType: "text/plain", SHA256: NoteBodySHA256("a<b"), Body: "a<b", Signature: "ignored"}
	canonical, err := NoteRevisionSigningBytes(n)
	if err != nil {
		t.Fatal(err)
	}
	want := `agent-inbox-note-v1` + "\n" + `{"type":"note_revision","revision_id":"id","space_id":"space","note_id":"note","base_revision":3,"author_id":"agent-a","key_id":"primary","created_at":"2026-01-01T00:00:00Z","content_is_data":true,"media_type":"text/plain","sha256":"` + n.SHA256 + `","body":"a\u003cb"}`
	if string(canonical) != want {
		t.Fatalf("canonical note bytes\n got %s\nwant %s", canonical, want)
	}
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := SignNoteRevision(&n, private); err != nil || !VerifyNoteRevision(n, public) {
		t.Fatalf("note signature does not round-trip: %v", err)
	}
	n.Body = "a>b"
	if VerifyNoteRevision(n, public) {
		t.Fatal("a changed body still verifies")
	}
}
