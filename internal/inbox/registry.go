package inbox

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"

	"crypto/ed25519"
)

type Registry struct {
	Version int             `json:"version"`
	Agents  []RegistryAgent `json:"agents"`
}

type RegistryAgent struct {
	ID                string        `json:"id"`
	TenantID          string        `json:"tenant_id"`
	Disabled          bool          `json:"disabled,omitempty"`
	PublicKeys        []RegistryKey `json:"public_keys"`
	AllowedRecipients []string      `json:"allowed_recipients"`
	AllowedKinds      []string      `json:"allowed_kinds"`
}

type RegistryKey struct {
	ID        string `json:"id"`
	PublicKey string `json:"public_key"`
	Disabled  bool   `json:"disabled,omitempty"`
}

type RegistrySource interface {
	Agent(id string) (RegistryAgent, error)
	Key(agentID, keyID string) (RegistryAgent, ed25519.PublicKey, error)
}

// FileRegistry reads the human-managed file for every lookup. This intentionally
// favors immediate revocation and simple operations over caching a tiny pilot
// registry; malformed edits fail closed.
type FileRegistry struct{ Path string }

var registryIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func (f FileRegistry) Validate() error {
	_, err := f.load()
	return err
}

func (f FileRegistry) load() (map[string]RegistryAgent, error) {
	data, err := os.ReadFile(f.Path)
	if err != nil {
		return nil, fmt.Errorf("read registry: %w", err)
	}
	var r Registry
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse registry JSON: %w", err)
	}
	if r.Version != 1 {
		return nil, fmt.Errorf("registry version must be 1")
	}
	out := make(map[string]RegistryAgent, len(r.Agents))
	for _, a := range r.Agents {
		if !registryIDPattern.MatchString(a.ID) || !registryIDPattern.MatchString(a.TenantID) {
			return nil, errors.New("each registry agent needs a valid id and tenant_id")
		}
		if _, exists := out[a.ID]; exists {
			return nil, fmt.Errorf("duplicate registry agent %q", a.ID)
		}
		seenKeys := map[string]bool{}
		for _, k := range a.PublicKeys {
			if !registryIDPattern.MatchString(k.ID) || seenKeys[k.ID] {
				return nil, fmt.Errorf("agent %q has an invalid or duplicate key id", a.ID)
			}
			seenKeys[k.ID] = true
			pub, err := base64.StdEncoding.DecodeString(k.PublicKey)
			if err != nil || len(pub) != ed25519.PublicKeySize {
				return nil, fmt.Errorf("agent %q key %q must be a base64 Ed25519 public key", a.ID, k.ID)
			}
		}
		for _, kind := range a.AllowedKinds {
			if !KindAllowed(kind) {
				return nil, fmt.Errorf("agent %q has unsupported message kind %q", a.ID, kind)
			}
		}
		for _, recipient := range a.AllowedRecipients {
			if !registryIDPattern.MatchString(recipient) {
				return nil, fmt.Errorf("agent %q has an invalid allowed recipient id", a.ID)
			}
		}
		out[a.ID] = a
	}
	return out, nil
}

func (f FileRegistry) Agent(id string) (RegistryAgent, error) {
	entries, err := f.load()
	if err != nil {
		return RegistryAgent{}, err
	}
	a, ok := entries[id]
	if !ok || a.Disabled {
		return RegistryAgent{}, ErrUnknownAgent
	}
	return a, nil
}

func (f FileRegistry) Key(agentID, keyID string) (RegistryAgent, ed25519.PublicKey, error) {
	a, err := f.Agent(agentID)
	if err != nil {
		return RegistryAgent{}, nil, err
	}
	for _, k := range a.PublicKeys {
		if k.ID != keyID || k.Disabled {
			continue
		}
		pub, _ := base64.StdEncoding.DecodeString(k.PublicKey)
		return a, ed25519.PublicKey(pub), nil
	}
	return RegistryAgent{}, nil, ErrUnknownKey
}

var (
	ErrUnknownAgent = errors.New("unknown or revoked agent")
	ErrUnknownKey   = errors.New("unknown or revoked signing key")
)
