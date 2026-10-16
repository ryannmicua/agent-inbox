package inbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"strings"
	"time"
)

const (
	// UploadGrantTTL and DownloadGrantTTL bound how long a presigned URL works.
	// A grant already issued stays usable until it expires, so a member
	// removed from a space can finish at most one transfer it had started.
	UploadGrantTTL   = 10 * time.Minute
	DownloadGrantTTL = 5 * time.Minute
	// DefaultMaxArtifactBytes caps an artifact's declared size.
	DefaultMaxArtifactBytes = 100 << 20
	// ArtifactURIScheme names an artifact stored by this inbox, as
	// inbox-artifact://<space>/<sha256>.
	ArtifactURIScheme = "inbox-artifact"
)

// ArtifactObjectKey is where an artifact's bytes live in the object store.
// Keys are content-addressed per tenant and space, so an artifact is
// deduplicated within its space and never shared across spaces.
func ArtifactObjectKey(tenant, space, sha string) string {
	return tenant + "/" + space + "/" + sha
}

// ArtifactURI returns the reference a message carries for an artifact.
func ArtifactURI(space, sha string) string {
	return ArtifactURIScheme + "://" + space + "/" + sha
}

// parseArtifactURI splits an inbox-artifact reference. isArtifact reports
// whether uri uses the scheme at all; ok whether it is well formed.
func parseArtifactURI(uri string) (space, sha string, isArtifact, ok bool) {
	prefix := ArtifactURIScheme + "://"
	if len(uri) < len(prefix) || !strings.EqualFold(uri[:len(prefix)], prefix) {
		return "", "", strings.HasPrefix(strings.ToLower(uri), ArtifactURIScheme+":"), false
	}
	space, sha, found := strings.Cut(uri[len(prefix):], "/")
	if !strings.HasPrefix(uri, prefix) || !found || !registryIDPattern.MatchString(space) || !shaPattern.MatchString(sha) {
		return "", "", true, false
	}
	return space, sha, true, true
}

// artifactRoute parses /v1/spaces/{space}/artifacts[/{sha256}/{complete|download}].
func artifactRoute(path string) (space, sha, op string, ok bool) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 4 || parts[0] != "v1" || parts[1] != "spaces" || parts[3] != "artifacts" {
		return "", "", "", false
	}
	switch {
	case len(parts) == 4:
		return parts[2], "", "upload", true
	case len(parts) == 6 && (parts[5] == "complete" || parts[5] == "download"):
		return parts[2], parts[4], parts[5], true
	}
	return "", "", "", false
}

func isArtifactRoute(path string) bool {
	_, _, _, ok := artifactRoute(path)
	return ok
}

// artifactRequest carries one authenticated artifact call through its checks.
type artifactRequest struct {
	auth  authContext
	space string
	sha   string
	op    string
}

func (a artifactRequest) action() string { return "artifact." + a.op }

func (a artifactRequest) subject() string {
	if a.sha == "" {
		return a.space
	}
	return a.space + "/" + a.sha
}

func (a artifactRequest) audit(outcome, code string, detail map[string]any) AuditRecord {
	return AuditRecord{Action: a.action(), ActorID: a.auth.agent.ID, SubjectID: a.subject(), TenantID: a.auth.agent.TenantID, Outcome: outcome, Code: code, Detail: detail}
}

type artifactUploadRequest struct {
	SHA256    string `json:"sha256"`
	Size      *int64 `json:"size"`
	MediaType string `json:"media_type"`
}

