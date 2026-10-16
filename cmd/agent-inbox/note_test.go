package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ryannmicua/agent-inbox/internal/inbox"
)

// noteServer serves a release-notes space where agent-a and agent-b both
// hold read and note.write, and returns each agent's key file.
func noteServer(t *testing.T) (*httptest.Server, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	store, err := inbox.OpenSQLite(filepath.Join(dir, "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	registry := inbox.Registry{Version: 2}
	keys := map[string]string{}
	members := []inbox.RegistrySpaceMember{}
	for _, id := range []string{"agent-a", "agent-b"} {
		public, private, err := inbox.GenerateKeyPair()
		if err != nil {
			t.Fatal(err)
		}
		keys[id] = filepath.Join(dir, id+".key")
		if err := os.WriteFile(keys[id], []byte(base64.StdEncoding.EncodeToString(private)), 0o600); err != nil {
			t.Fatal(err)
		}
		registry.Agents = append(registry.Agents, inbox.RegistryAgent{ID: id, TenantID: "tenant-one", PublicKeys: []inbox.RegistryKey{{ID: "primary", PublicKey: base64.StdEncoding.EncodeToString(public)}}, AllowedRecipients: []string{}, AllowedKinds: []string{"instruction"}})
		members = append(members, inbox.RegistrySpaceMember{Agent: id, Grants: []string{"read", "note.write"}})
	}
	registry.Spaces = []inbox.RegistrySpace{{ID: "release-notes", TenantID: "tenant-one", Members: members}}
	registryPath := filepath.Join(dir, "registry.json")
	data, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registryPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := inbox.NewServer(store, inbox.FileRegistry{Path: registryPath}, notifierFunc(func(context.Context, inbox.Notification) error { return nil }), inbox.DefaultServerConfig())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(service)
	t.Cleanup(server.Close)
	return server, keys
}

func TestNoteCommandsPutGetAndLog(t *testing.T) {
	server, keys := noteServer(t)
	note := func(agent string, args ...string) (string, error) {
		var stdout bytes.Buffer
		full := append([]string{"note", args[0], "--server", server.URL, "--agent", agent, "--key", keys[agent], "--space", "release-notes", "--note", "release-plan"}, args[1:]...)
		err := run(full, &stdout, io.Discard)
		return stdout.String(), err
	}
	dir := t.TempDir()
	draft := filepath.Join(dir, "plan.md")
	if err := os.WriteFile(draft, []byte("# Plan\n\n- ship <v1>\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := note("agent-a", "put", "--file", draft); err == nil || !strings.Contains(err.Error(), "--base") {
		t.Fatalf("put without --base was not refused: %v", err)
	}
	out, err := note("agent-a", "put", "--base", "0", "--file", draft, "--id", "0192f0c4-7d2e-7a10-8c3b-5d6e7f809c01")
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	var created inbox.StoredNoteRevision
	if err := json.Unmarshal([]byte(out), &created); err != nil || created.Revision != 1 || created.MediaType != "text/markdown" || created.Body != "# Plan\n\n- ship <v1>\n" {
		t.Fatalf("create printed %q (%v)", out, err)
	}
	if out, err := note("agent-a", "put", "--base", "0", "--file", draft, "--id", "0192f0c4-7d2e-7a10-8c3b-5d6e7f809c01"); err != nil || !strings.Contains(out, `"revision":1`) {
		t.Fatalf("retry with the same --id returned %q (%v)", out, err)
	}

	edit := filepath.Join(dir, "edit.txt")
	if err := os.WriteFile(edit, []byte("b's edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := note("agent-b", "put", "--base", "1", "--file", edit); err != nil {
		t.Fatalf("second revision failed: %v", err)
	}
	_, err = note("agent-a", "put", "--base", "1", "--file", draft)
	if err == nil || !strings.Contains(err.Error(), "note_revision_conflict") || !strings.Contains(err.Error(), "head is revision 2") {
		t.Fatalf("stale put did not report the conflict and head: %v", err)
	}

	out, err = note("agent-a", "get")
	var head inbox.StoredNoteRevision
	if err != nil || json.Unmarshal([]byte(out), &head) != nil || head.Revision != 2 || head.AuthorID != "agent-b" || head.MediaType != "text/plain" {
		t.Fatalf("get printed %q (%v)", out, err)
	}
	out, err = note("agent-b", "get", "--revision", "1")
	if err != nil || !strings.Contains(out, `"revision":1`) || !strings.Contains(out, `"author_id":"agent-a"`) {
		t.Fatalf("get --revision 1 printed %q (%v)", out, err)
	}
	out, err = note("agent-b", "log", "--limit", "5")
	var history inbox.NoteLogResponse
	if err != nil || json.Unmarshal([]byte(out), &history) != nil || len(history.Revisions) != 2 {
		t.Fatalf("log printed %q (%v)", out, err)
	}
	revisions := []int64{history.Revisions[0].Revision, history.Revisions[1].Revision}
	if !slices.Equal(revisions, []int64{2, 1}) {
		t.Fatalf("log lists revisions %v", revisions)
	}
	if err := run([]string{"note", "rename"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "get, put, or log") {
		t.Fatal("unknown note subcommand was accepted")
	}
}
