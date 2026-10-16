package inbox

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	HeaderAgent          = "X-Agent-ID"
	HeaderKeyID          = "X-Key-ID"
	HeaderTimestamp      = "X-Request-Timestamp"
	HeaderNonce          = "X-Request-Nonce"
	HeaderSignature      = "X-Request-Signature"
	MinimumRetryInterval = time.Second
	// revokedCredentialAuditInterval caps auth.revoked_credential rows to one
	// per agent and key in this window, so a revoked bot cannot flood the log.
	revokedCredentialAuditInterval = time.Minute
)

var (
	messageIDPattern   = regexp.MustCompile(`^([0-9a-f]{8}-[0-9a-f]{4}-[47][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}|[0-7][0-9A-HJKMNP-TV-Z]{25})$`)
	referenceIDPattern = regexp.MustCompile(`(?i)^([0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}|[0-7][0-9a-hjkmnp-tv-z]{25})$`)
	noncePattern       = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
	shaPattern         = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type ServerConfig struct {
	RequestSkew         time.Duration
	RetryInterval       time.Duration
	MaxDoorbellAttempts int
	MaxPollLimit        int
}

func DefaultServerConfig() ServerConfig {
	return ServerConfig{RequestSkew: 5 * time.Minute, RetryInterval: 30 * time.Second, MaxDoorbellAttempts: 3, MaxPollLimit: 100}
}

type Server struct {
	store    Store
	registry RegistrySource
	notifier Notifier
	config   ServerConfig
	now      func() time.Time
	hub      *doorbellHub
	preAuth  preAuthCounters

	registryMu       sync.Mutex
	registrySHA256   string // last registry hash known to be recorded
	revokedAuditMu   sync.Mutex
	revokedAuditedAt map[string]time.Time
}

type preAuthCounters struct {
	unsigned                          atomic.Uint64
	oversized                         atomic.Uint64
	bodyReadFailures                  atomic.Uint64
	authenticationBad                 atomic.Uint64
	authenticationInvalidNonce        atomic.Uint64
	authenticationStaleRequest        atomic.Uint64
	authenticationUnregisteredAgent   atomic.Uint64
	authenticationUnknownKey          atomic.Uint64
	authenticationRegistryUnavailable atomic.Uint64
	authenticationInvalidSignature    atomic.Uint64
	authenticationReplayedRequest     atomic.Uint64
	authenticationStorageUnavailable  atomic.Uint64
	authenticationOther               atomic.Uint64
}

type authContext struct {
	agent RegistryAgent
	keyID string
	key   ed25519.PublicKey
}

func NewServer(store Store, registry RegistrySource, notifier Notifier, config ServerConfig) (*Server, error) {
	if notifier == nil {
		return nil, errors.New("notifier is required")
	}
	if config.RequestSkew <= 0 {
		config.RequestSkew = 5 * time.Minute
	}
	if config.RequestSkew > requestNonceRetention/2 {
		return nil, errors.New("request skew cannot exceed half the replay nonce retention window")
	}
	if config.RetryInterval == 0 {
		config.RetryInterval = 30 * time.Second
	}
	if config.RetryInterval < MinimumRetryInterval {
		return nil, errors.New("retry interval must be at least 1s")
	}
	if config.MaxDoorbellAttempts <= 0 {
		config.MaxDoorbellAttempts = 3
	}
	if config.MaxPollLimit <= 0 {
		config.MaxPollLimit = 100
	}
	return &Server{store: store, registry: registry, notifier: notifier, config: config, now: time.Now, hub: newDoorbellHub(), revokedAuditedAt: make(map[string]time.Time)}, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !hasRequestSignature(r) {
		s.preAuth.unsigned.Add(1)
		writeAuthenticationFailure(w)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBytes+1))
	if err != nil {
		s.preAuth.bodyReadFailures.Add(1)
		writeError(w, http.StatusBadRequest, "invalid_body", "request body could not be read")
		return
	}
	if len(body) > MaxRequestBytes {
		s.preAuth.oversized.Add(1)
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 64 KiB")
		return
	}
	identity, apiErr := s.authenticate(r, body)
	if apiErr != nil {
		s.recordAuthenticationFailure(apiErr.Code)
		writeAuthenticationFailure(w)
		return
	}
	s.observeRegistry(r.Context())
	r.Body = io.NopCloser(bytes.NewReader(body))
	s.route(w, r, identity)
}

func (s *Server) route(w http.ResponseWriter, r *http.Request, identity authContext) {
	switch {
	case r.URL.Path == "/v1/messages" && r.Method == http.MethodPost:
		s.send(w, r, identity)
	case r.URL.Path == "/v1/messages" && r.Method == http.MethodGet:
		s.poll(w, r, identity)
	case r.URL.Path == "/v1/events" && r.Method == http.MethodGet:
		s.events(w, r, identity)
	case strings.HasPrefix(r.URL.Path, "/v1/messages/") && strings.HasSuffix(r.URL.Path, "/ack") && r.Method == http.MethodPost:
		s.ack(w, r, identity)
	case r.URL.Path == "/v1/spaces" && r.Method == http.MethodGet:
		s.listSpaces(w, r, identity)
	default:
		writeError(w, http.StatusNotFound, "not_found", "route not found")
	}
}

