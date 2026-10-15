package inbox

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"crypto/ed25519"
)

type Registry struct {
	Version int             `json:"version"`
	Agents  []RegistryAgent `json:"agents"`
	// Spaces requires version 2. Space IDs are a namespace of their own and
	// are never addressed through recipient_id.
	Spaces []RegistrySpace `json:"spaces,omitempty"`
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

// RegistrySpace is an operator-granted group of agents in one tenant.
type RegistrySpace struct {
	ID       string                `json:"id"`
	TenantID string                `json:"tenant_id"`
	Members  []RegistrySpaceMember `json:"members"`
}

type RegistrySpaceMember struct {
	Agent  string   `json:"agent"`
	Grants []string `json:"grants"`
}

// Space grants form a closed set; later routes check exactly one of them.
const (
	GrantRead        = "read"
	GrantPost        = "post"
	GrantNoteWrite   = "note.write"
	GrantArtifactPut = "artifact.put"
	GrantArtifactGet = "artifact.get"
)

var validGrants = map[string]bool{GrantRead: true, GrantPost: true, GrantNoteWrite: true, GrantArtifactPut: true, GrantArtifactGet: true}

// SpaceGrant is one agent's membership in one space, as the registry holds it
// at lookup time. Members lists the IDs of every active member.
type SpaceGrant struct {
	SpaceID  string
	TenantID string
	Grants   []string
	Members  []string
}

// Has reports whether the membership carries grant.
func (g SpaceGrant) Has(grant string) bool { return slices.Contains(g.Grants, grant) }

type RegistrySource interface {
	Agent(id string) (RegistryAgent, error)
	Key(agentID, keyID string) (RegistryAgent, ed25519.PublicKey, error)
	// DisabledKey returns a key that is still listed but disabled, directly or
	// through its agent, so revoked-credential use can be told apart from
	// unknown callers. It returns ErrUnknownKey for anything else.
	DisabledKey(agentID, keyID string) (RegistryAgent, ed25519.PublicKey, error)
	// Snapshot returns every listed agent, including disabled ones, every
	// space, and the SHA-256 of the registry file they were parsed from.
	Snapshot() (RegistrySnapshot, error)
	// SpaceGrants returns the active agent's memberships ordered by space ID.
	SpaceGrants(agentID string) ([]SpaceGrant, error)
	// Grant returns the active agent's membership in one space, or
	// ErrSpaceNotFound when the space is unknown or the agent is not a member.
	Grant(spaceID, agentID string) (SpaceGrant, error)
}

type RegistrySnapshot struct {
	SHA256 string
	Agents map[string]RegistryAgent
	Spaces map[string]RegistrySpace
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
	snapshot, err := f.Snapshot()
	return snapshot.Agents, err
}

func (f FileRegistry) Snapshot() (RegistrySnapshot, error) {
	data, err := os.ReadFile(f.Path)
	if err != nil {
		return RegistrySnapshot{}, fmt.Errorf("read registry: %w", err)
	}
	agents, spaces, err := parseRegistry(data)
	if err != nil {
		return RegistrySnapshot{}, err
	}
	sum := sha256.Sum256(data)
	return RegistrySnapshot{SHA256: hex.EncodeToString(sum[:]), Agents: agents, Spaces: spaces}, nil
}

func parseRegistry(data []byte) (map[string]RegistryAgent, map[string]RegistrySpace, error) {
	if err := validateRegistryFieldNames(data); err != nil {
		return nil, nil, fmt.Errorf("parse registry JSON: %w", err)
	}
	var r Registry
	if err := decodeStrictJSON(data, &r); err != nil {
		return nil, nil, fmt.Errorf("parse registry JSON: %w", err)
	}
	if r.Version != 1 && r.Version != 2 {
		return nil, nil, fmt.Errorf("registry version must be 1 or 2")
	}
	if r.Version == 1 && r.Spaces != nil {
		return nil, nil, errors.New("registry spaces require version 2")
	}
	agents, err := parseRegistryAgents(r.Agents)
	if err != nil {
		return nil, nil, err
	}
	spaces, err := parseRegistrySpaces(r.Spaces, agents)
	if err != nil {
		return nil, nil, err
	}
	return agents, spaces, nil
}

func parseRegistrySpaces(spaces []RegistrySpace, agents map[string]RegistryAgent) (map[string]RegistrySpace, error) {
	out := make(map[string]RegistrySpace, len(spaces))
	for _, space := range spaces {
		if !registryIDPattern.MatchString(space.ID) || !registryIDPattern.MatchString(space.TenantID) {
			return nil, errors.New("each registry space needs a valid id and tenant_id")
		}
		if _, exists := out[space.ID]; exists {
			return nil, fmt.Errorf("duplicate registry space %q", space.ID)
		}
		seenMembers := map[string]bool{}
		for _, member := range space.Members {
			agent, known := agents[member.Agent]
			if !known {
				return nil, fmt.Errorf("space %q lists unknown member %q", space.ID, member.Agent)
			}
			if seenMembers[member.Agent] {
				return nil, fmt.Errorf("space %q lists member %q twice", space.ID, member.Agent)
			}
			seenMembers[member.Agent] = true
			if agent.TenantID != space.TenantID {
				return nil, fmt.Errorf("space %q member %q belongs to another tenant", space.ID, member.Agent)
			}
			if len(member.Grants) == 0 {
				return nil, fmt.Errorf("space %q member %q needs at least one grant", space.ID, member.Agent)
			}
			seenGrants := map[string]bool{}
			for _, grant := range member.Grants {
				if !validGrants[grant] || seenGrants[grant] {
					return nil, fmt.Errorf("space %q member %q has an unsupported or duplicate grant %q", space.ID, member.Agent, grant)
				}
				seenGrants[grant] = true
			}
		}
		out[space.ID] = space
	}
	return out, nil
}

func parseRegistryAgents(agents []RegistryAgent) (map[string]RegistryAgent, error) {
	out := make(map[string]RegistryAgent, len(agents))
	seenPublicKeys := map[string]string{}
	for _, a := range agents {
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
			if owner, exists := seenPublicKeys[string(pub)]; exists && owner != a.ID {
				return nil, fmt.Errorf("agents %q and %q reuse the same public key", owner, a.ID)
			}
			seenPublicKeys[string(pub)] = a.ID
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

func validateRegistryFieldNames(data []byte) error {
	if err := validateJSONMembers(data); err != nil {
		return err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return err
	}
	if err := rejectRegistryAliases(root, "version", "agents", "spaces"); err != nil {
		return err
	}
	if err := validateSpaceFieldNames(root["spaces"]); err != nil {
		return err
	}
	var agents []json.RawMessage
	if raw, ok := root["agents"]; ok {
		if err := json.Unmarshal(raw, &agents); err != nil {
			return err
		}
	}
	for _, rawAgent := range agents {
		var agent map[string]json.RawMessage
		if err := json.Unmarshal(rawAgent, &agent); err != nil {
			return err
		}
		if agent == nil {
			continue
		}
		if err := rejectRegistryAliases(agent, "id", "tenant_id", "disabled", "public_keys", "allowed_recipients", "allowed_kinds"); err != nil {
			return err
		}
		if err := rejectNullDisabled(agent); err != nil {
			return err
		}
		var keys []json.RawMessage
		if raw, ok := agent["public_keys"]; ok {
			if err := json.Unmarshal(raw, &keys); err != nil {
				return err
			}
		}
		for _, rawKey := range keys {
			var key map[string]json.RawMessage
			if err := json.Unmarshal(rawKey, &key); err != nil {
				return err
			}
			if key == nil {
				continue
			}
			if err := rejectRegistryAliases(key, "id", "public_key", "disabled"); err != nil {
				return err
			}
			if err := rejectNullDisabled(key); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateSpaceFieldNames(raw json.RawMessage) error {
	if raw == nil {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("registry spaces must be an array")
	}
	var spaces []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &spaces); err != nil {
		return err
	}
	for _, space := range spaces {
		if err := rejectRegistryAliases(space, "id", "tenant_id", "members"); err != nil {
			return err
		}
		var members []map[string]json.RawMessage
		if rawMembers, ok := space["members"]; ok {
			if err := json.Unmarshal(rawMembers, &members); err != nil {
				return err
			}
		}
		for _, member := range members {
			if err := rejectRegistryAliases(member, "agent", "grants"); err != nil {
				return err
			}
		}
	}
	return nil
}

func rejectRegistryAliases(fields map[string]json.RawMessage, canonical ...string) error {
	for name := range fields {
		for _, expected := range canonical {
			if name != expected && strings.EqualFold(name, expected) {
				return fmt.Errorf("registry field %q must use canonical spelling %q", name, expected)
			}
		}
	}
	return nil
}

func rejectNullDisabled(fields map[string]json.RawMessage) error {
	if raw, ok := fields["disabled"]; ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("registry disabled fields must be booleans")
	}
	return nil
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

func (f FileRegistry) DisabledKey(agentID, keyID string) (RegistryAgent, ed25519.PublicKey, error) {
	entries, err := f.load()
	if err != nil {
		return RegistryAgent{}, nil, err
	}
	a, ok := entries[agentID]
	if !ok {
		return RegistryAgent{}, nil, ErrUnknownKey
	}
	for _, k := range a.PublicKeys {
		if k.ID != keyID || (!k.Disabled && !a.Disabled) {
			continue
		}
		pub, _ := base64.StdEncoding.DecodeString(k.PublicKey)
		return a, ed25519.PublicKey(pub), nil
	}
	return RegistryAgent{}, nil, ErrUnknownKey
}

func (f FileRegistry) SpaceGrants(agentID string) ([]SpaceGrant, error) {
	snapshot, err := f.Snapshot()
	if err != nil {
		return nil, err
	}
	agent, ok := snapshot.Agents[agentID]
	if !ok || agent.Disabled {
		return nil, ErrUnknownAgent
	}
	grants := []SpaceGrant{}
	for _, space := range snapshot.Spaces {
		if grant, ok := spaceGrant(snapshot, space, agent); ok {
			grants = append(grants, grant)
		}
	}
	slices.SortFunc(grants, func(a, b SpaceGrant) int { return strings.Compare(a.SpaceID, b.SpaceID) })
	return grants, nil
}

func (f FileRegistry) Grant(spaceID, agentID string) (SpaceGrant, error) {
	snapshot, err := f.Snapshot()
	if err != nil {
		return SpaceGrant{}, err
	}
	agent, ok := snapshot.Agents[agentID]
	if !ok || agent.Disabled {
		return SpaceGrant{}, ErrUnknownAgent
	}
	space, ok := snapshot.Spaces[spaceID]
	if !ok {
		return SpaceGrant{}, ErrSpaceNotFound
	}
	grant, ok := spaceGrant(snapshot, space, agent)
	if !ok {
		return SpaceGrant{}, ErrSpaceNotFound
	}
	return grant, nil
}

// spaceGrant returns agent's membership in space. Validation already ties
// members to the space tenant; the check is repeated so a lookup can never
// cross tenants.
func spaceGrant(snapshot RegistrySnapshot, space RegistrySpace, agent RegistryAgent) (SpaceGrant, bool) {
	if space.TenantID != agent.TenantID {
		return SpaceGrant{}, false
	}
	var grant SpaceGrant
	found := false
	members := []string{}
	for _, member := range space.Members {
		if member.Agent == agent.ID {
			grant = SpaceGrant{SpaceID: space.ID, TenantID: space.TenantID, Grants: slices.Clone(member.Grants)}
			found = true
		}
		if other := snapshot.Agents[member.Agent]; !other.Disabled {
			members = append(members, member.Agent)
		}
	}
	if !found {
		return SpaceGrant{}, false
	}
	slices.Sort(grant.Grants)
	slices.Sort(members)
	grant.Members = members
	return grant, true
}

var (
	ErrUnknownAgent  = errors.New("unknown or revoked agent")
	ErrUnknownKey    = errors.New("unknown or revoked signing key")
	ErrSpaceNotFound = errors.New("space not found")
)