func (s *Server) artifacts(w http.ResponseWriter, r *http.Request, auth authContext) {
	space, sha, op, _ := artifactRoute(r.URL.Path)
	req := artifactRequest{auth: auth, space: space, op: op}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusNotFound, "not_found", "route not found")
		return
	}
	if !registryIDPattern.MatchString(space) {
		s.rejectArtifact(w, req, http.StatusNotFound, "space_not_found", "not_member", "space was not found")
		return
	}
	if op != "upload" {
		if !shaPattern.MatchString(sha) {
			s.rejectArtifact(w, req, http.StatusNotFound, "artifact_not_found", "invalid_sha256", "artifact was not found")
			return
		}
		req.sha = sha
	}
	if r.URL.RawQuery != "" {
		s.rejectArtifact(w, req, http.StatusBadRequest, "unsupported_query_parameter", "unsupported_query_parameter", "artifact routes accept no query parameters")
		return
	}
	if s.config.Objects == nil {
		s.rejectArtifact(w, req, http.StatusServiceUnavailable, "artifacts_disabled", "artifacts_disabled", "this inbox has no object store configured")
		return
	}
	needed, deniedCode := GrantArtifactPut, "artifact_put_not_allowed"
	if op == "download" {
		needed, deniedCode = GrantArtifactGet, "artifact_get_not_allowed"
	}
	grant, err := s.registry.Grant(space, auth.agent.ID)
	switch {
	case errors.Is(err, ErrSpaceNotFound) || errors.Is(err, ErrUnknownAgent) || (err == nil && grant.TenantID != auth.agent.TenantID):
		s.rejectArtifact(w, req, http.StatusNotFound, "space_not_found", "not_member", "space was not found")
		return
	case err != nil:
		s.rejectArtifact(w, req, http.StatusServiceUnavailable, "registry_unavailable", "registry_unavailable", "space grants could not be read")
		return
	case !grant.Has(needed):
		s.rejectArtifact(w, req, http.StatusForbidden, deniedCode, strings.ReplaceAll(needed, ".", "_")+"_not_granted", "caller does not hold "+needed+" in this space")
		return
	}
	switch op {
	case "upload":
		s.grantUpload(w, r, req)
	case "complete":
		s.completeUpload(w, r, req)
	case "download":
		s.grantDownload(w, r, req)
	}
}

func (s *Server) grantUpload(w http.ResponseWriter, r *http.Request, req artifactRequest) {
	var body artifactUploadRequest
	if err := decodeStrict(r.Body, &body); err != nil {
		s.rejectArtifact(w, req, http.StatusBadRequest, "invalid_json", "invalid_json", "request must be {sha256, size, media_type}")
		return
	}
	if !shaPattern.MatchString(body.SHA256) {
		s.rejectArtifact(w, req, http.StatusBadRequest, "invalid_artifact", "invalid_sha256", "sha256 must be 64 lowercase hex characters")
		return
	}
	req.sha = body.SHA256
	if body.Size == nil || *body.Size < 0 || *body.Size > s.config.MaxArtifactBytes {
		s.rejectArtifact(w, req, http.StatusBadRequest, "invalid_artifact_size", "invalid_size", "size must be between 0 and the inbox's artifact limit")
		return
	}
	if _, _, err := mime.ParseMediaType(body.MediaType); err != nil || len(body.MediaType) > 128 || strings.Contains(body.MediaType, "|") {
		s.rejectArtifact(w, req, http.StatusBadRequest, "invalid_artifact", "invalid_media_type", "media_type must be a valid media type of at most 128 characters")
		return
	}
	tenant := req.auth.agent.TenantID
	now := s.now().UTC()
	expires := now.Add(UploadGrantTTL)
	uploadURL, err := s.config.Objects.PresignPut(ArtifactObjectKey(tenant, req.space, req.sha), *body.Size, now, UploadGrantTTL)
	if err != nil {
		log.Printf("presign artifact upload %s: %v", req.subject(), err)
		s.rejectArtifact(w, req, http.StatusServiceUnavailable, "object_store_unavailable", "presign_failed", "upload could not be granted")
		return
	}
	record := ArtifactRecord{TenantID: tenant, SpaceID: req.space, SHA256: req.sha, Size: *body.Size, MediaType: body.MediaType, State: ArtifactPending, UploadedBy: req.auth.agent.ID, CreatedAt: now}
	stored, err := s.store.GrantArtifactUpload(r.Context(), record, req.audit("granted", "", map[string]any{"size": *body.Size, "media_type": body.MediaType, "expires_at": expires.Format(time.RFC3339)}))
	if err != nil {
		log.Printf("record artifact upload %s: %v", req.subject(), err)
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "upload could not be recorded")
		return
	}
	if stored.State == ArtifactAvailable {
		if stored.Size != *body.Size {
			s.rejectArtifact(w, req, http.StatusConflict, "artifact_conflict", "size_conflict", "an artifact with this sha256 is already stored with a different size")
			return
		}
		if err := s.store.AppendAudit(r.Context(), req.audit("duplicate", "already_available", nil)); err != nil {
			log.Printf("record duplicate artifact upload %s: %v", req.subject(), err)
		}
		writeJSON(w, http.StatusOK, ArtifactResponse{Artifact: artifactInfo(stored)})
		return
	}
	writeJSON(w, http.StatusCreated, ArtifactResponse{Artifact: artifactInfo(stored), Upload: &ArtifactTransfer{Method: http.MethodPut, URL: uploadURL, ExpiresAt: expires}})
}

