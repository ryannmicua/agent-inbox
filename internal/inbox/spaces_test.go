package inbox

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func (s *testSystem) listSpaces(t *testing.T, clientID, path string) (int, SpacesResponse, []byte) {
	t.Helper()
	data, status, err := s.clients[clientID].Do(context.Background(), http.MethodGet, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	var response SpacesResponse
	if status == http.StatusOK {
		if err := json.Unmarshal(data, &response); err != nil {
			t.Fatalf("decode spaces response %s: %v", data, err)
		}
	}
	return status, response, data
}

func releaseNotesSpace() RegistrySpace {
	return RegistrySpace{ID: "release-notes", TenantID: "tenant-one", Members: []RegistrySpaceMember{
		{Agent: "agent-a", Grants: []string{"read", "post", "note.write", "artifact.put", "artifact.get"}},
		{Agent: "agent-b", Grants: []string{"read", "post"}},
	}}
}

func TestRegistryV2SpaceValidation(t *testing.T) {
	keys := map[string]string{}
	for _, id := range []string{"agent-a", "agent-b", "agent-c"} {
		public, _, err := GenerateKeyPair()
		if err != nil {
			t.Fatal(err)
		}
		keys[id] = base64.StdEncoding.EncodeToString(public)
	}
	agent := func(id, tenant string) string {
		return fmt.Sprintf(`{"id":%q,"tenant_id":%q,"public_keys":[{"id":"primary","public_key":%q}],"allowed_recipients":[],"allowed_kinds":["instruction"]}`, id, tenant, keys[id])
	}
	agents := `"agents":[` + agent("agent-a", "tenant-one") + `,` + agent("agent-b", "tenant-one") + `,` + agent("agent-c", "tenant-two") + `]`
	registry := func(version int, spaces string) string {
		if spaces == "" {
			return fmt.Sprintf(`{"version":%d,%s}`, version, agents)
		}
		return fmt.Sprintf(`{"version":%d,%s,"spaces":%s}`, version, agents, spaces)
	}
	member := func(agent string, grants ...string) string {
		encoded, _ := json.Marshal(grants)
		return fmt.Sprintf(`{"agent":%q,"grants":%s}`, agent, encoded)
	}
	space := func(id, tenant string, members ...string) string {
		return fmt.Sprintf(`{"id":%q,"tenant_id":%q,"members":[%s]}`, id, tenant, strings.Join(members, ","))
	}
	cases := []struct {
		name  string
		data  string
		valid bool
	}{
		{"v1 compatibility", registry(1, ""), true},
		{"v2 without spaces", registry(2, ""), true},
		{"v2 with spaces", registry(2, `[`+space("release-notes", "tenant-one", member("agent-a", "read", "post", "note.write", "artifact.put", "artifact.get"), member("agent-b", "read"))+`]`), true},
		{"v2 empty space", registry(2, `[`+space("empty", "tenant-one")+`]`), true},
		{"space id may equal an agent id", registry(2, `[`+space("agent-a", "tenant-one", member("agent-b", "read"))+`]`), true},
		{"tenant mismatch", registry(2, `[`+space("release-notes", "tenant-one", member("agent-c", "read"))+`]`), false},
		{"unknown member", registry(2, `[`+space("release-notes", "tenant-one", member("agent-z", "read"))+`]`), false},
		{"unknown grant", registry(2, `[`+space("release-notes", "tenant-one", member("agent-a", "admin"))+`]`), false},
		{"duplicate space", registry(2, `[`+space("release-notes", "tenant-one", member("agent-a", "read"))+`,`+space("release-notes", "tenant-one", member("agent-b", "read"))+`]`), false},
		{"duplicate member", registry(2, `[`+space("release-notes", "tenant-one", member("agent-a", "read"), member("agent-a", "post"))+`]`), false},
		{"duplicate grant", registry(2, `[`+space("release-notes", "tenant-one", member("agent-a", "read", "read"))+`]`), false},
		{"member without grants", registry(2, `[`+space("release-notes", "tenant-one", member("agent-a"))+`]`), false},
		{"invalid space id", registry(2, `[`+space("-bad", "tenant-one", member("agent-a", "read"))+`]`), false},
		{"spaces in a v1 registry", registry(1, `[]`), false},
		{"null spaces", registry(2, `null`), false},
		{"unsupported version", registry(3, ""), false},
		{"unknown space field", registry(2, `[{"id":"release-notes","tenant_id":"tenant-one","members":[],"owner":"agent-a"}]`), false},
		{"space field alias", registry(2, `[{"ID":"release-notes","tenant_id":"tenant-one","members":[]}]`), false},
		{"member field alias", registry(2, `[{"id":"release-notes","tenant_id":"tenant-one","members":[{"Agent":"agent-a","grants":["read"]}]}]`), false},
		{"root spaces alias", strings.Replace(registry(2, `[]`), `"spaces"`, `"Spaces"`, 1), false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "registry.json")
			if err := os.WriteFile(path, []byte(test.data), 0o600); err != nil {
				t.Fatal(err)
			}
			registry := FileRegistry{Path: path}
			err := registry.Validate()
			if test.valid && err != nil {
				t.Fatalf("valid registry rejected: %v", err)
			}
			if !test.valid {
				if err == nil {
					t.Fatal("invalid registry accepted")
				}
				// Malformed edits fail closed for authentication and grants alike.
				if _, _, err := registry.Key("agent-a", "primary"); err == nil {
					t.Fatal("registry authenticated an agent from malformed configuration")
				}
				if _, err := registry.Grant("release-notes", "agent-a"); err == nil {
					t.Fatal("registry returned a grant from malformed configuration")
				}
			}
		})
	}
}