func (s *Server) authenticate(r *http.Request, body []byte) (authContext, *APIError) {
	agentID := r.Header.Get(HeaderAgent)
	keyID := r.Header.Get(HeaderKeyID)
	timestamp := r.Header.Get(HeaderTimestamp)
	nonce := r.Header.Get(HeaderNonce)
	signature := r.Header.Get(HeaderSignature)
	if !noncePattern.MatchString(nonce) {
		return authContext{}, &APIError{Code: "invalid_nonce", Message: "request nonce must be 16 to 128 URL-safe characters"}
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	requestTime := time.Unix(seconds, 0)
	now := s.now()
	if err != nil || requestTime.Before(now.Add(-s.config.RequestSkew)) || requestTime.After(now.Add(s.config.RequestSkew)) {
		return authContext{}, &APIError{Code: "stale_request", Message: "request timestamp is outside the accepted clock window"}
	}
	verify := func(public ed25519.PublicKey) bool {
		sig, err := base64.StdEncoding.DecodeString(signature)
		return err == nil && len(sig) == ed25519.SignatureSize && ed25519.Verify(public, RequestSigningBytes(agentID, keyID, r.Method, r.URL.RequestURI(), timestamp, nonce, body), sig)
	}
	agent, public, err := s.registry.Key(agentID, keyID)
	if err != nil {
		if errors.Is(err, ErrUnknownAgent) || errors.Is(err, ErrUnknownKey) {
			s.auditRevokedCredential(r, agentID, keyID, verify)
		}
		if errors.Is(err, ErrUnknownAgent) {
			return authContext{}, &APIError{Code: "unregistered_agent", Message: "agent is unknown, disabled, or revoked"}
		}
		if errors.Is(err, ErrUnknownKey) {
			return authContext{}, &APIError{Code: "unknown_key", Message: "signing key is unknown, disabled, or revoked"}
		}
		return authContext{}, &APIError{Code: "registry_unavailable", Message: "agent registry could not be loaded"}
	}
	if !verify(public) {
		return authContext{}, &APIError{Code: "invalid_signature", Message: "request signature is invalid"}
	}
	err = s.store.RecordNonce(r.Context(), agentID, nonce, s.now())
	if err != nil {
		if errors.Is(err, ErrReplay) {
			return authContext{}, &APIError{Code: "replayed_request", Message: "request nonce has already been used"}
		}
		return authContext{}, &APIError{Code: "storage_unavailable", Message: "request replay state could not be saved"}
	}
	return authContext{agent: agent, keyID: keyID, key: public}, nil
}

// auditRevokedCredential records use of a key that is still listed but
// disabled, directly or through its agent. Only a request whose signature
// verifies against that key is recorded, so unknown callers still cannot grow
// the audit log, and rows are capped per agent and key.
func (s *Server) auditRevokedCredential(r *http.Request, agentID, keyID string, verify func(ed25519.PublicKey) bool) {
	agent, public, err := s.registry.DisabledKey(agentID, keyID)
	if err != nil || !verify(public) {
		return
	}
	now := s.now()
	capKey := agentID + "\x00" + keyID
	s.revokedAuditMu.Lock()
	for key, at := range s.revokedAuditedAt {
		if now.Sub(at) >= revokedCredentialAuditInterval {
			delete(s.revokedAuditedAt, key)
		}
	}
	if _, recent := s.revokedAuditedAt[capKey]; recent {
		s.revokedAuditMu.Unlock()
		return
	}
	s.revokedAuditedAt[capKey] = now
	s.revokedAuditMu.Unlock()

	s.observeRegistry(r.Context())
	code := "key_disabled"
	if agent.Disabled {
		code = "agent_disabled"
	}
	path := r.URL.Path
	if len(path) > 200 {
		path = path[:200]
	}
	if err := s.store.AppendAudit(r.Context(), AuditRecord{
		Action: "auth.revoked_credential", ActorID: agentID, TenantID: agent.TenantID,
		Outcome: "rejected", Code: code,
		Detail: map[string]any{"key_id": keyID, "method": r.Method, "path": path},
	}); err != nil {
		log.Printf("record revoked credential use by %s/%s: %v", agentID, keyID, err)
	}
}

// observeRegistry appends one registry.changed audit row for each registry
// edit the service observes. The store records the hash transition and the
// audit row together, so restarts and concurrent instances record a change
// once.
func (s *Server) observeRegistry(ctx context.Context) {
	snapshot, err := s.registry.Snapshot()
	if err != nil {
		return // Requests already fail closed on an unreadable registry.
	}
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	if snapshot.SHA256 == s.registrySHA256 {
		return
	}
	for attempt := 0; attempt < 3; attempt++ {
		state, err := s.store.RegistryState(ctx)
		if err != nil {
			log.Printf("read recorded registry state: %v", err)
			return
		}
		previousSHA := state.SHA256
		if previousSHA == snapshot.SHA256 {
			s.registrySHA256 = snapshot.SHA256
			return
		}
		previous := RegistrySnapshot{Agents: map[string]RegistryAgent{}, Spaces: map[string]RegistrySpace{}}
		if err := decodeRecordedJSON(state.AgentsJSON, &previous.Agents); err != nil {
			log.Printf("decode recorded registry agents: %v", err)
			return
		}
		if err := decodeRecordedJSON(state.SpacesJSON, &previous.Spaces); err != nil {
			log.Printf("decode recorded registry spaces: %v", err)
			return
		}
		agentsJSON, err := json.Marshal(snapshot.Agents)
		if err != nil {
			log.Printf("encode registry state: %v", err)
			return
		}
		spacesJSON, err := json.Marshal(snapshot.Spaces)
		if err != nil {
			log.Printf("encode registry state: %v", err)
			return
		}
		recorded, err := s.store.RecordRegistryChange(ctx, RegistryChange{
			PreviousSHA256: previousSHA, SHA256: snapshot.SHA256, AgentsJSON: agentsJSON, SpacesJSON: spacesJSON, ObservedAt: s.now(),
			Audit: AuditRecord{Action: "registry.changed", ActorID: "system", Outcome: "recorded", Detail: registryChangeDetail(previousSHA, snapshot.SHA256, previous, snapshot)},
		})
		if err != nil {
			log.Printf("record registry change: %v", err)
			return
		}
		if recorded {
			s.registrySHA256 = snapshot.SHA256
			return
		}
		// Another instance recorded a change first; diff against its state.
	}
}

// decodeRecordedJSON decodes one recorded registry column; a column written
// before spaces existed is empty.
func decodeRecordedJSON[T any](data []byte, target *T) error {
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, target)
}

