package inbox

import (
	"encoding/json"
	"time"
)

const (
	EnvelopeType    = "message"
	MaxPayloadBytes = 16 * 1024
	MaxRequestBytes = 64 * 1024
)

var validKinds = map[string]bool{
	"instruction": true,
	"result":      true,
	"finding":     true,
	"status":      true,
	"ack":         true,
}

// Artifact is a reference to externally stored content. The inbox never accepts
// artifact bytes.
type Artifact struct {
	URI       string `json:"uri"`
	MediaType string `json:"media_type"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
}

// Envelope is the sender-authored A2A-style message. Tenant and ordering are
// deliberately absent: the server derives both. Exactly one of RecipientID and
// SpaceID is set; SpaceID addresses every member of a space holding read.
type Envelope struct {
	Type              string            `json:"type"`
	ID                string            `json:"id"`
	TaskID            string            `json:"task_id"`
	ThreadID          string            `json:"thread_id"`
	ReplyTo           string            `json:"reply_to,omitempty"`
	SenderID          string            `json:"sender_id"`
	RecipientID       string            `json:"recipient_id"`
	SpaceID           string            `json:"space_id,omitempty"`
	KeyID             string            `json:"key_id"`
	Kind              string            `json:"kind"`
	CreatedAt         string            `json:"created_at"`
	AssertedAuthority string            `json:"asserted_authority,omitempty"`
	ContentIsData     bool              `json:"content_is_data"`
	Payload           json.RawMessage   `json:"payload"`
	Artifacts         []Artifact        `json:"artifacts"`
	Provenance        map[string]string `json:"provenance,omitempty"`
	Signature         string            `json:"signature"`
}

// AuthorStatusRevoked marks a polled message whose sender has since been
// revoked. The message itself is kept as stored and stays attributed.
const AuthorStatusRevoked = "revoked"

type DeliveredMessage struct {
	Envelope
	TenantID       string     `json:"tenant_id"`
	Sequence       int64      `json:"sequence"`
	AcceptedAt     time.Time  `json:"accepted_at"`
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
	// AuthorStatus is computed from the registry at read time; it is empty
	// while the sender is active.
	AuthorStatus string `json:"author_status,omitempty"`
}

type SendResponse struct {
	Envelope
	TenantID   string    `json:"tenant_id"`
	Sequence   int64     `json:"sequence"`
	AcceptedAt time.Time `json:"accepted_at"`
}

type AckRequest struct {
	Processed bool `json:"processed"`
}

type PollResponse struct {
	Messages []DeliveredMessage `json:"messages"`
}

// Space is one of the caller's space memberships. Grants are the caller's
// own; Members lists every active member by ID.
type Space struct {
	ID       string   `json:"id"`
	TenantID string   `json:"tenant_id"`
	Grants   []string `json:"grants"`
	Members  []string `json:"members"`
}

type SpacesResponse struct {
	Spaces []Space `json:"spaces"`
}

type DoorbellEvent struct {
	MessageID string `json:"message_id"`
	Sequence  int64  `json:"sequence"`
	Recipient string `json:"recipient_id"`
	SpaceID   string `json:"space_id,omitempty"`
}

type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ErrorResponse struct {
	Error APIError `json:"error"`
}

func KindAllowed(kind string) bool { return validKinds[kind] }
