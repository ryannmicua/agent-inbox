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
// deliberately absent: the server derives both.
type Envelope struct {
	Type              string            `json:"type"`
	ID                string            `json:"id"`
	TaskID            string            `json:"task_id"`
	ThreadID          string            `json:"thread_id"`
	ReplyTo           string            `json:"reply_to,omitempty"`
	SenderID          string            `json:"sender_id"`
	RecipientID       string            `json:"recipient_id"`
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

type DeliveredMessage struct {
	Envelope
	TenantID       string     `json:"tenant_id"`
	Sequence       int64      `json:"sequence"`
	AcceptedAt     time.Time  `json:"accepted_at"`
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
}

type AckRequest struct {
	Processed   bool   `json:"processed"`
	ProcessedAt string `json:"processed_at"`
}

type PollResponse struct {
	Messages []DeliveredMessage `json:"messages"`
}

type DoorbellEvent struct {
	MessageID string `json:"message_id"`
	Sequence  int64  `json:"sequence"`
	Recipient string `json:"recipient_id"`
}

type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ErrorResponse struct {
	Error APIError `json:"error"`
}

func KindAllowed(kind string) bool { return validKinds[kind] }
