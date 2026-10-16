package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// notes serves /v1/spaces/{space}/notes/{note} and its /revisions history.
// Every route checks membership first, so a caller outside the space gets the
// same not_found as an unknown route, whatever the space holds.
func (s *Server) notes(w http.ResponseWriter, r *http.Request, auth authContext) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	history := len(parts) == 6 && parts[5] == "revisions"
	if (len(parts) != 5 && !history) || parts[0] != "v1" || parts[1] != "spaces" || parts[3] != "notes" {
		writeError(w, http.StatusNotFound, "not_found", "route not found")
		return
	}
	space, note := parts[2], parts[4]
	switch {
	case r.Method == http.MethodPut && !history:
		s.putNote(w, r, auth, space, note)
	case r.Method == http.MethodGet && !history:
		s.getNote(w, r, auth, space, note)
	case r.Method == http.MethodGet && history:
		s.noteLog(w, r, auth, space, note)
	default:
		writeError(w, http.StatusNotFound, "not_found", "route not found")
	}
}

func (s *Server) putNote(w http.ResponseWriter, r *http.Request, auth authContext, spaceID, noteID string) {
	reject := func(status int, code, auditCode, message string, detail map[string]any) {
		if err := s.store.AppendAudit(context.Background(), AuditRecord{Action: "note.revision", ActorID: auth.agent.ID, SubjectID: noteSubjectIfValid(spaceID, noteID), TenantID: auth.agent.TenantID, Outcome: "rejected", Code: auditCode, Detail: detail}); err != nil {
			log.Printf("record rejected note revision by %s (%s): %v", auth.agent.ID, auditCode, err)
		}
		writeError(w, status, code, message)
	}
	snapshot, err := s.registry.Snapshot()
	if err != nil {
		reject(http.StatusServiceUnavailable, "registry_unavailable", "registry_unavailable", "agent registry could not be loaded", nil)
		return
	}
	space, grant, member := spaceMember(snapshot, auth, spaceID)
	if !member {
		reject(http.StatusNotFound, "not_found", "not_member", "route not found", nil)
		return
	}
	if !grant.Has(GrantNoteWrite) {
		reject(http.StatusForbidden, "note_write_not_allowed", "note_write_not_granted", "writer does not hold note.write in this space", nil)
		return
	}
	if r.URL.RawQuery != "" {
		reject(http.StatusBadRequest, "unsupported_query_parameter", "unsupported_query_parameter", "a note write accepts no query parameters", nil)
		return
	}
	if !registryIDPattern.MatchString(noteID) {
		reject(http.StatusBadRequest, "invalid_note_id", "invalid_note_id", "note id must be 1 to 128 letters, digits, '.', '_', ':' or '-', starting with a letter or digit", nil)
		return
	}
	var n NoteRevision
	if err := decodeStrict(r.Body, &n); err != nil {
		reject(http.StatusBadRequest, "invalid_json", "invalid_json", "request must be a valid note revision", nil)
		return
	}
	if !VerifyNoteRevision(n, auth.key) {
		reject(http.StatusBadRequest, "invalid_note_signature", "invalid_note_signature", "note revision signature is invalid", nil)
		return
	}
	if err := validateNoteRevision(n, auth, spaceID, noteID); err != nil {
		status := http.StatusBadRequest
		switch err.code {
		case "note_too_large":
			status = http.StatusRequestEntityTooLarge
		case "secret_detected":
			status = http.StatusForbidden
		}
		reject(status, err.code, err.code, err.message, nil)
		return
	}

	revision := n.BaseRevision + 1
	recipients := spaceReaders(snapshot, space, auth.agent.ID)
	detail := map[string]any{"revision": revision, "base_revision": n.BaseRevision, "sha256": n.SHA256, "revision_id": n.RevisionID, "recipients": recipients, "registry_sha256": snapshot.SHA256}
	write := NoteWrite{Revision: n, TenantID: space.TenantID, Recipients: recipients,
		Audit: AuditRecord{Action: "note.revision", ActorID: auth.agent.ID, SubjectID: NoteSubject(spaceID, noteID, revision), TenantID: space.TenantID, Outcome: "accepted", Detail: detail}}
	if len(recipients) > 0 {
		notice, err := noteNotice(n, revision, s.now())
		if err != nil {
			reject(http.StatusServiceUnavailable, "storage_unavailable", "storage_unavailable", "note revision could not be persisted", nil)
			return
		}
		write.Notice = notice
		detail["notice_id"] = notice.ID
	}
	result, err := s.store.PutNoteRevision(r.Context(), write)
	var conflict *NoteConflictError
	switch {
	case errors.As(err, &conflict):
		if auditErr := s.store.AppendAudit(context.Background(), AuditRecord{Action: "note.revision", ActorID: auth.agent.ID, SubjectID: NoteSubject(spaceID, noteID, 0), TenantID: space.TenantID, Outcome: "rejected", Code: "revision_conflict",
			Detail: map[string]any{"base_revision": n.BaseRevision, "head_revision": conflict.HeadRevision, "sha256": n.SHA256, "revision_id": n.RevisionID}}); auditErr != nil {
			log.Printf("record note revision conflict by %s: %v", auth.agent.ID, auditErr)
		}
		writeJSON(w, http.StatusConflict, NoteConflictResponse{
			Error:        APIError{Code: "note_revision_conflict", Message: fmt.Sprintf("base_revision %d is not the head; the head is revision %d. Read it, merge, and write again with that base.", n.BaseRevision, conflict.HeadRevision)},
			HeadRevision: conflict.HeadRevision, HeadSHA256: conflict.HeadSHA256, HeadAuthor: conflict.HeadAuthor,
		})
		return
	case errors.Is(err, ErrNoteRevisionIDConflict):
		reject(http.StatusConflict, "revision_id_conflict", "revision_id_conflict", "revision id already exists with different signed content", nil)
		return
	case err != nil:
		log.Printf("persist note revision %s: %v", n.RevisionID, err)
		reject(http.StatusServiceUnavailable, "storage_unavailable", "storage_unavailable", "note revision could not be persisted", nil)
		return
	}
	if result.Notice != nil {
		for _, recipient := range recipients {
			s.ring(*result.Notice, recipient)
		}
	}
	status := http.StatusCreated
	if result.Duplicate {
		status = http.StatusOK
	}
	writeJSON(w, status, result.Revision)
}

