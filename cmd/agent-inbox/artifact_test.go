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
	"strings"
	"testing"

	"github.com/ryannmicua/agent-inbox/internal/inbox"
)

func TestArtifactPutAndGetVerifiesHash(t *testing.T) {
	dir := t.TempDir()
	store, err := inbox.OpenSQLite(filepath.Join(dir, "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	keys := map[string]string{}
	agents := []inbox.RegistryAgent{}
	for _, id := range []string{"agent-a", "agent-b"} {
		public, private, err := inbox.GenerateKeyPair()
		if err != nil {
			t.Fatal(err)
		}
		keys[id] = filepath.Join(dir, id+".key")
		if err := os.WriteFile(keys[id], []byte(base64.StdEncoding.EncodeToString(private)), 0o600); err != nil {
			t.Fatal(err)
		}
		agents = append(agents, inbox.RegistryAgent{ID: id, TenantID: "tenant-one", PublicKeys: []inbox.RegistryKey{{ID: "primary", PublicKey: base64.StdEncoding.EncodeToString(public)}}, AllowedRecipients: []string{}, AllowedKinds: []string{"instruction"}})
	}
	registry := inbox.Registry{Version: 2, Agents: agents, Spaces: []inbox.RegistrySpace{{ID: "release-notes", TenantID: "tenant-one", Members: []inbox.RegistrySpaceMember{
		{Agent: "agent-a", Grants: []string{"read", "post", "artifact.put"}},
		{Agent: "agent-b", Grants: []string{"read", "artifact.get"}},
	}}}}
	registryPath := filepath.Join(dir, "registry.json")
	registryData, _ := json.Marshal(registry)
	if err := os.WriteFile(registryPath, registryData, 0o600); err != nil {
		t.Fatal(err)
	}
	objects := inbox.NewMemoryObjectStore()
	objectServer := httptest.NewServer(objects)
	defer objectServer.Close()
	objects.BaseURL = objectServer.URL
	config := inbox.DefaultServerConfig()
	config.Objects = objects
	service, err := inbox.NewServer(store, inbox.FileRegistry{Path: registryPath}, notifierFunc(func(context.Context, inbox.Notification) error { return nil }), config)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(service)
	defer server.Close()
	as := func(id string, args ...string) []string {
		return append(append([]string{"artifact"}, args...), "--server", server.URL, "--agent", id, "--key", keys[id])
	}

	draft := filepath.Join(dir, "draft.md")
	content := []byte("draft release notes\n")
	if err := os.WriteFile(draft, content, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := run(as("agent-a", "put", "--space", "release-notes", "--file", draft), &stdout, io.Discard); err != nil {
		t.Fatalf("artifact put failed: %v", err)
	}
	var put artifactPutOutput
	if err := json.Unmarshal(stdout.Bytes(), &put); err != nil || put.Artifact.State != inbox.ArtifactAvailable || put.Artifact.MediaType != "text/markdown" {
		t.Fatalf("artifact put printed %q (%v)", stdout.String(), err)
	}
	sha := put.Artifact.SHA256
	if want := "inbox-artifact://release-notes/" + sha + "|text/markdown|" + sha + "|20"; put.ArtifactRef != want {
		t.Fatalf("artifact_ref = %q, want %q", put.ArtifactRef, want)
	}
	// The printed reference is ready for send --artifact.
	if err := run([]string{"send", "--server", server.URL, "--agent", "agent-a", "--key", keys["agent-a"], "--space", "release-notes", "--kind", "instruction", "--artifact", put.ArtifactRef}, io.Discard, io.Discard); err != nil {
		t.Fatalf("send with the artifact reference failed: %v", err)
	}
	stdout.Reset()
	if err := run(as("agent-a", "put", "--space", "release-notes", "--file", draft), &stdout, io.Discard); err != nil || !strings.Contains(stdout.String(), `"state":"available"`) {
		t.Fatalf("putting a stored file again = %q (%v)", stdout.String(), err)
	}

	out := filepath.Join(dir, "downloaded.md")
	stdout.Reset()
	if err := run(as("agent-b", "get", "--uri", put.Artifact.URI, "--out", out), &stdout, io.Discard); err != nil {
		t.Fatalf("artifact get failed: %v", err)
	}
	if got, err := os.ReadFile(out); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("downloaded %q (%v)", got, err)
	}
	if err := run(as("agent-b", "get", "--space", "release-notes", "--sha256", sha, "--out", out), io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("artifact get overwrote an existing file: %v", err)
	}
	if err := run(as("agent-a", "get", "--uri", put.Artifact.URI, "--out", filepath.Join(dir, "a.md")), io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "artifact_get_not_allowed") {
		t.Fatalf("artifact get without artifact.get = %v", err)
	}

	// Bytes changed in the store after verification never reach the file.
	objects.Put(inbox.ArtifactObjectKey("tenant-one", "release-notes", sha), bytes.Repeat([]byte("x"), len(content)))
	tampered := filepath.Join(dir, "tampered.md")
	if err := run(as("agent-b", "get", "--uri", put.Artifact.URI, "--out", tampered), io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "do not match") {
		t.Fatalf("artifact get accepted tampered bytes: %v", err)
	}
	if _, err := os.Stat(tampered); !os.IsNotExist(err) {
		t.Fatalf("tampered download left a file: %v", err)
	}
	if entries, _ := filepath.Glob(filepath.Join(dir, ".artifact-*")); len(entries) != 0 {
		t.Fatalf("temporary files left behind: %v", entries)
	}

	for _, args := range [][]string{
		as("agent-b", "remove"),
		as("agent-b", "get", "--uri", "https://files.example.com/x", "--out", out),
		as("agent-b", "get", "--uri", put.Artifact.URI, "--space", "release-notes", "--out", out),
		as("agent-b", "get", "--space", "release-notes", "--sha256", strings.ToUpper(sha), "--out", out),
		as("agent-a", "put", "--space", "../messages", "--file", draft),
		as("agent-a", "put", "--space", "release-notes"),
	} {
		if err := run(args, io.Discard, io.Discard); err == nil {
			t.Fatalf("artifact command %v was accepted", args)
		}
	}
}
