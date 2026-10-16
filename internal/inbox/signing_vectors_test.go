package inbox

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateVectors = flag.Bool("update-vectors", false, "rewrite testdata/signing-vectors.json from the cases in this file")

const signingVectorsPath = "testdata/signing-vectors.json"

// SigningVectors is the published test-vector file that lets non-Go clients
// check their request and envelope signing byte for byte.
type SigningVectors struct {
	Description    string           `json:"description"`
	Seed           string           `json:"seed_base64"`
	PrivateKeyFile string           `json:"private_key_file"`
	PublicKey      string           `json:"public_key_base64"`
	Envelopes      []EnvelopeVector `json:"envelopes"`
	Requests       []RequestVector  `json:"requests"`
	Notes          []NoteVector     `json:"notes"`
}

type EnvelopeVector struct {
	Name      string          `json:"name"`
	Note      string          `json:"note"`
	Envelope  json.RawMessage `json:"envelope"`
	Canonical string          `json:"canonical"`
	Signature string          `json:"signature"`
}

type NoteVector struct {
	Name      string          `json:"name"`
	Note      string          `json:"note"`
	Revision  json.RawMessage `json:"revision"`
	Canonical string          `json:"canonical"`
	Signature string          `json:"signature"`
}

type RequestVector struct {
	Name      string `json:"name"`
	AgentID   string `json:"agent_id"`
	KeyID     string `json:"key_id"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Timestamp string `json:"timestamp"`
	Nonce     string `json:"nonce"`
	Body      string `json:"body"`
	Canonical string `json:"canonical"`
	Signature string `json:"signature"`
}

// vectorSeed is a fixed, published test seed. Never use it for a real agent.
var vectorSeed = bytes.Repeat([]byte{0x42}, ed25519.SeedSize)

type envelopeCase struct {
	name, note string
	envelope   Envelope
}

func vectorEnvelopeCases() []envelopeCase {
	base := func(id, kind, payload string) Envelope {
		return Envelope{
			Type: EnvelopeType, ID: id, TaskID: "0192f0c4-7d2e-7a10-8c3b-5d6e7f809a1b", ThreadID: "0192f0c4-7d2e-7a10-8c3b-5d6e7f809a1b",
			SenderID: "vector-bot", RecipientID: "peer-bot", KeyID: "primary", Kind: kind,
			CreatedAt: "2026-10-15T12:00:00Z", ContentIsData: true, Payload: json.RawMessage(payload), Artifacts: []Artifact{},
		}
	}
	reply := base("0192f0c4-7d2e-7a10-8c3b-5d6e7f809a20", "result", `{"summary":"done","count":3}`)
	reply.ReplyTo = "0192f0c4-7d2e-7a10-8c3b-5d6e7f809a1b"
	reply.AssertedAuthority = "operator approved this run"
	reply.Provenance = map[string]string{"tool": "unit-test", "run": "42"}
	reply.Artifacts = []Artifact{
		{URI: "https://files.example.com/a.txt", MediaType: "text/plain", SHA256: "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", Size: 4},
		{URI: "inbox-artifact://space/2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae", MediaType: "application/octet-stream", SHA256: "2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae", Size: 3},
	}
	return []envelopeCase{
		{"minimal-instruction", "Empty object payload, no artifacts. Optional fields are omitted from the canonical form.",
			base("0192f0c4-7d2e-7a10-8c3b-5d6e7f809a1b", "instruction", `{}`)},
		{"html-escaping", "Go escapes <, > and & inside strings as \\u003c, \\u003e and \\u0026. Signing plain JSON text here fails.",
			base("0192f0c4-7d2e-7a10-8c3b-5d6e7f809a1c", "instruction", `{"note":"x < y & z > w"}`)},
		{"unicode", "Non-ASCII stays raw UTF-8; U+2028 and U+2029 are escaped.",
			base("0192f0c4-7d2e-7a10-8c3b-5d6e7f809a1d", "instruction", "{\"text\":\"café – naïve \u2028 next \u2029 end\"}")},
		{"nested-keys-and-numbers", "Object keys are sorted at every depth, array order is kept, and numbers keep their original text.",
			base("0192f0c4-7d2e-7a10-8c3b-5d6e7f809a1e", "instruction", `{ "zeta": 1, "alpha": {"b": [3, 1, 2], "a": true}, "mid": null, "price": 1.50, "big": 1e3 }`)},
		{"control-characters", "Quote, backslash, \\b \\f \\n \\r \\t use short escapes; other control characters use lowercase \\u00XX.",
			base("0192f0c4-7d2e-7a10-8c3b-5d6e7f809a1f", "instruction", `{"s":"tab\there \"q\" back\\slash \b\f\n\r \u0001 \u001f"}`)},
		{"result-with-optional-fields", "reply_to, asserted_authority and provenance appear only when set; provenance keys are sorted; artifacts keep their order.",
			reply},
	}
}

type noteCase struct {
	name, note string
	revision   NoteRevision
}

func vectorNoteCases() []noteCase {
	base := func(id string, baseRevision int64, mediaType, body string) NoteRevision {
		return NoteRevision{
			Type: NoteRevisionType, RevisionID: id, SpaceID: "vector-space", NoteID: "release-plan", BaseRevision: baseRevision,
			AuthorID: "vector-bot", KeyID: "primary", CreatedAt: "2026-10-15T12:00:00Z", ContentIsData: true,
			MediaType: mediaType, SHA256: NoteBodySHA256(body), Body: body,
		}
	}
	return []noteCase{
		{"note-create", "base_revision 0 creates the note. The body is one JSON string with Go's escaping: newlines as \\n, <, > and & as \\u003c, \\u003e and \\u0026, non-ASCII raw. sha256 hashes the body's raw UTF-8 bytes, not the escaped text.",
			base("0192f0c4-7d2e-7a10-8c3b-5d6e7f809b01", 0, "text/markdown", "# Release plan\n\n- ship <v1> & tag\n- café notes\n")},
		{"note-revision", "A later revision names the head it was written against. Every member is always present, even an empty body.",
			base("0192f0c4-7d2e-7a10-8c3b-5d6e7f809b02", 3, "text/plain", "")},
	}
}

func vectorRequestCases() []RequestVector {
	return []RequestVector{
		{Name: "poll", AgentID: "vector-bot", KeyID: "primary", Method: http.MethodGet, Path: "/v1/messages?limit=100", Timestamp: "1792065600", Nonce: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Body: ""},
		{Name: "ack", AgentID: "vector-bot", KeyID: "primary", Method: http.MethodPost, Path: "/v1/messages/0192f0c4-7d2e-7a10-8c3b-5d6e7f809a1b/ack", Timestamp: "1792065601", Nonce: "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", Body: `{"processed":true}`},
	}
}

func buildSigningVectors(t *testing.T) []byte {
	t.Helper()
	private := ed25519.NewKeyFromSeed(vectorSeed)
	public := private.Public().(ed25519.PublicKey)
	out := SigningVectors{
		Description: "agent-inbox signing test vectors. Rebuild with: go test ./internal/inbox -run TestSigningVectors -update-vectors. The key is a published test key; never register it.",
		Seed:        base64.StdEncoding.EncodeToString(vectorSeed),
		// The CLI key file holds the 64-byte Go private key (seed followed by public key) in base64.
		PrivateKeyFile: base64.StdEncoding.EncodeToString(private),
		PublicKey:      base64.StdEncoding.EncodeToString(public),
	}
	for _, c := range vectorEnvelopeCases() {
		e := c.envelope
		if err := SignEnvelope(&e, private); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		canonical, err := EnvelopeSigningBytes(e)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		wire := marshalNoHTMLEscape(t, e)
		out.Envelopes = append(out.Envelopes, EnvelopeVector{Name: c.name, Note: c.note, Envelope: wire, Canonical: string(canonical), Signature: e.Signature})
	}
	for _, r := range vectorRequestCases() {
		canonical := RequestSigningBytes(r.AgentID, r.KeyID, r.Method, r.Path, r.Timestamp, r.Nonce, []byte(r.Body))
		r.Canonical = string(canonical)
		r.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(private, canonical))
		out.Requests = append(out.Requests, r)
	}
	for _, c := range vectorNoteCases() {
		n := c.revision
		if err := SignNoteRevision(&n, private); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		canonical, err := NoteRevisionSigningBytes(n)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		out.Notes = append(out.Notes, NoteVector{Name: c.name, Note: c.note, Revision: marshalNoHTMLEscape(t, n), Canonical: string(canonical), Signature: n.Signature})
	}
	return marshalNoHTMLEscapeIndent(t, out)
}

func marshalNoHTMLEscape(t *testing.T, value any) json.RawMessage {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		t.Fatal(err)
	}
	return json.RawMessage(bytes.TrimSpace(buf.Bytes()))
}

func marshalNoHTMLEscapeIndent(t *testing.T, value any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSigningVectors(t *testing.T) {
	want := buildSigningVectors(t)
	if *updateVectors {
		if err := os.MkdirAll(filepath.Dir(signingVectorsPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(signingVectorsPath, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(signingVectorsPath)
	if err != nil {
		t.Fatalf("read published vectors (run with -update-vectors to create them): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is out of date with the signing code; review the change and rebuild with -update-vectors", signingVectorsPath)
	}

	// Check the published file on its own terms, the way another client would.
	var vectors SigningVectors
	if err := json.Unmarshal(got, &vectors); err != nil {
		t.Fatal(err)
	}
	public, _ := base64.StdEncoding.DecodeString(vectors.PublicKey)
	keyFile, _ := base64.StdEncoding.DecodeString(vectors.PrivateKeyFile)
	if len(keyFile) != ed25519.PrivateKeySize || !bytes.Equal(keyFile[32:], public) {
		t.Fatal("private_key_file must be seed followed by public key")
	}
	if len(vectors.Envelopes) < 6 || len(vectors.Requests) < 2 || len(vectors.Notes) < 2 {
		t.Fatalf("vector file is missing cases: %d envelopes, %d requests, %d notes", len(vectors.Envelopes), len(vectors.Requests), len(vectors.Notes))
	}
	for _, v := range vectors.Envelopes {
		var e Envelope
		if err := json.Unmarshal(v.Envelope, &e); err != nil {
			t.Fatalf("%s: %v", v.Name, err)
		}
		canonical, err := EnvelopeSigningBytes(e)
		if err != nil || string(canonical) != v.Canonical {
			t.Fatalf("%s: canonical bytes differ: %v\n got %q\nwant %q", v.Name, err, canonical, v.Canonical)
		}
		if e.Signature != v.Signature || !VerifyEnvelope(e, ed25519.PublicKey(public)) {
			t.Fatalf("%s: signature does not verify", v.Name)
		}
	}
	for _, r := range vectors.Requests {
		canonical := RequestSigningBytes(r.AgentID, r.KeyID, r.Method, r.Path, r.Timestamp, r.Nonce, []byte(r.Body))
		sig, _ := base64.StdEncoding.DecodeString(r.Signature)
		if string(canonical) != r.Canonical || !ed25519.Verify(ed25519.PublicKey(public), canonical, sig) {
			t.Fatalf("%s: request vector does not verify", r.Name)
		}
	}
	for _, v := range vectors.Notes {
		var n NoteRevision
		if err := json.Unmarshal(v.Revision, &n); err != nil {
			t.Fatalf("%s: %v", v.Name, err)
		}
		canonical, err := NoteRevisionSigningBytes(n)
		if err != nil || string(canonical) != v.Canonical {
			t.Fatalf("%s: canonical bytes differ: %v\n got %q\nwant %q", v.Name, err, canonical, v.Canonical)
		}
		if n.Signature != v.Signature || !VerifyNoteRevision(n, ed25519.PublicKey(public)) || n.SHA256 != NoteBodySHA256(n.Body) {
			t.Fatalf("%s: note vector does not verify", v.Name)
		}
	}
}

func TestSigningVectorsCoverEscapingTrap(t *testing.T) {
	for _, c := range vectorEnvelopeCases() {
		if c.name != "html-escaping" {
			continue
		}
		e := c.envelope
		canonical, err := EnvelopeSigningBytes(e)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(canonical, []byte("x \\u003c y \\u0026 z \\u003e w")) {
			t.Fatalf("canonical form does not escape HTML characters: %s", canonical)
		}
		return
	}
	t.Fatal("html-escaping vector is missing")
}

// TestNaiveJSONSignatureIsRejected documents the cross-runtime trap: a client
// that signs ordinary compact JSON without Go's HTML escaping produces a
// signature the server rejects as soon as a payload contains <, > or &.
func TestNaiveJSONSignatureIsRejected(t *testing.T) {
	s := newTestSystem(t)
	e := s.envelope(t, "agent-a", "agent-b", "instruction", `{"note":"x < y & z"}`)
	canonical, err := EnvelopeSigningBytes(e)
	if err != nil {
		t.Fatal(err)
	}
	naive := strings.NewReplacer("\\u003c", "<", "\\u003e", ">", "\\u0026", "&").Replace(string(canonical))
	if naive == string(canonical) {
		t.Fatal("test payload did not exercise HTML escaping")
	}
	e.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(s.agents["agent-a"].private, []byte(naive)))
	if _, status, apiErr := s.send(t, e, "agent-a"); apiErr == nil || status != http.StatusBadRequest || apiErr.Error.Code != "invalid_message_signature" {
		t.Fatalf("naive JSON signature was not rejected: HTTP %d %+v", status, apiErr)
	}
	if err := SignEnvelope(&e, s.agents["agent-a"].private); err != nil {
		t.Fatal(err)
	}
	if _, status, apiErr := s.send(t, e, "agent-a"); apiErr != nil || status != http.StatusCreated {
		t.Fatalf("correctly signed envelope was rejected: HTTP %d %+v", status, apiErr)
	}
}