type membershipChange struct {
	Space     string   `json:"space"`
	Agent     string   `json:"agent"`
	OldGrants []string `json:"old_grants"`
	NewGrants []string `json:"new_grants"`
}

func registryChangeDetail(oldSHA, newSHA string, previousState, currentState RegistrySnapshot) map[string]any {
	previous, current := previousState.Agents, currentState.Agents
	added, removed, disabled, enabled, keysChanged, policyChanged := []string{}, []string{}, []string{}, []string{}, []string{}, []string{}
	for id, agent := range current {
		before, existed := previous[id]
		if !existed {
			added = append(added, id)
			continue
		}
		if agent.Disabled && !before.Disabled {
			disabled = append(disabled, id)
		}
		if !agent.Disabled && before.Disabled {
			enabled = append(enabled, id)
		}
		if !slices.Equal(agent.PublicKeys, before.PublicKeys) {
			keysChanged = append(keysChanged, id)
		}
		if agent.TenantID != before.TenantID || !slices.Equal(agent.AllowedRecipients, before.AllowedRecipients) || !slices.Equal(agent.AllowedKinds, before.AllowedKinds) {
			policyChanged = append(policyChanged, id)
		}
	}
	for id := range previous {
		if _, exists := current[id]; !exists {
			removed = append(removed, id)
		}
	}
	spacesAdded, spacesRemoved, spacesChanged := []string{}, []string{}, []string{}
	memberships := []membershipChange{}
	for id, space := range currentState.Spaces {
		before, existed := previousState.Spaces[id]
		if !existed {
			spacesAdded = append(spacesAdded, id)
		} else if before.TenantID != space.TenantID {
			spacesChanged = append(spacesChanged, id)
		}
		memberships = append(memberships, diffMemberships(id, before.Members, space.Members)...)
	}
	for id, space := range previousState.Spaces {
		if _, exists := currentState.Spaces[id]; !exists {
			spacesRemoved = append(spacesRemoved, id)
			memberships = append(memberships, diffMemberships(id, space.Members, nil)...)
		}
	}
	for _, list := range [][]string{added, removed, disabled, enabled, keysChanged, policyChanged, spacesAdded, spacesRemoved, spacesChanged} {
		slices.Sort(list)
	}
	slices.SortFunc(memberships, func(a, b membershipChange) int {
		if order := strings.Compare(a.Space, b.Space); order != 0 {
			return order
		}
		return strings.Compare(a.Agent, b.Agent)
	})
	return map[string]any{
		"old_sha256": oldSHA, "new_sha256": newSHA,
		"added": added, "removed": removed, "disabled": disabled, "enabled": enabled,
		"keys_changed": keysChanged, "policy_changed": policyChanged,
		"spaces_added": spacesAdded, "spaces_removed": spacesRemoved, "spaces_changed": spacesChanged,
		"memberships_changed": memberships,
	}
}

// diffMemberships lists each agent whose grants in one space differ between
// two member lists. An added member has no old grants; a removed one has no
// new grants.
func diffMemberships(spaceID string, before, after []RegistrySpaceMember) []membershipChange {
	grants := func(members []RegistrySpaceMember) map[string][]string {
		out := make(map[string][]string, len(members))
		for _, member := range members {
			sorted := slices.Clone(member.Grants)
			slices.Sort(sorted)
			out[member.Agent] = sorted
		}
		return out
	}
	old, current := grants(before), grants(after)
	changes := []membershipChange{}
	for agent, newGrants := range current {
		if oldGrants, existed := old[agent]; !existed || !slices.Equal(oldGrants, newGrants) {
			if !existed {
				oldGrants = []string{}
			}
			changes = append(changes, membershipChange{Space: spaceID, Agent: agent, OldGrants: oldGrants, NewGrants: newGrants})
		}
	}
	for agent, oldGrants := range old {
		if _, kept := current[agent]; !kept {
			changes = append(changes, membershipChange{Space: spaceID, Agent: agent, OldGrants: oldGrants, NewGrants: []string{}})
		}
	}
	return changes
}