// completeUpload re-reads the uploaded object from the inbox's own store and
// makes the artifact available only if its hash and size are the declared
// ones. A mismatching object is deleted along with its pending record.
func (s *Server) completeUpload(w http.ResponseWriter, r *http.Request, req artifactRequest) {
	if !emptyJSONBody(r.Body) {
		s.rejectArtifact(w, req, http.StatusBadRequest, "invalid_json", "invalid_json", "complete takes an empty body or {}")
		return
	}
	tenant := req.auth.agent.TenantID
	record, err := s.store.GetArtifact(r.Context(), tenant, req.space, req.sha)
	switch {
	case errors.Is(err, ErrArtifactNotFound):
		s.rejectArtifact(w, req, http.StatusNotFound, "artifact_not_found", "artifact_not_found", "artifact was not found")
		return
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "artifact could not be read")
		return
	case record.State == ArtifactAvailable:
		if err := s.store.AppendAudit(r.Context(), req.audit("duplicate", "already_stored", nil)); err != nil {
			log.Printf("record duplicate artifact completion %s: %v", req.subject(), err)
		}
		writeJSON(w, http.StatusOK, ArtifactResponse{Artifact: artifactInfo(record)})
		return
	}
	key := ArtifactObjectKey(tenant, req.space, req.sha)
	size, err := s.config.Objects.Stat(r.Context(), key)
	if errors.Is(err, ErrObjectNotFound) {
		s.rejectArtifact(w, req, http.StatusConflict, "artifact_not_uploaded", "object_missing", "no object has been uploaded for this artifact yet")
		return
	}
	if err != nil {
		log.Printf("stat artifact %s: %v", req.subject(), err)
		s.rejectArtifact(w, req, http.StatusServiceUnavailable, "object_store_unavailable", "object_store_unavailable", "artifact could not be checked")
		return
	}
	computed, read, mismatch, err := s.hashObject(r.Context(), key, record.Size, size)
	if err != nil {
		log.Printf("read artifact %s: %v", req.subject(), err)
		s.rejectArtifact(w, req, http.StatusServiceUnavailable, "object_store_unavailable", "object_store_unavailable", "artifact could not be checked")
		return
	}
	if mismatch == "" && computed != req.sha {
		mismatch = "hash_mismatch"
	}
	if mismatch != "" {
		if err := s.config.Objects.Delete(r.Context(), key); err != nil {
			log.Printf("delete mismatching artifact %s: %v", req.subject(), err)
			s.rejectArtifact(w, req, http.StatusServiceUnavailable, "object_store_unavailable", "object_store_unavailable", "mismatching artifact could not be deleted")
			return
		}
		detail := map[string]any{"declared_size": record.Size, "stored_size": max(size, read)}
		if computed != "" {
			detail["computed_sha256"] = computed
		}
		if err := s.store.DiscardArtifactUpload(r.Context(), tenant, req.space, req.sha, req.audit("rejected", mismatch, detail)); err != nil {
			log.Printf("discard mismatching artifact %s: %v", req.subject(), err)
		}
		writeError(w, http.StatusUnprocessableEntity, "artifact_mismatch", "uploaded bytes do not match the declared sha256 and size; the object was deleted")
		return
	}
	now := s.now().UTC()
	if _, err := s.store.MarkArtifactStored(r.Context(), tenant, req.space, req.sha, now, req.audit("stored", "", map[string]any{"size": record.Size, "media_type": record.MediaType})); err != nil {
		log.Printf("record stored artifact %s: %v", req.subject(), err)
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "artifact could not be recorded")
		return
	}
	stored, err := s.store.GetArtifact(r.Context(), tenant, req.space, req.sha)
	if err != nil || stored.State != ArtifactAvailable {
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "artifact could not be read back")
		return
	}
	writeJSON(w, http.StatusOK, ArtifactResponse{Artifact: artifactInfo(stored)})
}

// hashObject reads at most declared+1 bytes of key. mismatch names a size
// difference; otherwise computed is the object's SHA-256.
func (s *Server) hashObject(ctx context.Context, key string, declared, statSize int64) (computed string, read int64, mismatch string, err error) {
	if statSize != declared {
		return "", 0, "size_mismatch", nil
	}
	body, err := s.config.Objects.Open(ctx, key)
	if err != nil {
		return "", 0, "", err
	}
	defer body.Close()
	digest := sha256.New()
	read, err = io.Copy(digest, io.LimitReader(body, declared+1))
	if err != nil {
		return "", read, "", err
	}
	if read != declared {
		return "", read, "size_mismatch", nil
	}
	return hex.EncodeToString(digest.Sum(nil)), read, "", nil
}