func TestRegistryGrantFailsClosed(t *testing.T) {
	s := newTestSystem(t)
	s.setRegistry(t, func(r *Registry) {
		r.Version = 2
		space := releaseNotesSpace()
		space.Members = append(space.Members, RegistrySpaceMember{Agent: "agent-revoked", Grants: []string{"read"}})
		r.Spaces = []RegistrySpace{space}
	})
	registry := FileRegistry{Path: s.registryFile}
	grant, err := registry.Grant("release-notes", "agent-b")
	if err != nil || !grant.Has(GrantRead) || !grant.Has(GrantPost) || grant.Has(GrantNoteWrite) || grant.TenantID != "tenant-one" {
		t.Fatalf("member grant = %+v, %v", grant, err)
	}
	for _, test := range []struct {
		name, space, agent string
		want               error
	}{
		{"unknown space", "elsewhere", "agent-a", ErrSpaceNotFound},
		{"non-member", "release-notes", "agent-c", ErrSpaceNotFound},
		{"disabled member", "release-notes", "agent-revoked", ErrUnknownAgent},
		{"unknown agent", "release-notes", "ghost", ErrUnknownAgent},
	} {
		if _, err := registry.Grant(test.space, test.agent); !errors.Is(err, test.want) {
			t.Fatalf("%s: Grant error = %v, want %v", test.name, err, test.want)
		}
	}
	if _, err := registry.SpaceGrants("agent-revoked"); !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("disabled agent listed spaces: %v", err)
	}
	if err := os.WriteFile(s.registryFile, []byte(`{"version":2,`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Grant("release-notes", "agent-a"); err == nil || errors.Is(err, ErrSpaceNotFound) {
		t.Fatalf("unreadable registry did not fail closed with a registry error: %v", err)
	}
	if _, err := registry.SpaceGrants("agent-a"); err == nil {
		t.Fatal("unreadable registry listed spaces")
	}
}

func TestListSpacesShowsOnlyCallerGrants(t *testing.T) {
	s := newTestSystem(t)
	s.setRegistry(t, func(r *Registry) {
		r.Version = 2
		space := releaseNotesSpace()
		space.Members = append(space.Members, RegistrySpaceMember{Agent: "agent-revoked", Grants: []string{"read"}})
		r.Spaces = []RegistrySpace{
			space,
			{ID: "a-first", TenantID: "tenant-one", Members: []RegistrySpaceMember{{Agent: "agent-b", Grants: []string{"read"}}}},
			{ID: "team-two", TenantID: "tenant-two", Members: []RegistrySpaceMember{{Agent: "agent-c", Grants: []string{"read", "post"}}}},
		}
	})
	status, response, data := s.listSpaces(t, "agent-b", "/v1/spaces")
	if status != http.StatusOK {
		t.Fatalf("list spaces status = %d: %s", status, data)
	}
	want := []Space{
		{ID: "a-first", TenantID: "tenant-one", Grants: []string{"read"}, Members: []string{"agent-b"}},
		// Other members appear by ID only; the disabled member not at all.
		{ID: "release-notes", TenantID: "tenant-one", Grants: []string{"post", "read"}, Members: []string{"agent-a", "agent-b"}},
	}
	if !reflect.DeepEqual(response.Spaces, want) {
		t.Fatalf("agent-b spaces = %+v, want %+v", response.Spaces, want)
	}
	if strings.Contains(string(data), "note.write") || strings.Contains(string(data), "artifact.put") || strings.Contains(string(data), "team-two") {
		t.Fatalf("spaces response leaked another member's grants or another tenant's space: %s", data)
	}
	_, response, _ = s.listSpaces(t, "agent-a", "/v1/spaces")
	if len(response.Spaces) != 1 || response.Spaces[0].ID != "release-notes" || len(response.Spaces[0].Grants) != 5 {
		t.Fatalf("agent-a spaces = %+v", response.Spaces)
	}
	_, response, _ = s.listSpaces(t, "agent-c", "/v1/spaces")
	if len(response.Spaces) != 1 || response.Spaces[0].ID != "team-two" || response.Spaces[0].TenantID != "tenant-two" {
		t.Fatalf("agent-c spaces = %+v", response.Spaces)
	}
	if status, _, data := s.listSpaces(t, "agent-b", "/v1/spaces?tenant=tenant-two"); status != http.StatusBadRequest || !strings.Contains(string(data), "unsupported_query_parameter") {
		t.Fatalf("spaces query parameter status = %d: %s", status, data)
	}
	if status, _, _ := s.listSpaces(t, "agent-revoked", "/v1/spaces"); status != http.StatusUnauthorized {
		t.Fatalf("disabled member listed spaces with status %d", status)
	}
	// A space ID is never a recipient, even when an agent shares nothing else.
	e := s.envelope(t, "agent-a", "release-notes", "instruction", `{"task":"hello space"}`)
	if _, status, apiErr := s.send(t, e, "agent-a"); status != http.StatusForbidden || apiErr == nil || apiErr.Error.Code != "recipient_not_allowed" {
		t.Fatalf("send to a space id = %d %+v, want 403 recipient_not_allowed", status, apiErr)
	}
}

func TestSpaceGrantEditAppliesWithoutRestart(t *testing.T) {
	s := newTestSystem(t)
	s.setRegistry(t, func(r *Registry) {
		r.Version = 2
		r.Spaces = []RegistrySpace{releaseNotesSpace()}
	})
	_, response, _ := s.listSpaces(t, "agent-b", "/v1/spaces")
	if len(response.Spaces) != 1 || !reflect.DeepEqual(response.Spaces[0].Grants, []string{"post", "read"}) {
		t.Fatalf("initial agent-b spaces = %+v", response.Spaces)
	}

	s.setRegistry(t, func(r *Registry) { r.Spaces[0].Members[1].Grants = []string{"read", "artifact.get"} })
	_, response, _ = s.listSpaces(t, "agent-b", "/v1/spaces")
	if len(response.Spaces) != 1 || !reflect.DeepEqual(response.Spaces[0].Grants, []string{"artifact.get", "read"}) {
		t.Fatalf("agent-b spaces after grant edit = %+v", response.Spaces)
	}

	s.setRegistry(t, func(r *Registry) { r.Spaces[0].Members = r.Spaces[0].Members[:1] })
	_, response, _ = s.listSpaces(t, "agent-b", "/v1/spaces")
	if len(response.Spaces) != 0 {
		t.Fatalf("removed member still lists the space: %+v", response.Spaces)
	}
	_, response, _ = s.listSpaces(t, "agent-a", "/v1/spaces")
	if len(response.Spaces) != 1 || !reflect.DeepEqual(response.Spaces[0].Members, []string{"agent-a"}) {
		t.Fatalf("remaining member sees stale membership: %+v", response.Spaces)
	}

	// A malformed edit fails closed rather than serving the last good grants.
	if err := os.WriteFile(s.registryFile, []byte(`{"version":2,"agents":[],"spaces":[{"id":"x"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := s.listSpaces(t, "agent-a", "/v1/spaces"); status != http.StatusUnauthorized {
		t.Fatalf("malformed registry served spaces with status %d", status)
	}
}

func TestNonMemberSpaceListIsEmpty(t *testing.T) {
	s := newTestSystem(t)
	for _, version := range []int{1, 2} {
		s.setRegistry(t, func(r *Registry) {
			r.Version = version
			r.Spaces = nil
			if version == 2 {
				r.Spaces = []RegistrySpace{releaseNotesSpace()}
			}
		})
		status, _, data := s.listSpaces(t, "agent-c", "/v1/spaces")
		if status != http.StatusOK || strings.TrimSpace(string(data)) != `{"spaces":[]}` {
			t.Fatalf("registry v%d: non-member spaces = %d %s, want an empty list", version, status, data)
		}
	}
}

func TestRegistryChangeRecordsMembershipDiffs(t *testing.T) {
	s := newTestSystem(t)
	s.pollStatus(t, "agent-a") // Record the v1 baseline.
	s.setRegistry(t, func(r *Registry) {
		r.Version = 2
		r.Spaces = []RegistrySpace{releaseNotesSpace(), {ID: "old-space", TenantID: "tenant-two", Members: []RegistrySpaceMember{{Agent: "agent-c", Grants: []string{"read"}}}}}
	})
	s.pollStatus(t, "agent-a")
	s.setRegistry(t, func(r *Registry) {
		r.Spaces = []RegistrySpace{{ID: "release-notes", TenantID: "tenant-one", Members: []RegistrySpaceMember{
			{Agent: "agent-a", Grants: []string{"artifact.get", "artifact.put", "note.write", "post", "read"}}, // Reordered only.
			{Agent: "agent-b", Grants: []string{"read"}},
			{Agent: "agent-revoked", Grants: []string{"read"}},
		}}}
	})
	s.pollStatus(t, "agent-a")

	changes := s.auditActions(t, "registry.changed")
	if len(changes) != 3 {
		t.Fatalf("registry.changed rows = %d, want baseline and two edits", len(changes))
	}
	membership := func(entry AuditEntry) string {
		encoded, err := json.Marshal(entry.Detail["memberships_changed"])
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	added := changes[1]
	requireList(t, added, "spaces_added", "old-space", "release-notes")
	requireList(t, added, "spaces_removed")
	requireList(t, added, "added")
	requireList(t, added, "policy_changed")
	if got, want := membership(added), `[{"agent":"agent-c","new_grants":["read"],"old_grants":[],"space":"old-space"},{"agent":"agent-a","new_grants":["artifact.get","artifact.put","note.write","post","read"],"old_grants":[],"space":"release-notes"},{"agent":"agent-b","new_grants":["post","read"],"old_grants":[],"space":"release-notes"}]`; got != want {
		t.Fatalf("memberships_changed for new spaces = %s, want %s", got, want)
	}
	edited := changes[2]
	requireList(t, edited, "spaces_added")
	requireList(t, edited, "spaces_removed", "old-space")
	if got, want := membership(edited), `[{"agent":"agent-c","new_grants":[],"old_grants":["read"],"space":"old-space"},{"agent":"agent-b","new_grants":["read"],"old_grants":["post","read"],"space":"release-notes"},{"agent":"agent-revoked","new_grants":["read"],"old_grants":[],"space":"release-notes"}]`; got != want {
		t.Fatalf("memberships_changed for an edit = %s, want %s", got, want)
	}
}

// TestRegistrySpacesStateMigratesPreSliceDatabase opens a database whose
// registry_state predates spaces, then checks the first edit diffs against the
// recorded agents and lists every space as added.
func TestRegistrySpacesStateMigratesPreSliceDatabase(t *testing.T) {
	ctx := context.Background()
	keys := map[string]string{}
	for _, id := range []string{"agent-a", "agent-b"} {
		public, _, err := GenerateKeyPair()
		if err != nil {
			t.Fatal(err)
		}
		keys[id] = base64.StdEncoding.EncodeToString(public)
	}
	agent := func(id string) RegistryAgent {
		return RegistryAgent{ID: id, TenantID: "tenant-one", PublicKeys: []RegistryKey{{ID: "primary", PublicKey: keys[id]}}, AllowedRecipients: []string{}, AllowedKinds: []string{"instruction"}}
	}
	legacyAgents, err := json.Marshal(map[string]RegistryAgent{"agent-a": agent("agent-a")})
	if err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, open func() (Store, error), downgrade func(*testing.T)) {
		store, err := open()
		if err != nil {
			t.Fatal(err)
		}
		store.Close()
		downgrade(t)
		for range 2 { // The migration is idempotent across restarts.
			if store, err = open(); err != nil {
				t.Fatalf("open pre-slice database: %v", err)
			}
			if state, err := store.RegistryState(ctx); err != nil || state.SHA256 != "legacy" || string(state.AgentsJSON) != string(legacyAgents) || state.SpacesJSON != nil {
				store.Close()
				t.Fatalf("pre-slice registry state after migration = %+v, %v", state, err)
			}
			store.Close()
		}
		if store, err = open(); err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		registryPath := filepath.Join(t.TempDir(), "registry.json")
		writeRegistry(t, registryPath, Registry{Version: 2, Agents: []RegistryAgent{agent("agent-a"), agent("agent-b")}, Spaces: []RegistrySpace{
			{ID: "release-notes", TenantID: "tenant-one", Members: []RegistrySpaceMember{{Agent: "agent-a", Grants: []string{"read"}}}},
		}})
		service, err := NewServer(store, FileRegistry{Path: registryPath}, &recordingNotifier{store: store}, DefaultServerConfig())
		if err != nil {
			t.Fatal(err)
		}
		service.observeRegistry(ctx)
		entries, err := store.AuditEntries(ctx, 10)
		if err != nil || len(entries) != 1 || entries[0].Action != "registry.changed" {
			t.Fatalf("audit after first observation = %+v, %v", entries, err)
		}
		edit := entries[0]
		if edit.Detail["old_sha256"] != "legacy" {
			t.Fatalf("edit did not chain from the legacy state: %+v", edit.Detail)
		}
		requireList(t, edit, "added", "agent-b")
		requireList(t, edit, "removed")
		requireList(t, edit, "spaces_added", "release-notes")
		state, err := store.RegistryState(ctx)
		if err != nil || state.SHA256 == "legacy" || !strings.Contains(string(state.SpacesJSON), "release-notes") {
			t.Fatalf("recorded state after edit = %+v, %v", state, err)
		}
	}
	legacyRow := func(t *testing.T, db *sql.DB, insert string, observed any) {
		t.Helper()
		if _, err := db.Exec(`ALTER TABLE registry_state DROP COLUMN spaces_json`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(insert, string(legacyAgents), observed); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pre-slice.db")
		check(t, func() (Store, error) { return OpenSQLite(path) }, func(t *testing.T) {
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			legacyRow(t, db, `INSERT INTO registry_state(id, sha256, agents_json, observed_at) VALUES(1, 'legacy', ?, ?)`, time.Now().UTC().Format(time.RFC3339Nano))
		})
	})
	t.Run("postgres", func(t *testing.T) {
		databaseURL := os.Getenv("INBOX_TEST_POSTGRES_URL")
		if databaseURL == "" {
			t.Skip("set INBOX_TEST_POSTGRES_URL to run the PostgreSQL migration test")
		}
		isolated := isolatedPostgresURL(t, databaseURL)
		check(t, func() (Store, error) { return OpenPostgres(isolated) }, func(t *testing.T) {
			db, err := sql.Open("pgx", isolated)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			legacyRow(t, db, `INSERT INTO registry_state(id, sha256, agents_json, observed_at) VALUES(1, 'legacy', $1, $2)`, time.Now().UTC())
		})
	})
}