func (s *Server) send(w http.ResponseWriter, r *http.Request, auth authContext) {
	var e Envelope
	if err := decodeStrict(r.Body, &e); err != nil {
		s.rejectSend(w, auth.agent, "invalid_json", "request must be a valid message envelope", "")
		return
	}
	if !VerifyEnvelope(e, auth.key) {
		s.rejectSend(w, auth.agent, "invalid_message_signature", "message envelope signature is invalid", "")
		return
	}
	messageID := e.ID
	if err := validateEnvelope(e, auth); err != nil {
		s.rejectSend(w, auth.agent, err.code, err.message, messageID)
		return
	}
	existing, err := s.store.GetMessage(r.Context(), e.ID, auth.agent.TenantID)
	switch {
	case err == nil:
		storedBytes, storedErr := envelopeRetryBytes(existing.Envelope)
		retryBytes, retryErr := envelopeRetryBytes(e)
		if existing.SenderID != auth.agent.ID || storedErr != nil || retryErr != nil || !bytes.Equal(storedBytes, retryBytes) {
			s.rejectSend(w, auth.agent, "message_id_conflict", "message id already exists with different signed content", messageID)
			return
		}
		if err := s.store.AppendAudit(r.Context(), AuditRecord{Action: "send", ActorID: e.SenderID, SubjectID: e.ID, TenantID: auth.agent.TenantID, Outcome: "duplicate", Code: "duplicate_message"}); err != nil {
			log.Printf("record duplicate send %s: %v", e.ID, err)
			writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "message retry could not be recorded")
			return
		}
		writeJSON(w, http.StatusOK, SendResponse{Envelope: existing.Envelope, TenantID: existing.TenantID, Sequence: existing.Sequence, AcceptedAt: existing.AcceptedAt})
		return
	case !errors.Is(err, ErrMessageNotFound):
		log.Printf("check message retry %s: %v", e.ID, err)
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "message retry could not be checked")
		return
	}
	if !messageIDPattern.MatchString(e.ID) {
		s.rejectSend(w, auth.agent, "invalid_id", "new message id must be a UUID v4, UUID v7, or ULID", messageID)
		return
	}
	if e.Kind == "instruction" {
		if !messageIDPattern.MatchString(e.TaskID) {
			s.rejectSend(w, auth.agent, "invalid_task_id", "new task_id must be a UUID v4, UUID v7, or ULID", messageID)
			return
		}
		if !messageIDPattern.MatchString(e.ThreadID) {
			s.rejectSend(w, auth.agent, "invalid_thread_id", "new thread_id must be a UUID v4, UUID v7, or ULID", messageID)
			return
		}
	}
	var recipients []string
	var auditDetail map[string]any
	if e.SpaceID != "" {
		var ok bool
		if recipients, auditDetail, ok = s.spaceAudience(w, auth, e); !ok {
			return
		}
	} else {
		if !s.directRecipientAllowed(w, auth, e) {
			return
		}
		recipients = []string{e.RecipientID}
	}
	if e.ReplyTo != "" {
		original, err := s.store.GetReplyTarget(r.Context(), e.ReplyTo, auth.agent.TenantID, auth.agent.ID)
		if err != nil {
			if errors.Is(err, ErrMessageNotFound) {
				s.rejectReply(w, auth.agent, "reply_not_found", messageID)
			} else {
				log.Printf("lookup reply target %s: %v", e.ReplyTo, err)
				writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "reply target could not be read")
			}
			return
		}
		if !replyCorrelates(original, e) {
			s.rejectReply(w, auth.agent, "reply_correlation_mismatch", messageID)
			return
		}
	} else if e.Kind == "result" {
		s.rejectSend(w, auth.agent, "reply_to_required", "result messages must reference the instruction they answer", messageID)
		return
	}
	message, duplicate, err := s.store.CreateMessage(r.Context(), e, auth.agent.TenantID, recipients, auditDetail)
	if err != nil {
		if errors.Is(err, ErrMessageConflict) {
			s.rejectSend(w, auth.agent, "message_id_conflict", "message id already exists with different signed content", messageID)
		} else {
			log.Printf("persist message %s: %v", e.ID, err)
			s.rejectSend(w, auth.agent, "storage_unavailable", "message could not be persisted", messageID)
		}
		return
	}
	if !duplicate {
		for _, recipient := range recipients {
			s.ring(message, recipient)
		}
		s.notify(Notification{Event: "message.accepted", OccurredAt: s.now().UTC().Format(time.RFC3339Nano), MessageID: e.ID, SenderID: e.SenderID, RecipientID: e.RecipientID, SpaceID: e.SpaceID, TenantID: auth.agent.TenantID, Kind: e.Kind})
	}
	status := http.StatusCreated
	if duplicate {
		status = http.StatusOK
	}
	writeJSON(w, status, SendResponse{
		Envelope: message.Envelope, TenantID: message.TenantID,
		Sequence: message.Sequence, AcceptedAt: message.AcceptedAt,
	})
}

// directRecipientAllowed applies the registry checks for a message addressed
// to one agent, rejecting the send when one fails.
func (s *Server) directRecipientAllowed(w http.ResponseWriter, auth authContext, e Envelope) bool {
	if !containsString(auth.agent.AllowedRecipients, e.RecipientID) {
		s.rejectRecipient(w, auth.agent, "recipient_not_allowed", e.ID)
		return false
	}
	recipient, err := s.registry.Agent(e.RecipientID)
	if err != nil {
		if errors.Is(err, ErrUnknownAgent) {
			s.rejectRecipient(w, auth.agent, "unknown_recipient", e.ID)
		} else {
			s.rejectSend(w, auth.agent, "registry_unavailable", "agent registry could not be loaded", e.ID)
		}
		return false
	}
	if !hasActiveKey(recipient) {
		s.rejectRecipient(w, auth.agent, "recipient_key_unavailable", e.ID)
		return false
	}
	if !s.kindAllowed(w, auth, e) {
		return false
	}
	if recipient.TenantID != auth.agent.TenantID {
		s.rejectRecipient(w, auth.agent, "wrong_tenant", e.ID)
		return false
	}
	return true
}

// spaceAudience checks that the sender may post to the message's space and
// returns its audience: every active member holding read at this moment,
// sender excluded, plus the send audit detail that records it. The audience
// comes from one registry read, whose hash the detail names, so later grant
// changes cannot rewrite who a message went to.
func (s *Server) spaceAudience(w http.ResponseWriter, auth authContext, e Envelope) ([]string, map[string]any, bool) {
	snapshot, err := s.registry.Snapshot()
	if err != nil {
		s.rejectSend(w, auth.agent, "registry_unavailable", "agent registry could not be loaded", e.ID)
		return nil, nil, false
	}
	space, exists := snapshot.Spaces[e.SpaceID]
	sender, listed := snapshot.Agents[auth.agent.ID]
	grant, member := spaceGrant(snapshot, space, sender)
	if !exists || !listed || sender.Disabled || sender.TenantID != auth.agent.TenantID || !member {
		s.rejectSendWithAuditCode(w, auth.agent, "space_not_found", "not_member", "space was not found", e.ID)
		return nil, nil, false
	}
	if !grant.Has(GrantPost) {
		s.rejectSendWithAuditCode(w, auth.agent, "post_not_allowed", "post_not_granted", "sender does not hold post in this space", e.ID)
		return nil, nil, false
	}
	if !s.kindAllowed(w, auth, e) {
		return nil, nil, false
	}
	audience := []string{}
	for _, m := range space.Members {
		agent, known := snapshot.Agents[m.Agent]
		if m.Agent == auth.agent.ID || !slices.Contains(m.Grants, GrantRead) || !known || agent.Disabled || agent.TenantID != space.TenantID || !hasActiveKey(agent) {
			continue
		}
		audience = append(audience, m.Agent)
	}
	slices.Sort(audience)
	return audience, map[string]any{"space_id": space.ID, "recipients": audience, "registry_sha256": snapshot.SHA256}, true
}