// noteNotice is the server-authored notice of an accepted revision, addressed
// to the note's space. It is unsigned and has no sender.
func noteNotice(n NoteRevision, revision int64, now time.Time) (Envelope, error) {
	id, err := NewUUID()
	if err != nil {
		return Envelope{}, err
	}
	payload, err := json.Marshal(NoteNotice{SpaceID: n.SpaceID, NoteID: n.NoteID, Revision: revision, AuthorID: n.AuthorID})
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{Type: NoticeType, ID: id, SpaceID: n.SpaceID, Kind: NoticeKind, CreatedAt: now.UTC().Format(time.RFC3339Nano), ContentIsData: true, Payload: payload, Artifacts: []Artifact{}}, nil
}

func validateNoteRevision(n NoteRevision, auth authContext, spaceID, noteID string) *validationError {
	_, timeErr := time.Parse(time.RFC3339Nano, n.CreatedAt)
	switch {
	case n.Type != NoteRevisionType:
		return &validationError{"invalid_type", "type must be note_revision"}
	case !messageIDPattern.MatchString(n.RevisionID):
		return &validationError{"invalid_revision_id", "revision_id must be a new UUID v4, UUID v7, or ULID"}
	case n.SpaceID != spaceID || n.NoteID != noteID:
		return &validationError{"note_path_mismatch", "space_id and note_id must match the request path"}
	case n.BaseRevision < 0:
		return &validationError{"invalid_base_revision", "base_revision must be 0 to create the note, or the head revision"}
	case n.AuthorID != auth.agent.ID:
		return &validationError{"author_mismatch", "author_id must match the authenticated agent"}
	case n.KeyID != auth.keyID:
		return &validationError{"key_mismatch", "note key_id must match the authenticated signing key"}
	case timeErr != nil:
		return &validationError{"invalid_created_at", "created_at must be an RFC3339 timestamp"}
	case !n.ContentIsData:
		return &validationError{"content_marker_required", "content_is_data must be true; note content is data, not authority"}
	case !validMediaType(n.MediaType):
		return &validationError{"invalid_media_type", "media_type must be a type/subtype of at most 128 characters"}
	case len(n.Body) > MaxNoteBodyBytes:
		return &validationError{"note_too_large", "note body must be 48 KiB or smaller; reference larger documents as artifacts"}
	case strings.ContainsRune(n.Body, 0):
		return &validationError{"invalid_note_body", "note body must be text without NUL characters"}
	case n.SHA256 != NoteBodySHA256(n.Body):
		return &validationError{"note_hash_mismatch", "sha256 must be the lowercase hex SHA-256 of the body's UTF-8 bytes"}
	}
	canonical, _ := NoteRevisionSigningBytes(n)
	if containsSecret(string(canonical)) {
		return &validationError{"secret_detected", "note body or metadata resembles a secret and was rejected"}
	}
	return nil
}

func validMediaType(mediaType string) bool {
	slash := strings.IndexByte(mediaType, '/')
	return len(mediaType) <= 128 && slash > 0 && slash < len(mediaType)-1 && !strings.ContainsAny(mediaType, " \t\r\n")
}

