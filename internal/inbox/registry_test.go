package inbox

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryRejectsUnknownAndDuplicateFields(t *testing.T) {
	public, _, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	fields := fmt.Sprintf(`"id":"agent-a","tenant_id":"tenant-one","public_keys":[{"id":"primary","public_key":%q}],"allowed_recipients":[],"allowed_kinds":["instruction"]`, base64.StdEncoding.EncodeToString(public))
	cases := []struct {
		name string
		json string
	}{
		{name: "unknown revocation field", json: `{"version":1,"agents":[{` + fields + `,"disable":true}]}`},
		{name: "conflicting duplicate revocation field", json: `{"version":1,"agents":[{` + fields + `,"disabled":true,"disabled":false}]}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "registry.json")
			if err := os.WriteFile(path, []byte(test.json), 0o600); err != nil {
				t.Fatal(err)
			}
			registry := FileRegistry{Path: path}
			if _, _, err := registry.Key("agent-a", "primary"); err == nil {
				t.Fatal("registry authenticated an agent with malformed configuration")
			}
		})
	}
}

func TestRegistryRejectsPublicKeyReuseAcrossAgents(t *testing.T) {
	public, _, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(public)
	registry := Registry{Version: 1, Agents: []RegistryAgent{
		{ID: "agent-a", TenantID: "tenant-one", PublicKeys: []RegistryKey{{ID: "primary", PublicKey: encoded}}, AllowedKinds: []string{"instruction"}},
		{ID: "agent-b", TenantID: "tenant-one", PublicKeys: []RegistryKey{{ID: "primary", PublicKey: encoded}}, AllowedKinds: []string{"instruction"}},
	}}
	data, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (FileRegistry{Path: path}).Validate(); err == nil {
		t.Fatal("registry accepted one signing key for two agents")
	}
}