func (s *Server) grantDownload(w http.ResponseWriter, r *http.Request, req artifactRequest) {
	if !emptyJSONBody(r.Body) {
		s.rejectArtifact(w, req, http.StatusBadRequest, "invalid_json", "invalid_json", "download takes an empty body or {}")
		return
	}
	tenant := req.auth.agent.TenantID
	record, err := s.store.GetArtifact(r.Context(), tenant, req.space, req.sha)
	switch {
	case errors.Is(err, ErrArtifactNotFound) || (err == nil && record.State != ArtifactAvailable):
		s.rejectArtifact(w, req, http.StatusNotFound, "artifact_not_found", "not_available", "artifact was not found")
		return
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "artifact could not be read")
		return
	}
	now := s.now().UTC()
	expires := now.Add(DownloadGrantTTL)
	downloadURL, err := s.config.Objects.PresignGet(ArtifactObjectKey(tenant, req.space, req.sha), now, DownloadGrantTTL)
	if err != nil {
		log.Printf("presign artifact download %s: %v", req.subject(), err)
		s.rejectArtifact(w, req, http.StatusServiceUnavailable, "object_store_unavailable", "presign_failed", "download could not be granted")
		return
	}
	// The grant is returned only once its audit row is recorded.
	if err := s.store.AppendAudit(r.Context(), req.audit("granted", "", map[string]any{"expires_at": expires.Format(time.RFC3339)})); err != nil {
		log.Printf("record artifact download %s: %v", req.subject(), err)
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "download could not be recorded")
		return
	}
	writeJSON(w, http.StatusOK, ArtifactResponse{Artifact: artifactInfo(record), Download: &ArtifactTransfer{Method: http.MethodGet, URL: downloadURL, ExpiresAt: expires}})
}

func (s *Server) rejectArtifact(w http.ResponseWriter, req artifactRequest, status int, code, auditCode, message string) {
	if err := s.store.AppendAudit(context.Background(), req.audit("rejected", auditCode, nil)); err != nil {
		log.Printf("record rejected %s by %s (%s): %v", req.action(), req.auth.agent.ID, auditCode, err)
	}
	writeError(w, status, code, message)
}

// artifactReferencesAvailable checks every inbox-artifact reference in a
// space message against the artifacts table: each must be available in the
// message's space with the referenced size. validateEnvelope has already
// checked the form and that the reference names the message's space.
func (s *Server) artifactReferencesAvailable(w http.ResponseWriter, r *http.Request, auth authContext, e Envelope) bool {
	for _, artifact := range e.Artifacts {
		space, sha, isArtifact, _ := parseArtifactURI(artifact.URI)
		if !isArtifact {
			continue
		}
		if s.config.Objects == nil {
			s.rejectSend(w, auth.agent, "artifacts_disabled", "this inbox has no object store configured", e.ID)
			return false
		}
		record, err := s.store.GetArtifact(r.Context(), auth.agent.TenantID, space, sha)
		if err != nil && !errors.Is(err, ErrArtifactNotFound) {
			log.Printf("check artifact reference %s/%s: %v", space, sha, err)
			s.rejectSend(w, auth.agent, "storage_unavailable", "artifact references could not be checked", e.ID)
			return false
		}
		if err != nil || record.State != ArtifactAvailable || record.Size != artifact.Size {
			s.rejectSend(w, auth.agent, "artifact_not_available", "inbox-artifact references must name an available artifact in this space with its stored size", e.ID)
			return false
		}
	}
	return true
}

// emptyJSONBody accepts an empty body or exactly {}.
func emptyJSONBody(body io.Reader) bool {
	data, err := io.ReadAll(body)
	if err != nil {
		return false
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return true
	}
	var empty struct{}
	return decodeStrictJSON(data, &empty) == nil
}

func artifactInfo(record ArtifactRecord) ArtifactInfo {
	return ArtifactInfo{
		URI: ArtifactURI(record.SpaceID, record.SHA256), SpaceID: record.SpaceID, TenantID: record.TenantID,
		SHA256: record.SHA256, Size: record.Size, MediaType: record.MediaType, State: record.State,
		UploadedBy: record.UploadedBy, CreatedAt: record.CreatedAt, StoredAt: record.StoredAt,
	}
}
