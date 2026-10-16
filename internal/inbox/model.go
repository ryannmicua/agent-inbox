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

// Artifact is a reference to stored content. The inbox never accepts artifact
// bytes in a message: a reference names either an external location or, with
// the inbox-artifact scheme, a file stored through the inbox's object store.
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

const (
	NoteRevisionType = "note_revision"
	// MaxNoteBodyBytes keeps a revision request under MaxRequestBytes; larger
	// documents belong in an artifact referenced from a short note.
	MaxNoteBodyBytes = 48 * 1024
	// NoticeType and NoticeKind mark a server-authored delivery, such as a new
	// note revision. A notice is unsigned and has no sender.
	NoticeType = "notice"
	NoticeKind = "notice"
)

// NoteRevision is one signed, author-proposed version of a note. A revision
// with base_revision 0 creates the note; otherwise base_revision must be the
// note's head when the server accepts it.
type NoteRevision struct {
	Type          string `json:"type"`
	RevisionID    string `json:"revision_id"`
	SpaceID       string `json:"space_id"`
	NoteID        string `json:"note_id"`
	BaseRevision  int64  `json:"base_revision"`
	AuthorID      string `json:"author_id"`
	KeyID         string `json:"key_id"`
	CreatedAt     string `json:"created_at"`
	ContentIsData bool   `json:"content_is_data"`
	MediaType     string `json:"media_type"`
	SHA256        string `json:"sha256"`
	Body          string `json:"body"`
	Signature     string `json:"signature"`
}

// StoredNoteRevision is an accepted revision with its server-assigned number.
type StoredNoteRevision struct {
	NoteRevision
	TenantID   string    `json:"tenant_id"`
	Revision   int64     `json:"revision"`
	AcceptedAt time.Time `json:"accepted_at"`
	// AuthorStatus is computed from the registry at read time; it is empty
	// while the author is active.
	AuthorStatus string `json:"author_status,omitempty"`
}

// NoteLogEntry describes one revision in a note's history, without its body.
type NoteLogEntry struct {
	Revision     int64     `json:"revision"`
	BaseRevision int64     `json:"base_revision"`
	RevisionID   string    `json:"revision_id"`
	AuthorID     string    `json:"author_id"`
	KeyID        string    `json:"key_id"`
	CreatedAt    string    `json:"created_at"`
	MediaType    string    `json:"media_type"`
	SHA256       string    `json:"sha256"`
	AcceptedAt   time.Time `json:"accepted_at"`
	AuthorStatus string    `json:"author_status,omitempty"`
}

type NoteLogResponse struct {
	SpaceID   string         `json:"space_id"`
	NoteID    string         `json:"note_id"`
	Revisions []NoteLogEntry `json:"revisions"`
}

// NoteConflictResponse is the 409 answer to a write whose base is not the
// head. The head fields are empty when the note does not exist yet.
type NoteConflictResponse struct {
	Error        APIError `json:"error"`
	HeadRevision int64    `json:"head_revision"`
	HeadSHA256   string   `json:"head_sha256"`
	HeadAuthor   string   `json:"head_author"`
}

// NoteNotice is the payload of the notice each other reader of a space gets
// when a note revision is accepted.
type NoteNotice struct {
	SpaceID  string `json:"space_id"`
	NoteID   string `json:"note_id"`
	Revision int64  `json:"revision"`
	AuthorID string `json:"author_id"`
}

// ArtifactInfo describes one artifact stored in a space. URI is the
// inbox-artifact reference a message carries for it.
type ArtifactInfo struct {
	URI        string     `json:"uri"`
	SpaceID    string     `json:"space_id"`
	TenantID   string     `json:"tenant_id"`
	SHA256     string     `json:"sha256"`
	Size       int64      `json:"size"`
	MediaType  string     `json:"media_type"`
	State      string     `json:"state"`
	UploadedBy string     `json:"uploaded_by"`
	CreatedAt  time.Time  `json:"created_at"`
	StoredAt   *time.Time `json:"stored_at,omitempty"`
}

// ArtifactTransfer is a short-lived presigned request to the object store.
type ArtifactTransfer struct {
	Method    string    `json:"method"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type ArtifactResponse struct {
	Artifact ArtifactInfo      `json:"artifact"`
	Upload   *ArtifactTransfer `json:"upload,omitempty"`
	Download *ArtifactTransfer `json:"download,omitempty"`
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