func (s *Server) getNote(w http.ResponseWriter, r *http.Request, auth authContext, spaceID, noteID string) {
	var revision int64
	for key, values := range r.URL.Query() {
		if key != "revision" {
			writeError(w, http.StatusBadRequest, "unsupported_query_parameter", "a note read accepts only the revision parameter")
			return
		}
		parsed, err := strconv.ParseInt(values[0], 10, 64)
		if len(values) != 1 || err != nil || parsed < 1 {
			writeError(w, http.StatusBadRequest, "invalid_revision", "revision must be a positive integer")
			return
		}
		revision = parsed
	}
	space, ok := s.noteReader(w, auth, spaceID, noteID)
	if !ok {
		return
	}
	stored, err := s.store.GetNoteRevision(r.Context(), space.TenantID, spaceID, noteID, revision)
	switch {
	case errors.Is(err, ErrNoteNotFound):
		writeError(w, http.StatusNotFound, "note_not_found", "note or revision was not found")
		return
	case err != nil:
		log.Printf("read note %s: %v", NoteSubject(spaceID, noteID, revision), err)
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "note could not be read")
		return
	}
	stored.AuthorStatus = s.authorStatus(stored.AuthorID, stored.TenantID, map[string]string{})
	writeJSON(w, http.StatusOK, stored)
}

func (s *Server) noteLog(w http.ResponseWriter, r *http.Request, auth authContext, spaceID, noteID string) {
	limit := s.config.MaxPollLimit
	for key, values := range r.URL.Query() {
		if key != "limit" {
			writeError(w, http.StatusBadRequest, "unsupported_query_parameter", "note history accepts only the limit parameter")
			return
		}
		parsed, err := strconv.Atoi(values[0])
		if len(values) != 1 || err != nil || parsed < 1 || parsed > s.config.MaxPollLimit {
			writeError(w, http.StatusBadRequest, "invalid_limit", fmt.Sprintf("limit must be between 1 and %d", s.config.MaxPollLimit))
			return
		}
		limit = parsed
	}
	space, ok := s.noteReader(w, auth, spaceID, noteID)
	if !ok {
		return
	}
	revisions, err := s.store.ListNoteRevisions(r.Context(), space.TenantID, spaceID, noteID, limit)
	switch {
	case errors.Is(err, ErrNoteNotFound):
		writeError(w, http.StatusNotFound, "note_not_found", "note or revision was not found")
		return
	case err != nil:
		log.Printf("read note history %s: %v", NoteSubject(spaceID, noteID, 0), err)
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "note history could not be read")
		return
	}
	statuses := map[string]string{}
	response := NoteLogResponse{SpaceID: spaceID, NoteID: noteID, Revisions: make([]NoteLogEntry, 0, len(revisions))}
	for _, rev := range revisions {
		response.Revisions = append(response.Revisions, NoteLogEntry{
			Revision: rev.Revision, BaseRevision: rev.BaseRevision, RevisionID: rev.RevisionID, AuthorID: rev.AuthorID, KeyID: rev.KeyID,
			CreatedAt: rev.CreatedAt, MediaType: rev.MediaType, SHA256: rev.SHA256, AcceptedAt: rev.AcceptedAt,
			AuthorStatus: s.authorStatus(rev.AuthorID, rev.TenantID, statuses),
		})
	}
	writeJSON(w, http.StatusOK, response)
}

// noteReader checks that the caller may read notes in the space: an active
// member holding read. It answers the request itself when not.
func (s *Server) noteReader(w http.ResponseWriter, auth authContext, spaceID, noteID string) (RegistrySpace, bool) {
	reject := func(status int, code, auditCode, message string) {
		if err := s.store.AppendAudit(context.Background(), AuditRecord{Action: "note.read", ActorID: auth.agent.ID, SubjectID: noteSubjectIfValid(spaceID, noteID), TenantID: auth.agent.TenantID, Outcome: "rejected", Code: auditCode}); err != nil {
			log.Printf("record rejected note read by %s (%s): %v", auth.agent.ID, auditCode, err)
		}
		writeError(w, status, code, message)
	}
	snapshot, err := s.registry.Snapshot()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "registry_unavailable", "agent registry could not be loaded")
		return RegistrySpace{}, false
	}
	space, grant, member := spaceMember(snapshot, auth, spaceID)
	switch {
	case !member:
		reject(http.StatusNotFound, "not_found", "not_member", "route not found")
		return RegistrySpace{}, false
	case !grant.Has(GrantRead):
		reject(http.StatusForbidden, "read_not_allowed", "read_not_granted", "reader does not hold read in this space")
		return RegistrySpace{}, false
	case !registryIDPattern.MatchString(noteID):
		writeError(w, http.StatusBadRequest, "invalid_note_id", "note id must be 1 to 128 letters, digits, '.', '_', ':' or '-', starting with a letter or digit")
		return RegistrySpace{}, false
	}
	return space, true
}

// authorStatus is "revoked" when the author can no longer act in the tenant,
// computed from the registry at read time; statuses caches it per author.
func (s *Server) authorStatus(author, tenant string, statuses map[string]string) string {
	status, known := statuses[author]
	if !known {
		if _, revoked, err := s.revocation(author, tenant); err == nil && revoked {
			status = AuthorStatusRevoked
		}
		statuses[author] = status
	}
	return status
}

// noteSubjectIfValid keeps caller-chosen path segments out of the audit log
// unless they are well-formed IDs.
func noteSubjectIfValid(space, note string) string {
	if !registryIDPattern.MatchString(space) {
		return ""
	}
	if !registryIDPattern.MatchString(note) {
		return space
	}
	return NoteSubject(space, note, 0)
}
