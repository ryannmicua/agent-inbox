package inbox

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestRegistryRejectsCaseInsensitiveAliasesAndNullRevocationFlags(t *testing.T) {
	public, _, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(public)
	base := fmt.Sprintf(`{"version":1,"agents":[{"id":"agent-a","tenant_id":"tenant-one","disabled":false,"public_keys":[{"id":"primary","public_key":%q,"disabled":false}],"allowed_recipients":[],"allowed_kinds":["instruction"]}]}`, encoded)
	aliases := []struct {
		name       string
		from       string
		to         string
		occurrence int
	}{
		{"root version", `"version"`, `"Version"`, 1},
		{"root agents", `"agents"`, `"Agents"`, 1},
		{"agent id", `"id":"agent-a"`, `"ID":"agent-a"`, 1},
		{"agent tenant", `"tenant_id"`, `"Tenant_ID"`, 1},
		{"agent disabled", `"disabled"`, `"Disabled"`, 1},
		{"agent public keys", `"public_keys"`, `"Public_Keys"`, 1},
		{"agent allowed recipients", `"allowed_recipients"`, `"Allowed_Recipients"`, 1},
		{"agent allowed kinds", `"allowed_kinds"`, `"Allowed_Kinds"`, 1},
		{"key id", `"id":"primary"`, `"ID":"primary"`, 1},
		{"key public key", `"public_key"`, `"Public_Key"`, 1},
		{"key disabled", `"disabled"`, `"Disabled"`, 2},
	}
	cases := make([]struct{ name, data string }, 0, len(aliases)+4)
	for _, alias := range aliases {
		data := replaceRegistryOccurrence(base, alias.from, alias.to, alias.occurrence)
		if data == base {
			t.Fatalf("fixture did not contain %s member", alias.name)
		}
		cases = append(cases, struct{ name, data string }{name: alias.name, data: data})
		if alias.name == "agent disabled" || alias.name == "key disabled" {
			from := `"disabled":false`
			to := `"disabled":true,"Disabled":false`
			data = replaceRegistryOccurrence(base, from, to, alias.occurrence)
			cases = append(cases, struct{ name, data string }{name: alias.name + " conflicting alias", data: data})
		}
	}
	for _, target := range []struct {
		name  string
		index int
	}{{"agent disabled null", 1}, {"key disabled null", 2}} {
		cases = append(cases, struct{ name, data string }{
			name: target.name,
			data: replaceRegistryOccurrence(base, `"disabled":false`, `"disabled":null`, target.index),
		})
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "registry.json")
			if err := os.WriteFile(path, []byte(test.data), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := (FileRegistry{Path: path}).Validate(); err == nil {
				t.Fatal("registry accepted noncanonical or null revocation fields")
			}
			if _, _, err := (FileRegistry{Path: path}).Key("agent-a", "primary"); err == nil {
				t.Fatal("registry authenticated a credential from malformed configuration")
			}
		})
	}
}

func replaceRegistryOccurrence(value, old, replacement string, occurrence int) string {
	start := 0
	for i := 1; i <= occurrence; i++ {
		relative := strings.Index(value[start:], old)
		if relative < 0 {
			return value
		}
		index := start + relative
		if i == occurrence {
			return value[:index] + replacement + value[index+len(old):]
		}
		start = index + len(old)
	}
	return value
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