func (s *Server) kindAllowed(w http.ResponseWriter, auth authContext, e Envelope) bool {
	if e.Kind != "instruction" && e.Kind != "result" {
		s.rejectSend(w, auth.agent, "kind_not_allowed", "this pilot accepts instruction and result messages only", e.ID)
		return false
	}
	if !containsString(auth.agent.AllowedKinds, e.Kind) {
		s.rejectSend(w, auth.agent, "kind_not_allowed", "sender is not allowed to send this message kind", e.ID)
		return false
	}
	return true
}

// replyCorrelates reports whether e answers original: same task and thread, a
// result answers an instruction, and the reply goes back where the original
// came from, to its sender directly or to the same space.
func replyCorrelates(original DeliveredMessage, e Envelope) bool {
	if (e.Kind == "result" && original.Kind != "instruction") || original.ThreadID != e.ThreadID || original.TaskID != e.TaskID {
		return false
	}
	if e.SpaceID != "" {
		return original.SpaceID == e.SpaceID
	}
	return original.SpaceID == "" && original.SenderID == e.RecipientID && original.RecipientID == e.SenderID
}

func hasActiveKey(agent RegistryAgent) bool {
	return contains(agent.PublicKeys, func(k RegistryKey) bool { return !k.Disabled })
}

type validationError struct{ code, message string }

func (e *validationError) Error() string { return e.message }

func validateEnvelope(e Envelope, auth authContext) *validationError {
	if e.Type != EnvelopeType {
		return &validationError{"invalid_type", "type must be message"}
	}
	if !referenceIDPattern.MatchString(e.ID) {
		return &validationError{"invalid_id", "id must be a UUID or ULID"}
	}
	for label, value := range map[string]string{"task_id": e.TaskID, "thread_id": e.ThreadID} {
		if !referenceIDPattern.MatchString(value) {
			return &validationError{"invalid_" + label, label + " must be a UUID or ULID"}
		}
	}
	if e.ReplyTo != "" && !referenceIDPattern.MatchString(e.ReplyTo) {
		return &validationError{"invalid_reply_to", "reply_to must be a UUID or ULID"}
	}
	if e.SenderID != auth.agent.ID {
		return &validationError{"sender_mismatch", "sender_id must match the authenticated agent"}
	}
	if e.KeyID != auth.keyID {
		return &validationError{"key_mismatch", "message key_id must match the authenticated signing key"}
	}
	switch {
	case e.RecipientID != "" && e.SpaceID != "":
		return &validationError{"space_recipient_conflict", "set exactly one of recipient_id or space_id"}
	case e.RecipientID == "" && e.SpaceID == "":
		return &validationError{"recipient_required", "recipient_id or space_id must be explicitly addressed"}
	case e.SpaceID != "" && !registryIDPattern.MatchString(e.SpaceID):
		return &validationError{"invalid_space_id", "space_id must be a registry space ID"}
	}
	if !KindAllowed(e.Kind) {
		return &validationError{"invalid_kind", "kind must be instruction, result, finding, status, or ack"}
	}
	if _, err := time.Parse(time.RFC3339Nano, e.CreatedAt); err != nil {
		return &validationError{"invalid_created_at", "created_at must be an RFC3339 timestamp"}
	}
	if len(e.Payload) > MaxPayloadBytes {
		return &validationError{"payload_too_large", "payload must be 16 KiB or smaller; use artifact references for larger files"}
	}
	if !json.Valid(e.Payload) {
		return &validationError{"invalid_payload", "payload must be valid JSON"}
	}
	if !e.ContentIsData {
		return &validationError{"content_marker_required", "content_is_data must be true; message content is data, not authority"}
	}
	if len(e.Artifacts) > 8 {
		return &validationError{"too_many_artifacts", "a message may reference at most 8 artifacts"}
	}
	for _, artifact := range e.Artifacts {
		u, err := url.Parse(artifact.URI)
		if err != nil || u.Scheme == "" || u.User != nil || strings.EqualFold(u.Scheme, "data") || strings.EqualFold(u.Scheme, "javascript") || len(artifact.URI) > 2048 {
			return &validationError{"invalid_artifact_uri", "artifact uri must be a safe absolute reference"}
		}
		if artifact.MediaType == "" || len(artifact.MediaType) > 128 || artifact.Size < 0 || !shaPattern.MatchString(artifact.SHA256) {
			return &validationError{"invalid_artifact", "artifact needs media_type, non-negative size, and lowercase sha256"}
		}
	}
	canonical, _ := EnvelopeSigningBytes(e)
	if containsSecret(string(canonical)) {
		return &validationError{"secret_detected", "payload, provenance, or artifact reference resembles a secret and was rejected"}
	}
	return nil
}

func (s *Server) rejectSend(w http.ResponseWriter, agent RegistryAgent, code, message, messageID string) {
	s.rejectSendWithAuditCode(w, agent, code, code, message, messageID)
}

func (s *Server) rejectRecipient(w http.ResponseWriter, agent RegistryAgent, reason, messageID string) {
	s.rejectSendWithAuditCode(w, agent, "recipient_not_allowed", reason, "recipient is unavailable or not allowed", messageID)
}

func (s *Server) rejectReply(w http.ResponseWriter, agent RegistryAgent, reason, messageID string) {
	s.rejectSendWithAuditCode(w, agent, "invalid_reply_to", reason, "reply_to must reference a correlated message addressed to this sender", messageID)
}

