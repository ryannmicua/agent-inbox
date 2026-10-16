package inbox

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

type canonicalEnvelope struct {
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
	Payload           any               `json:"payload"`
	Artifacts         []Artifact        `json:"artifacts"`
	Provenance        map[string]string `json:"provenance,omitempty"`
}

func EnvelopeSigningBytes(e Envelope) ([]byte, error) {
	var payload any
	if len(e.Payload) == 0 {
		payload = map[string]any{}
	} else {
		if err := validateJSONMembers(e.Payload); err != nil {
			return nil, fmt.Errorf("payload must contain unique object members: %w", err)
		}
		dec := json.NewDecoder(bytes.NewReader(e.Payload))
		dec.UseNumber()
		if err := dec.Decode(&payload); err != nil {
			return nil, fmt.Errorf("payload must be JSON: %w", err)
		}
		var extra any
		if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
			return nil, errors.New("payload must contain exactly one JSON value")
		}
	}
	artifacts := e.Artifacts
	if artifacts == nil {
		artifacts = []Artifact{}
	}
	canon := canonicalEnvelope{
		Type: e.Type, ID: e.ID, TaskID: e.TaskID, ThreadID: e.ThreadID,
		ReplyTo: e.ReplyTo, SenderID: e.SenderID, RecipientID: e.RecipientID, SpaceID: e.SpaceID,
		KeyID: e.KeyID, Kind: e.Kind, CreatedAt: e.CreatedAt,
		AssertedAuthority: e.AssertedAuthority, ContentIsData: e.ContentIsData,
		Payload: payload, Artifacts: artifacts, Provenance: e.Provenance,
	}
	encoded, err := json.Marshal(canon)
	if err != nil {
		return nil, err
	}
	return append([]byte("agent-inbox-envelope-v1\n"), encoded...), nil
}

func envelopeRetryBytes(e Envelope) ([]byte, error) {
	e.CreatedAt = ""
	e.KeyID = ""
	return EnvelopeSigningBytes(e)
}

func SignEnvelope(e *Envelope, private ed25519.PrivateKey) error {
	data, err := EnvelopeSigningBytes(*e)
	if err != nil {
		return err
	}
	e.Signature = encodeSignature(ed25519.Sign(private, data))
	return nil
}

func VerifyEnvelope(e Envelope, public ed25519.PublicKey) bool {
	sig, err := decodeSignature(e.Signature)
	if err != nil {
		return false
	}
	data, err := EnvelopeSigningBytes(e)
	if err != nil {
		return false
	}
	return ed25519.Verify(public, data, sig)
}

type canonicalNoteRevision struct {
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
}

// NoteRevisionSigningBytes returns the bytes a note revision's signature
// covers. Every member is always present, in this order, with the same string
// escaping as envelopes.
func NoteRevisionSigningBytes(n NoteRevision) ([]byte, error) {
	encoded, err := json.Marshal(canonicalNoteRevision{
		Type: n.Type, RevisionID: n.RevisionID, SpaceID: n.SpaceID, NoteID: n.NoteID,
		BaseRevision: n.BaseRevision, AuthorID: n.AuthorID, KeyID: n.KeyID, CreatedAt: n.CreatedAt,
		ContentIsData: n.ContentIsData, MediaType: n.MediaType, SHA256: n.SHA256, Body: n.Body,
	})
	if err != nil {
		return nil, err
	}
	return append([]byte("agent-inbox-note-v1\n"), encoded...), nil
}

// noteRetryBytes is what a retry of a revision must repeat exactly: everything
// signed except created_at and key_id.
func noteRetryBytes(n NoteRevision) ([]byte, error) {
	n.CreatedAt = ""
	n.KeyID = ""
	return NoteRevisionSigningBytes(n)
}

func SignNoteRevision(n *NoteRevision, private ed25519.PrivateKey) error {
	data, err := NoteRevisionSigningBytes(*n)
	if err != nil {
		return err
	}
	n.Signature = encodeSignature(ed25519.Sign(private, data))
	return nil
}

func VerifyNoteRevision(n NoteRevision, public ed25519.PublicKey) bool {
	sig, err := decodeSignature(n.Signature)
	if err != nil {
		return false
	}
	data, err := NoteRevisionSigningBytes(n)
	if err != nil {
		return false
	}
	return ed25519.Verify(public, data, sig)
}

// NoteBodySHA256 is the lowercase hex SHA-256 of a note body's UTF-8 bytes.
func NoteBodySHA256(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func RequestSigningBytes(agentID, keyID, method, requestURI, timestamp, nonce string, body []byte) []byte {
	digest := sha256.Sum256(body)
	return []byte(fmt.Sprintf("agent-inbox-request-v1\n%s\n%s\n%s\n%s\n%s\n%s\n%s",
		agentID, keyID, method, requestURI, timestamp, nonce, hex.EncodeToString(digest[:])))
}

func encodeSignature(sig []byte) string { return base64.StdEncoding.EncodeToString(sig) }

func decodeSignature(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
