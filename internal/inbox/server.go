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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	HeaderAgent     = "X-Agent-ID"
	HeaderKeyID     = "X-Key-ID"
	HeaderTimestamp = "X-Request-Timestamp"
	HeaderNonce     = "X-Request-Nonce"
	HeaderSignature = "X-Request-Signature"
)

var (
	messageIDPattern   = regexp.MustCompile(`(?i)^([0-9a-f]{8}-[0-9a-f]{4}-[47][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}|[0-7][0-9a-hjkmnp-tv-z]{25})$`)
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
	if config.RetryInterval <= 0 {
		config.RetryInterval = 30 * time.Second
	}
	if config.MaxDoorbellAttempts <= 0 {
		config.MaxDoorbellAttempts = 3
	}
	if config.MaxPollLimit <= 0 {
		config.MaxPollLimit = 100
	}
	return &Server{store: store, registry: registry, notifier: notifier, config: config, now: time.Now, hub: newDoorbellHub()}, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	if !hasRequestSignature(r) {
		s.preAuth.unsigned.Add(1)
		writeAuthenticationFailure(w)
		return
	}
	identity, apiErr := s.authenticate(r, body)
	if apiErr != nil {
		s.recordAuthenticationFailure(apiErr.Code)
		writeAuthenticationFailure(w)
		return
	}
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
	agent, public, err := s.registry.Key(agentID, keyID)
	if err != nil {
		if errors.Is(err, ErrUnknownAgent) {
			return authContext{}, &APIError{Code: "unregistered_agent", Message: "agent is unknown, disabled, or revoked"}
		}
		if errors.Is(err, ErrUnknownKey) {
			return authContext{}, &APIError{Code: "unknown_key", Message: "signing key is unknown, disabled, or revoked"}
		}
		return authContext{}, &APIError{Code: "registry_unavailable", Message: "agent registry could not be loaded"}
	}
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(public, RequestSigningBytes(agentID, keyID, r.Method, r.URL.RequestURI(), timestamp, nonce, body), sig) {
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
	if !containsString(auth.agent.AllowedRecipients, e.RecipientID) {
		s.rejectRecipient(w, auth.agent, "recipient_not_allowed", messageID)
		return
	}
	recipient, err := s.registry.Agent(e.RecipientID)
	if err != nil {
		if errors.Is(err, ErrUnknownAgent) {
			s.rejectRecipient(w, auth.agent, "unknown_recipient", messageID)
		} else {
			s.rejectSend(w, auth.agent, "registry_unavailable", "agent registry could not be loaded", messageID)
		}
		return
	}
	if !contains(recipient.PublicKeys, func(k RegistryKey) bool { return !k.Disabled }) {
		s.rejectRecipient(w, auth.agent, "recipient_key_unavailable", messageID)
		return
	}
	if e.Kind != "instruction" && e.Kind != "result" {
		s.rejectSend(w, auth.agent, "kind_not_allowed", "this pilot accepts instruction and result messages only", messageID)
		return
	}
	if !containsString(auth.agent.AllowedKinds, e.Kind) {
		s.rejectSend(w, auth.agent, "kind_not_allowed", "sender is not allowed to send this message kind", messageID)
		return
	}
	if recipient.TenantID != auth.agent.TenantID {
		s.rejectSend(w, auth.agent, "wrong_tenant", "sender and recipient must belong to the same server-assigned tenant", messageID)
		return
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
		if original.SenderID != e.RecipientID || original.RecipientID != e.SenderID || original.ThreadID != e.ThreadID || original.TaskID != e.TaskID {
			s.rejectReply(w, auth.agent, "reply_correlation_mismatch", messageID)
			return
		}
	} else if e.Kind == "result" {
		s.rejectSend(w, auth.agent, "reply_to_required", "result messages must reference the instruction they answer", messageID)
		return
	}
	message, duplicate, err := s.store.CreateMessage(r.Context(), e, auth.agent.TenantID)
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
		s.ring(message)
		s.notify(Notification{Event: "message.accepted", OccurredAt: s.now().UTC().Format(time.RFC3339Nano), MessageID: e.ID, SenderID: e.SenderID, RecipientID: e.RecipientID, TenantID: auth.agent.TenantID, Kind: e.Kind})
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
	if e.RecipientID == "" {
		return &validationError{"recipient_required", "recipient_id must be explicitly addressed"}
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
	if err := s.store.AppendAudit(context.Background(), AuditRecord{Action: "send", ActorID: agent.ID, TenantID: agent.TenantID, Outcome: "rejected", Code: auditCode}); err != nil {
		log.Printf("record rejected send by %s (%s): %v", agent.ID, auditCode, err)
	}
	s.notify(Notification{Event: "message.rejected", OccurredAt: s.now().UTC().Format(time.RFC3339Nano), MessageID: messageID, SenderID: agent.ID, TenantID: agent.TenantID, Code: code})
	status := http.StatusBadRequest
	switch code {
	case "secret_detected", "wrong_tenant", "recipient_not_allowed", "kind_not_allowed":
		status = http.StatusForbidden
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
	messages, err := s.store.ListMessages(r.Context(), auth.agent.ID, auth.agent.TenantID, limit)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "messages could not be read")
		return
	}
	writeJSON(w, http.StatusOK, PollResponse{Messages: messages})
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
	duplicate, err := s.store.Acknowledge(r.Context(), parts[2], auth.agent.ID, auth.agent.TenantID)
	if err != nil {
		switch {
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
	w.Header().Set("Cache-Control", "no-cache")
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
	ticker := time.NewTicker(min(s.config.RetryInterval/2, 10*time.Second))
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
	candidates, err := s.store.DueNotifications(ctx, s.now(), s.config.RetryInterval, s.config.MaxDoorbellAttempts)
	if err != nil {
		log.Printf("load due notifications: %v", err)
		return
	}
	for _, c := range candidates {
		if c.Escalated {
			continue
		}
		if c.Attempts < s.config.MaxDoorbellAttempts {
			if s.assignedToTenant(c.Recipient, c.TenantID) {
				s.hub.publish(c.Recipient, DoorbellEvent{MessageID: c.MessageID, Sequence: c.Sequence, Recipient: c.Recipient})
			}
			if err := s.store.MarkNotified(ctx, c.MessageID, s.now()); err != nil {
				log.Printf("record notification attempt for %s: %v", c.MessageID, err)
			}
			continue
		}
		marked, err := s.store.MarkEscalated(ctx, c.MessageID, s.now())
		if err != nil {
			log.Printf("record escalation for %s: %v", c.MessageID, err)
			continue
		}
		if marked {
			s.notify(Notification{Event: "message.unacknowledged_escalation", OccurredAt: s.now().UTC().Format(time.RFC3339Nano), MessageID: c.MessageID, RecipientID: c.Recipient, TenantID: c.TenantID})
		}
	}
}

func (s *Server) ring(message DeliveredMessage) {
	if !s.assignedToTenant(message.RecipientID, message.TenantID) {
		return
	}
	s.hub.publish(message.RecipientID, DoorbellEvent{MessageID: message.ID, Sequence: message.Sequence, Recipient: message.RecipientID})
	if err := s.store.MarkNotified(context.Background(), message.ID, s.now()); err != nil {
		log.Printf("record initial doorbell for %s: %v", message.ID, err)
	}
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
		if auditErr := s.store.AppendAudit(context.Background(), AuditRecord{
			Action: "notification.failed", ActorID: actor, SubjectID: event.MessageID,
			TenantID: event.TenantID, Outcome: "failed", Code: class,
			Detail: map[string]any{"event": event.Event, "message_id": event.MessageID, "error_class": class},
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