func (s *Server) rejectSendWithAuditCode(w http.ResponseWriter, agent RegistryAgent, code, auditCode, message, messageID string) {
	if !messageIDPattern.MatchString(messageID) {
		messageID = ""
	}
	if err := s.store.AppendAudit(context.Background(), AuditRecord{Action: "send", ActorID: agent.ID, SubjectID: messageID, TenantID: agent.TenantID, Outcome: "rejected", Code: auditCode}); err != nil {
		log.Printf("record rejected send by %s (%s): %v", agent.ID, auditCode, err)
	}
	s.notify(Notification{Event: "message.rejected", OccurredAt: s.now().UTC().Format(time.RFC3339Nano), MessageID: messageID, SenderID: agent.ID, TenantID: agent.TenantID, Code: code})
	status := http.StatusBadRequest
	switch code {
	case "secret_detected", "recipient_not_allowed", "kind_not_allowed", "post_not_allowed":
		status = http.StatusForbidden
	case "space_not_found":
		status = http.StatusNotFound
	case "message_id_conflict":
		status = http.StatusConflict
	case "registry_unavailable", "storage_unavailable":
		status = http.StatusServiceUnavailable
	}
	writeError(w, status, code, message)
}

func (s *Server) poll(w http.ResponseWriter, r *http.Request, auth authContext) {
	limit := s.config.MaxPollLimit
	query := r.URL.Query()
	for key := range query {
		if key != "limit" {
			writeError(w, http.StatusBadRequest, "unsupported_query_parameter", "poll accepts only the limit parameter")
			return
		}
	}
	if values, exists := query["limit"]; exists {
		if len(values) != 1 || values[0] == "" {
			writeError(w, http.StatusBadRequest, "invalid_limit", fmt.Sprintf("limit must be between 1 and %d", s.config.MaxPollLimit))
			return
		}
		raw := values[0]
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > s.config.MaxPollLimit {
			writeError(w, http.StatusBadRequest, "invalid_limit", fmt.Sprintf("limit must be between 1 and %d", s.config.MaxPollLimit))
			return
		}
		limit = parsed
	}
	readable, err := s.readableSpaces(auth.agent)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "registry_unavailable", "space grants could not be read")
		return
	}
	messages, err := s.store.ListMessages(r.Context(), auth.agent.ID, auth.agent.TenantID, readable, limit)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "messages could not be read")
		return
	}
	authorStatus := map[string]string{}
	for i := range messages {
		sender := messages[i].SenderID
		status, known := authorStatus[sender]
		if !known {
			if _, revoked, err := s.revocation(sender, messages[i].TenantID); err == nil && revoked {
				status = AuthorStatusRevoked
			}
			authorStatus[sender] = status
		}
		messages[i].AuthorStatus = status
	}
	writeJSON(w, http.StatusOK, PollResponse{Messages: messages})
}

// readableSpaces lists the spaces whose group messages the agent may read at
// request time. A member removed from a space, or left without read, stops
// seeing its deliveries; the rows are kept.
func (s *Server) readableSpaces(agent RegistryAgent) ([]string, error) {
	grants, err := s.registry.SpaceGrants(agent.ID)
	if err != nil {
		return nil, err
	}
	spaces := []string{}
	for _, grant := range grants {
		if grant.TenantID == agent.TenantID && grant.Has(GrantRead) {
			spaces = append(spaces, grant.SpaceID)
		}
	}
	return spaces, nil
}

// listSpaces returns the caller's own memberships, read from the registry at
// request time. Other members appear by ID only, never with their grants.
func (s *Server) listSpaces(w http.ResponseWriter, r *http.Request, auth authContext) {
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "unsupported_query_parameter", "spaces accepts no query parameters")
		return
	}
	grants, err := s.registry.SpaceGrants(auth.agent.ID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "registry_unavailable", "spaces could not be read")
		return
	}
	spaces := make([]Space, 0, len(grants))
	for _, grant := range grants {
		if grant.TenantID != auth.agent.TenantID {
			continue
		}
		spaces = append(spaces, Space{ID: grant.SpaceID, TenantID: grant.TenantID, Grants: grant.Grants, Members: grant.Members})
	}
	writeJSON(w, http.StatusOK, SpacesResponse{Spaces: spaces})
}

func (s *Server) ack(w http.ResponseWriter, r *http.Request, auth authContext) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "messages" || parts[3] != "ack" || !referenceIDPattern.MatchString(parts[2]) {
		s.rejectAck(w, auth.agent, "not_found", "acknowledgement route not found", http.StatusNotFound)
		return
	}
	var body AckRequest
	if err := decodeStrict(r.Body, &body); err != nil || !body.Processed {
		s.rejectAck(w, auth.agent, "processing_required", "acknowledgement must assert processed=true", http.StatusBadRequest)
		return
	}
	readable, err := s.readableSpaces(auth.agent)
	if err != nil {
		s.rejectAck(w, auth.agent, "ack_failed", "message could not be acknowledged", http.StatusServiceUnavailable)
		return
	}
	duplicate, err := s.store.Acknowledge(r.Context(), parts[2], auth.agent.ID, auth.agent.TenantID, readable)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotMember):
			s.rejectAckWithAuditCode(w, auth.agent, "message_not_found", "not_member", "message was not found", http.StatusNotFound)
		case errors.Is(err, ErrMessageNotFound):
			s.rejectAckWithAuditCode(w, auth.agent, "message_not_found", "message_not_found", "message was not found", http.StatusNotFound)
		case errors.Is(err, ErrNotRecipient):
			s.rejectAckWithAuditCode(w, auth.agent, "message_not_found", "not_recipient", "message was not found", http.StatusNotFound)
		case errors.Is(err, ErrWrongTenant):
			s.rejectAckWithAuditCode(w, auth.agent, "message_not_found", "wrong_tenant", "message was not found", http.StatusNotFound)
		default:
			s.rejectAck(w, auth.agent, "ack_failed", "message could not be acknowledged", http.StatusServiceUnavailable)
		}
		return
	}
	if !duplicate {
		s.notify(Notification{Event: "message.acknowledged", OccurredAt: s.now().UTC().Format(time.RFC3339Nano), MessageID: parts[2], RecipientID: auth.agent.ID, TenantID: auth.agent.TenantID})
	}
	writeJSON(w, http.StatusOK, map[string]any{"message_id": parts[2], "acknowledged": true, "duplicate": duplicate})
}

func (s *Server) rejectAck(w http.ResponseWriter, agent RegistryAgent, code, message string, status int) {
	s.rejectAckWithAuditCode(w, agent, code, code, message, status)
}

func (s *Server) rejectAckWithAuditCode(w http.ResponseWriter, agent RegistryAgent, code, auditCode, message string, status int) {
	if err := s.store.AppendAudit(context.Background(), AuditRecord{Action: "ack", ActorID: agent.ID, TenantID: agent.TenantID, Outcome: "rejected", Code: auditCode}); err != nil {
		log.Printf("record rejected acknowledgement: %v", err)
	}
	writeError(w, status, code, message)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request, auth authContext) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "stream_unavailable", "server does not support event streaming")
		return
	}
	ch, remove := s.hub.subscribe(auth.agent.ID)
	defer remove()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-ch:
			if !s.streamAuthorized(auth) {
				return
			}
			encoded, _ := json.Marshal(event)
			_, _ = fmt.Fprintf(w, "id: %d\nevent: doorbell\ndata: %s\n\n", event.Sequence, encoded)
			flusher.Flush()
		case <-heartbeat.C:
			if !s.streamAuthorized(auth) {
				return
			}
			_, _ = fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) RunPreAuthMetricsLog(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.logPreAuthMetrics()
		}
	}
}

func (s *Server) logPreAuthMetrics() {
	unsigned := s.preAuth.unsigned.Load()
	oversized := s.preAuth.oversized.Load()
	bodyFailures := s.preAuth.bodyReadFailures.Load()
	authFailures := s.preAuth.authenticationBad.Load()
	invalidNonce := s.preAuth.authenticationInvalidNonce.Load()
	staleRequest := s.preAuth.authenticationStaleRequest.Load()
	unregisteredAgent := s.preAuth.authenticationUnregisteredAgent.Load()
	unknownKey := s.preAuth.authenticationUnknownKey.Load()
	registryUnavailable := s.preAuth.authenticationRegistryUnavailable.Load()
	invalidSignature := s.preAuth.authenticationInvalidSignature.Load()
	replayedRequest := s.preAuth.authenticationReplayedRequest.Load()
	storageUnavailable := s.preAuth.authenticationStorageUnavailable.Load()
	other := s.preAuth.authenticationOther.Load()
	if unsigned+oversized+bodyFailures+authFailures == 0 {
		return
	}
	log.Printf("agent-inbox unauthenticated request totals: unsigned=%d oversized=%d body_read_failures=%d authentication_failures=%d invalid_nonce=%d stale_request=%d unregistered_agent=%d unknown_key=%d registry_unavailable=%d invalid_signature=%d replayed_request=%d storage_unavailable=%d other=%d", unsigned, oversized, bodyFailures, authFailures, invalidNonce, staleRequest, unregisteredAgent, unknownKey, registryUnavailable, invalidSignature, replayedRequest, storageUnavailable, other)
}

func (s *Server) recordAuthenticationFailure(reason string) {
	s.preAuth.authenticationBad.Add(1)
	switch reason {
	case "invalid_nonce":
		s.preAuth.authenticationInvalidNonce.Add(1)
	case "stale_request":
		s.preAuth.authenticationStaleRequest.Add(1)
	case "unregistered_agent":
		s.preAuth.authenticationUnregisteredAgent.Add(1)
	case "unknown_key":
		s.preAuth.authenticationUnknownKey.Add(1)
	case "registry_unavailable":
		s.preAuth.authenticationRegistryUnavailable.Add(1)
	case "invalid_signature":
		s.preAuth.authenticationInvalidSignature.Add(1)
	case "replayed_request":
		s.preAuth.authenticationReplayedRequest.Add(1)
	case "storage_unavailable":
		s.preAuth.authenticationStorageUnavailable.Add(1)
	default:
		s.preAuth.authenticationOther.Add(1)
	}
}

func (s *Server) RunNotifications(ctx context.Context) {
	tickerInterval := min(s.config.RetryInterval/2, 10*time.Second)
	ticker := time.NewTicker(tickerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.dispatchDue(ctx)
		}
	}
}

func (s *Server) dispatchDue(ctx context.Context) {
	s.observeRegistry(ctx)
	candidates, err := s.store.DueNotifications(ctx, s.now(), s.config.RetryInterval, s.config.MaxDoorbellAttempts)
	if err != nil {
		log.Printf("load due notifications: %v", err)
		return
	}
	for _, c := range candidates {
		if c.Escalated {
			continue
		}
		reason, revoked, err := s.deliveryRevocation(c)
		if err == nil && revoked {
			marked, err := s.store.MarkRecipientRevoked(ctx, c.MessageID, c.Recipient, reason, s.now())
			if err != nil {
				log.Printf("record revoked recipient %s for %s: %v", c.Recipient, c.MessageID, err)
				continue
			}
			if marked {
				s.notify(Notification{Event: "delivery.recipient_revoked", OccurredAt: s.now().UTC().Format(time.RFC3339Nano), MessageID: c.MessageID, RecipientID: c.Recipient, SpaceID: c.SpaceID, TenantID: c.TenantID, Code: reason})
			}
			continue
		}
		if c.Attempts < s.config.MaxDoorbellAttempts {
			if err := s.store.MarkNotified(ctx, c.MessageID, c.Recipient, s.now()); err != nil {
				log.Printf("record notification attempt for %s to %s: %v", c.MessageID, c.Recipient, err)
				continue
			}
			if s.assignedToTenant(c.Recipient, c.TenantID) {
				s.hub.publish(c.Recipient, DoorbellEvent{MessageID: c.MessageID, Sequence: c.Sequence, Recipient: c.Recipient, SpaceID: c.SpaceID})
			}
			continue
		}
		marked, err := s.store.MarkEscalated(ctx, c.MessageID, c.Recipient, s.now())
		if err != nil {
			log.Printf("record escalation for %s to %s: %v", c.MessageID, c.Recipient, err)
			continue
		}
		if marked {
			s.notify(Notification{Event: "message.unacknowledged_escalation", OccurredAt: s.now().UTC().Format(time.RFC3339Nano), MessageID: c.MessageID, RecipientID: c.Recipient, SpaceID: c.SpaceID, TenantID: c.TenantID})
		}
	}
}

func (s *Server) ring(message DeliveredMessage, recipient string) {
	if !s.assignedToTenant(recipient, message.TenantID) {
		return
	}
	if err := s.store.MarkNotified(context.Background(), message.ID, recipient, s.now()); err != nil {
		log.Printf("record initial doorbell for %s to %s: %v", message.ID, recipient, err)
		return
	}
	s.hub.publish(recipient, DoorbellEvent{MessageID: message.ID, Sequence: message.Sequence, Recipient: recipient, SpaceID: message.SpaceID})
}

// deliveryRevocation reports whether a delivery's recipient can no longer
// collect it: the recipient is revoked or, for a group message, no longer
// holds read in the space (reason not_member). A registry read failure is
// returned as an error, never as a revocation.
func (s *Server) deliveryRevocation(c NotificationCandidate) (string, bool, error) {
	reason, revoked, err := s.revocation(c.Recipient, c.TenantID)
	if err != nil || revoked || c.SpaceID == "" {
		return reason, revoked, err
	}
	grant, err := s.registry.Grant(c.SpaceID, c.Recipient)
	switch {
	case errors.Is(err, ErrSpaceNotFound):
		return "not_member", true, nil
	case err != nil:
		return "", false, err
	case !grant.Has(GrantRead):
		return "not_member", true, nil
	}
	return "", false, nil
}

// revocation reports whether an agent can no longer act in a tenant: it was
// removed or disabled, moved to another tenant, or has no active key. A
// registry read failure is returned as an error, never as a revocation.
func (s *Server) revocation(agentID, tenantID string) (string, bool, error) {
	agent, err := s.registry.Agent(agentID)
	switch {
	case errors.Is(err, ErrUnknownAgent):
		return "unknown_recipient", true, nil
	case err != nil:
		return "", false, err
	case agent.TenantID != tenantID:
		return "wrong_tenant", true, nil
	case !contains(agent.PublicKeys, func(k RegistryKey) bool { return !k.Disabled }):
		return "recipient_key_unavailable", true, nil
	}
	return "", false, nil
}

func (s *Server) assignedToTenant(agentID, tenantID string) bool {
	agent, err := s.registry.Agent(agentID)
	return err == nil && agent.TenantID == tenantID
}

func (s *Server) streamAuthorized(auth authContext) bool {
	agent, key, err := s.registry.Key(auth.agent.ID, auth.keyID)
	return err == nil && agent.ID == auth.agent.ID && agent.TenantID == auth.agent.TenantID && bytes.Equal(key, auth.key)
}

func (s *Server) notify(event Notification) {
	if err := s.notifier.Notify(context.Background(), event); err != nil {
		log.Printf("human notification %s failed: %v", event.Event, err)
		actor := event.SenderID
		if actor == "" {
			actor = event.RecipientID
		}
		if actor == "" {
			actor = "system"
		}
		class := "delivery_error"
		if errors.Is(err, context.DeadlineExceeded) {
			class = "timeout"
		} else if errors.Is(err, context.Canceled) {
			class = "canceled"
		}
		detail := map[string]any{"event": event.Event, "error_class": class}
		if event.MessageID != "" {
			detail["message_id"] = event.MessageID
		}
		if auditErr := s.store.AppendAudit(context.Background(), AuditRecord{
			Action: "notification.failed", ActorID: actor, SubjectID: event.MessageID,
			TenantID: event.TenantID, Outcome: "failed", Code: class,
			Detail: detail,
		}); auditErr != nil {
			log.Printf("record notification failure for %s: %v", event.Event, auditErr)
		}
	}
}

func hasRequestSignature(r *http.Request) bool {
	return r.Header.Get(HeaderAgent) != "" &&
		r.Header.Get(HeaderKeyID) != "" &&
		r.Header.Get(HeaderTimestamp) != "" &&
		r.Header.Get(HeaderNonce) != "" &&
		r.Header.Get(HeaderSignature) != ""
}

func decodeStrict(body io.Reader, target any) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	return decodeStrictJSON(data, target)
}

func writeAuthenticationFailure(w http.ResponseWriter) {
	writeError(w, http.StatusUnauthorized, "authentication_failed", "request could not be authenticated")
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorResponse{Error: APIError{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func contains[T any](values []T, match func(T) bool) bool {
	for _, value := range values {
		if match(value) {
			return true
		}
	}
	return false
}

type doorbellHub struct {
	mu          sync.Mutex
	subscribers map[string]map[chan DoorbellEvent]struct{}
}

func newDoorbellHub() *doorbellHub {
	return &doorbellHub{subscribers: make(map[string]map[chan DoorbellEvent]struct{})}
}

func (h *doorbellHub) subscribe(agentID string) (<-chan DoorbellEvent, func()) {
	ch := make(chan DoorbellEvent, 16)
	h.mu.Lock()
	if h.subscribers[agentID] == nil {
		h.subscribers[agentID] = make(map[chan DoorbellEvent]struct{})
	}
	h.subscribers[agentID][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subscribers[agentID], ch)
		if len(h.subscribers[agentID]) == 0 {
			delete(h.subscribers, agentID)
		}
		h.mu.Unlock()
	}
}

func (h *doorbellHub) publish(agentID string, event DoorbellEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subscribers[agentID] {
		select {
		case ch <- event:
		default: // A full or disconnected doorbell never affects durable delivery.
		}
	}
}

func NewNonce() (string, error) {
	var value [24]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value[:]), nil
}
