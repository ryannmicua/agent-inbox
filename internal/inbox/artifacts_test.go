package inbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// newArtifactSystem gives the release-notes space an in-memory object store.
// agent-a holds every grant, agent-b read, post, and artifact.get, and
// agent-d read and post only. agent-a is also the only member of
// other-notes.
func newArtifactSystem(t *testing.T) (*testSystem, *MemoryObjectStore) {
	t.Helper()
	s := newTestSystem(t)
	s.addAgent(t, "agent-d", "tenant-one")
	s.setRegistry(t, func(r *Registry) {
		r.Version = 2
		r.Spaces = []RegistrySpace{
			groupSpace(
				RegistrySpaceMember{Agent: "agent-a", Grants: []string{"read", "post", "artifact.put", "artifact.get"}},
				RegistrySpaceMember{Agent: "agent-b", Grants: []string{"read", "post", "artifact.get"}},
				RegistrySpaceMember{Agent: "agent-d", Grants: []string{"read", "post"}},
			),
			{ID: "other-notes", TenantID: "tenant-one", Members: []RegistrySpaceMember{{Agent: "agent-a", Grants: []string{"read", "post", "artifact.put", "artifact.get"}}}},
		}
	})
	objects := s.attachMemoryObjects(t)
	return s, objects
}

func (s *testSystem) attachMemoryObjects(t *testing.T) *MemoryObjectStore {
	t.Helper()
	objects := NewMemoryObjectStore()
	server := httptest.NewServer(objects)
	t.Cleanup(server.Close)
	objects.BaseURL = server.URL
	s.service.config.Objects = objects
	return objects
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// artifactCall posts body to an artifact route and decodes either response.
func (s *testSystem) artifactCall(t *testing.T, clientID, path string, body any) (int, ArtifactResponse, string) {
	t.Helper()
	var encoded []byte
	if body != nil {
		encoded = mustJSON(t, body)
	}
	data, status, err := s.clients[clientID].Do(context.Background(), http.MethodPost, path, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if status >= 200 && status < 300 {
		var response ArtifactResponse
		if err := json.Unmarshal(data, &response); err != nil {
			t.Fatalf("HTTP %d returned invalid JSON: %s", status, data)
		}
		return status, response, ""
	}
	var apiErr ErrorResponse
	if err := json.Unmarshal(data, &apiErr); err != nil {
		t.Fatalf("HTTP %d returned invalid error JSON: %s", status, data)
	}
	return status, ArtifactResponse{}, apiErr.Error.Code
}

func uploadBody(data []byte, mediaType string) map[string]any {
	return map[string]any{"sha256": sha256Hex(data), "size": len(data), "media_type": mediaType}
}

func artifactPath(space string, parts ...string) string {
	return "/v1/spaces/" + space + "/artifacts" + strings.Join(append([]string{""}, parts...), "/")
}

func transfer(t *testing.T, method, rawURL string, data []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if data == nil {
		req.Body = nil
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, body
}

// storeArtifact uploads and completes data in space as agent-a.
func (s *testSystem) storeArtifact(t *testing.T, space string, data []byte) ArtifactInfo {
	t.Helper()
	status, grant, code := s.artifactCall(t, "agent-a", artifactPath(space), uploadBody(data, "text/markdown"))
	if status != http.StatusCreated || grant.Upload == nil {
		t.Fatalf("upload grant = %d %s %+v", status, code, grant)
	}
	if status, body := transfer(t, grant.Upload.Method, grant.Upload.URL, data); status != http.StatusOK {
		t.Fatalf("upload PUT = %d %s", status, body)
	}
	status, stored, code := s.artifactCall(t, "agent-a", artifactPath(space, sha256Hex(data), "complete"), nil)
	if status != http.StatusOK || stored.Artifact.State != ArtifactAvailable {
		t.Fatalf("complete = %d %s %+v", status, code, stored)
	}
	return stored.Artifact
}

// lastAudit returns the newest audit row for action.
func (s *testSystem) lastAudit(t *testing.T, action string) AuditEntry {
	t.Helper()
	rows := s.auditActions(t, action)
	if len(rows) == 0 {
		t.Fatalf("no %s audit row", action)
	}
	return rows[len(rows)-1]
}

func requireAudit(t *testing.T, entry AuditEntry, actor, subject, outcome, code string) {
	t.Helper()
	if entry.ActorID != actor || entry.SubjectID != subject || entry.Outcome != outcome || entry.Code != code || entry.TenantID != "tenant-one" {
		t.Fatalf("%s audit = %+v, want actor %s subject %s outcome %s code %q", entry.Action, entry, actor, subject, outcome, code)
	}
}

func TestUploadGrantRequiresArtifactPut(t *testing.T) {
	s, _ := newArtifactSystem(t)
	data := []byte("draft release notes\n")
	sha := sha256Hex(data)

	status, _, code := s.artifactCall(t, "agent-b", artifactPath("release-notes"), uploadBody(data, "text/markdown"))
	if status != http.StatusForbidden || code != "artifact_put_not_allowed" {
		t.Fatalf("member without artifact.put = %d %s", status, code)
	}
	// Grants are checked before the body is read, so the subject is the space.
	requireAudit(t, s.lastAudit(t, "artifact.upload"), "agent-b", "release-notes", "rejected", "artifact_put_not_granted")
	for _, c := range []struct{ client, space string }{
		{"agent-c", "release-notes"}, // another tenant
		{"agent-b", "other-notes"},   // not a member
		{"agent-a", "no-such-space"},
	} {
		if status, _, code := s.artifactCall(t, c.client, artifactPath(c.space), uploadBody(data, "text/markdown")); status != http.StatusNotFound || code != "space_not_found" {
			t.Fatalf("%s uploading to %s = %d %s, want 404 space_not_found", c.client, c.space, status, code)
		}
	}
	if _, err := s.store.GetArtifact(context.Background(), "tenant-one", "release-notes", sha); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("a refused grant recorded an artifact: %v", err)
	}
	// Completing an upload is part of putting it.
	if status, _, code := s.artifactCall(t, "agent-b", artifactPath("release-notes", sha, "complete"), nil); status != http.StatusForbidden || code != "artifact_put_not_allowed" {
		t.Fatalf("complete without artifact.put = %d %s", status, code)
	}

	for _, body := range []map[string]any{
		{"sha256": strings.ToUpper(sha), "size": len(data), "media_type": "text/plain"},
		{"sha256": sha, "size": -1, "media_type": "text/plain"},
		{"sha256": sha, "size": DefaultMaxArtifactBytes + 1, "media_type": "text/plain"},
		{"sha256": sha, "media_type": "text/plain"},
		{"sha256": sha, "size": len(data), "media_type": "not a type"},
		{"sha256": sha, "size": len(data), "media_type": "text/plain", "uri": "https://example.com"},
	} {
		if status, _, code := s.artifactCall(t, "agent-a", artifactPath("release-notes"), body); status != http.StatusBadRequest {
			t.Fatalf("upload body %v = %d %s, want 400", body, status, code)
		}
	}

	status, grant, code := s.artifactCall(t, "agent-a", artifactPath("release-notes"), uploadBody(data, "text/markdown"))
	if status != http.StatusCreated || grant.Upload == nil || grant.Upload.Method != http.MethodPut {
		t.Fatalf("upload grant = %d %s %+v", status, code, grant)
	}
	want := ArtifactInfo{URI: "inbox-artifact://release-notes/" + sha, SpaceID: "release-notes", TenantID: "tenant-one", SHA256: sha, Size: int64(len(data)), MediaType: "text/markdown", State: ArtifactPending, UploadedBy: "agent-a"}
	got := grant.Artifact
	got.CreatedAt = time.Time{}
	if got != want {
		t.Fatalf("granted artifact = %+v, want %+v", got, want)
	}
	requireAudit(t, s.lastAudit(t, "artifact.upload"), "agent-a", "release-notes/"+sha, "granted", "")
}

func TestCompleteRehashesAndRejectsMismatch(t *testing.T) {
	s, objects := newArtifactSystem(t)
	data := []byte("the real draft\n")
	sha := sha256Hex(data)
	key := ArtifactObjectKey("tenant-one", "release-notes", sha)
	complete := artifactPath("release-notes", sha, "complete")

	if status, _, code := s.artifactCall(t, "agent-a", complete, nil); status != http.StatusNotFound || code != "artifact_not_found" {
		t.Fatalf("complete without a grant = %d %s", status, code)
	}
	_, grant, _ := s.artifactCall(t, "agent-a", artifactPath("release-notes"), uploadBody(data, "text/plain"))
	if status, _, code := s.artifactCall(t, "agent-a", complete, nil); status != http.StatusConflict || code != "artifact_not_uploaded" {
		t.Fatalf("complete before the upload = %d %s", status, code)
	}

	// The presigned PUT accepts only the declared size, so a mismatch of the
	// same length is the case a client can actually produce.
	forged := bytes.Repeat([]byte("x"), len(data))
	if status, body := transfer(t, http.MethodPut, grant.Upload.URL, append(forged, 'y')); status == http.StatusOK {
		t.Fatalf("store accepted a body longer than the signed size: %s", body)
	}
	if status, body := transfer(t, http.MethodPut, grant.Upload.URL, forged); status != http.StatusOK {
		t.Fatalf("upload PUT = %d %s", status, body)
	}
	if status, _, code := s.artifactCall(t, "agent-a", complete, map[string]any{}); status != http.StatusUnprocessableEntity || code != "artifact_mismatch" {
		t.Fatalf("complete with mismatching bytes = %d %s", status, code)
	}
	if _, ok := objects.Object(key); ok {
		t.Fatal("the mismatching object was not deleted")
	}
	if _, err := s.store.GetArtifact(context.Background(), "tenant-one", "release-notes", sha); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("the mismatching upload is still recorded: %v", err)
	}
	rejected := s.lastAudit(t, "artifact.complete")
	requireAudit(t, rejected, "agent-a", "release-notes/"+sha, "rejected", "hash_mismatch")
	if rejected.Detail["computed_sha256"] != sha256Hex(forged) {
		t.Fatalf("hash mismatch audit lacks the computed hash: %+v", rejected.Detail)
	}

	// An object planted with another size is caught before it is read.
	s.artifactCall(t, "agent-a", artifactPath("release-notes"), uploadBody(data, "text/plain"))
	objects.Put(key, append(bytes.Clone(data), '!'))
	if status, _, code := s.artifactCall(t, "agent-a", complete, nil); status != http.StatusUnprocessableEntity || code != "artifact_mismatch" {
		t.Fatalf("complete with the wrong size = %d %s", status, code)
	}
	requireAudit(t, s.lastAudit(t, "artifact.complete"), "agent-a", "release-notes/"+sha, "rejected", "size_mismatch")
	if _, ok := objects.Object(key); ok {
		t.Fatal("the wrongly sized object was not deleted")
	}

	stored := s.storeArtifact(t, "release-notes", data)
	if stored.StoredAt == nil || stored.MediaType != "text/markdown" {
		t.Fatalf("stored artifact = %+v", stored)
	}
	requireAudit(t, s.lastAudit(t, "artifact.complete"), "agent-a", "release-notes/"+sha, "stored", "")
	if status, again, _ := s.artifactCall(t, "agent-a", complete, nil); status != http.StatusOK || again.Artifact.State != ArtifactAvailable {
		t.Fatalf("repeated complete = %d %+v", status, again)
	}
	requireAudit(t, s.lastAudit(t, "artifact.complete"), "agent-a", "release-notes/"+sha, "duplicate", "already_stored")
	// A stored artifact is never offered for upload again.
	status, regrant, _ := s.artifactCall(t, "agent-a", artifactPath("release-notes"), uploadBody(data, "text/plain"))
	if status != http.StatusOK || regrant.Upload != nil || regrant.Artifact.State != ArtifactAvailable || regrant.Artifact.MediaType != "text/markdown" {
		t.Fatalf("upload of a stored artifact = %d %+v", status, regrant)
	}
	requireAudit(t, s.lastAudit(t, "artifact.upload"), "agent-a", "release-notes/"+sha, "duplicate", "already_available")
	body := uploadBody(data, "text/plain")
	body["size"] = len(data) + 1
	if status, _, code := s.artifactCall(t, "agent-a", artifactPath("release-notes"), body); status != http.StatusConflict || code != "artifact_conflict" {
		t.Fatalf("upload of a stored hash with another size = %d %s", status, code)
	}
}

func TestDownloadGrantCheckedAtRequestTime(t *testing.T) {
	s, _ := newArtifactSystem(t)
	data := []byte("notes for every member\n")
	sha := sha256Hex(data)
	download := artifactPath("release-notes", sha, "download")

	_, grant, _ := s.artifactCall(t, "agent-a", artifactPath("release-notes"), uploadBody(data, "text/plain"))
	if status, _, code := s.artifactCall(t, "agent-b", download, nil); status != http.StatusNotFound || code != "artifact_not_found" {
		t.Fatalf("download of a pending artifact = %d %s", status, code)
	}
	transfer(t, http.MethodPut, grant.Upload.URL, data)
	s.artifactCall(t, "agent-a", artifactPath("release-notes", sha, "complete"), nil)

	status, granted, code := s.artifactCall(t, "agent-b", download, nil)
	if status != http.StatusOK || granted.Download == nil || granted.Download.Method != http.MethodGet || granted.Artifact.SHA256 != sha {
		t.Fatalf("download grant = %d %s %+v", status, code, granted)
	}
	if status, body := transfer(t, http.MethodGet, granted.Download.URL, nil); status != http.StatusOK || !bytes.Equal(body, data) {
		t.Fatalf("presigned GET = %d %q", status, body)
	}
	requireAudit(t, s.lastAudit(t, "artifact.download"), "agent-b", "release-notes/"+sha, "granted", "")
	if status, _, code := s.artifactCall(t, "agent-d", download, nil); status != http.StatusForbidden || code != "artifact_get_not_allowed" {
		t.Fatalf("member without artifact.get = %d %s", status, code)
	}
	if status, _, code := s.artifactCall(t, "agent-a", artifactPath("release-notes", strings.Repeat("0", 64), "download"), nil); status != http.StatusNotFound || code != "artifact_not_found" {
		t.Fatalf("download of an unknown artifact = %d %s", status, code)
	}

	// The registry is read at request time: narrowing the grant, then removing
	// the member, refuses the next grant with no restart.
	s.setMembers(t,
		RegistrySpaceMember{Agent: "agent-a", Grants: []string{"read", "post", "artifact.put", "artifact.get"}},
		RegistrySpaceMember{Agent: "agent-b", Grants: []string{"read"}},
	)
	if status, _, code := s.artifactCall(t, "agent-b", download, nil); status != http.StatusForbidden || code != "artifact_get_not_allowed" {
		t.Fatalf("download after artifact.get was removed = %d %s", status, code)
	}
	s.setMembers(t, RegistrySpaceMember{Agent: "agent-a", Grants: []string{"read", "artifact.get"}})
	if status, _, code := s.artifactCall(t, "agent-b", download, nil); status != http.StatusNotFound || code != "space_not_found" {
		t.Fatalf("download after removal from the space = %d %s", status, code)
	}
	requireAudit(t, s.lastAudit(t, "artifact.download"), "agent-b", "release-notes/"+sha, "rejected", "not_member")
	// A grant issued before the removal lives out its short TTL.
	if status, _ := transfer(t, http.MethodGet, granted.Download.URL, nil); status != http.StatusOK {
		t.Fatalf("an unexpired grant stopped working early: %d", status)
	}
	// A revoked agent cannot authenticate at all.
	s.setRegistry(t, func(r *Registry) { r.Agents[s.agentIndex("agent-a")].Disabled = true })
	data2, status2, err := s.clients["agent-a"].Do(context.Background(), http.MethodPost, download, nil)
	if err != nil || status2 != http.StatusUnauthorized {
		t.Fatalf("revoked agent download = %d %s (%v)", status2, data2, err)
	}
}

func TestGrantTTLsBounded(t *testing.T) {
	s, objects := newArtifactSystem(t)
	// Clients sign with the real clock, so the fixed server clock stays near it.
	now := time.Now().UTC().Truncate(time.Second)
	s.service.now = func() time.Time { return now }
	objects.Now = func() time.Time { return now }
	data := []byte("short-lived grants\n")
	sha := sha256Hex(data)

	_, grant, _ := s.artifactCall(t, "agent-a", artifactPath("release-notes"), uploadBody(data, "text/plain"))
	if !grant.Upload.ExpiresAt.Equal(now.Add(UploadGrantTTL)) || UploadGrantTTL > 10*time.Minute {
		t.Fatalf("upload grant expires at %s, want %s", grant.Upload.ExpiresAt, now.Add(UploadGrantTTL))
	}
	objects.Now = func() time.Time { return now.Add(UploadGrantTTL + time.Second) }
	if status, _ := transfer(t, http.MethodPut, grant.Upload.URL, data); status != http.StatusForbidden {
		t.Fatalf("expired upload URL = %d, want 403", status)
	}
	objects.Now = func() time.Time { return now.Add(UploadGrantTTL) }
	if status, body := transfer(t, http.MethodPut, grant.Upload.URL, data); status != http.StatusOK {
		t.Fatalf("upload URL at its expiry = %d %s", status, body)
	}
	s.artifactCall(t, "agent-a", artifactPath("release-notes", sha, "complete"), nil)
	_, granted, _ := s.artifactCall(t, "agent-b", artifactPath("release-notes", sha, "download"), nil)
	if !granted.Download.ExpiresAt.Equal(now.Add(DownloadGrantTTL)) || DownloadGrantTTL > 5*time.Minute {
		t.Fatalf("download grant expires at %s, want %s", granted.Download.ExpiresAt, now.Add(DownloadGrantTTL))
	}
	objects.Now = func() time.Time { return now.Add(DownloadGrantTTL + time.Second) }
	if status, _ := transfer(t, http.MethodGet, granted.Download.URL, nil); status != http.StatusForbidden {
		t.Fatalf("expired download URL = %d, want 403", status)
	}

	// The S3 store signs the same lifetimes into its URLs.
	s3, err := NewS3ObjectStore(S3Config{Endpoint: "https://objects.example.com:443", Bucket: "inbox-artifacts", Region: "garage", AccessKeyID: "GKexample", SecretAccessKey: "example-secret"})
	if err != nil {
		t.Fatal(err)
	}
	s.service.config.Objects = s3
	other := []byte("another file\n")
	_, grant, _ = s.artifactCall(t, "agent-a", artifactPath("release-notes"), uploadBody(other, "text/plain"))
	upload, err := url.Parse(grant.Upload.URL)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := "/inbox-artifacts/tenant-one/release-notes/" + sha256Hex(other)
	if upload.Host != "objects.example.com" || upload.Path != wantPath || upload.Query().Get("X-Amz-Expires") != "600" || upload.Query().Get("X-Amz-SignedHeaders") != "content-length;host" {
		t.Fatalf("S3 upload URL = %s", grant.Upload.URL)
	}
	_, granted, _ = s.artifactCall(t, "agent-b", artifactPath("release-notes", sha, "download"), nil)
	downloadURL, err := url.Parse(granted.Download.URL)
	if err != nil {
		t.Fatal(err)
	}
	if downloadURL.Query().Get("X-Amz-Expires") != "300" || downloadURL.Query().Get("X-Amz-SignedHeaders") != "host" {
		t.Fatalf("S3 download URL = %s", granted.Download.URL)
	}
	for _, ttl := range []time.Duration{0, maxPresignTTL + time.Second} {
		if _, err := s3.PresignGet("k", now, ttl); err == nil {
			t.Fatalf("presign accepted a lifetime of %s", ttl)
		}
	}
}

func TestReferenceMustBeAvailableInSpace(t *testing.T) {
	s, _ := newArtifactSystem(t)
	data := []byte("reference me\n")
	stored := s.storeArtifact(t, "release-notes", data)
	elsewhere := s.storeArtifact(t, "other-notes", []byte("only in other-notes\n"))
	pending := []byte("not finished\n")
	s.artifactCall(t, "agent-a", artifactPath("release-notes"), uploadBody(pending, "text/plain"))

	send := func(space, recipient string, artifacts ...Artifact) (int, string) {
		t.Helper()
		e := s.envelope(t, "agent-a", recipient, "instruction", `{"task":"read the file"}`)
		e.SpaceID = space
		e.Artifacts = artifacts
		if err := SignEnvelope(&e, s.agents["agent-a"].private); err != nil {
			t.Fatal(err)
		}
		_, status, apiErr := s.send(t, e, "agent-a")
		if apiErr != nil {
			return status, apiErr.Error.Code
		}
		return status, ""
	}
	ref := func(uri, sha string, size int64) Artifact {
		return Artifact{URI: uri, MediaType: "text/markdown", SHA256: sha, Size: size}
	}
	good := ref(stored.URI, stored.SHA256, stored.Size)
	if status, code := send("release-notes", "", good, ref("https://files.example.com/x", stored.SHA256, 1)); status != http.StatusCreated {
		t.Fatalf("send with an available reference = %d %s", status, code)
	}
	polled := s.poll(t, "agent-b")
	if len(polled.Messages) != 1 || len(polled.Messages[0].Artifacts) != 2 {
		t.Fatalf("agent-b polled %+v", polled.Messages)
	}
	if got := polled.Messages[0].Artifacts[0]; got != good {
		t.Fatalf("delivered reference = %+v, want %+v", got, good)
	}

	pendingSHA := sha256Hex(pending)
	unknownSHA := strings.Repeat("a", 64)
	for _, c := range []struct {
		name, space, recipient string
		artifact               Artifact
		status                 int
		code                   string
	}{
		{"pending", "release-notes", "", ref(ArtifactURI("release-notes", pendingSHA), pendingSHA, int64(len(pending))), http.StatusConflict, "artifact_not_available"},
		{"unknown", "release-notes", "", ref(ArtifactURI("release-notes", unknownSHA), unknownSHA, 4), http.StatusConflict, "artifact_not_available"},
		{"wrong size", "release-notes", "", ref(stored.URI, stored.SHA256, stored.Size+1), http.StatusConflict, "artifact_not_available"},
		{"another space", "release-notes", "", ref(elsewhere.URI, elsewhere.SHA256, elsewhere.Size), http.StatusBadRequest, "invalid_artifact_reference"},
		{"direct message", "", "agent-b", good, http.StatusBadRequest, "invalid_artifact_reference"},
		{"hash differs from uri", "release-notes", "", ref(stored.URI, unknownSHA, stored.Size), http.StatusBadRequest, "invalid_artifact_reference"},
		{"no authority", "release-notes", "", ref("inbox-artifact:release-notes/"+stored.SHA256, stored.SHA256, stored.Size), http.StatusBadRequest, "invalid_artifact_reference"},
		{"uppercase scheme", "release-notes", "", ref("INBOX-ARTIFACT://release-notes/"+stored.SHA256, stored.SHA256, stored.Size), http.StatusBadRequest, "invalid_artifact_reference"},
		{"extra path", "release-notes", "", ref(stored.URI+"/x", stored.SHA256, stored.Size), http.StatusBadRequest, "invalid_artifact_reference"},
	} {
		if status, code := send(c.space, c.recipient, c.artifact); status != c.status || code != c.code {
			t.Fatalf("%s: send = %d %s, want %d %s", c.name, status, code, c.status, c.code)
		}
	}
	// A space ID may contain ':', which a URL host cannot.
	if space, sha, isArtifact, ok := parseArtifactURI("inbox-artifact://team:notes/" + unknownSHA); !isArtifact || !ok || space != "team:notes" || sha != unknownSHA {
		t.Fatalf("parse of a space with ':' = %q %q %v %v", space, sha, isArtifact, ok)
	}
}

func TestArtifactsDisabledWithoutStore(t *testing.T) {
	s := newGroupSystem(t)
	s.setMembers(t, RegistrySpaceMember{Agent: "agent-a", Grants: []string{"read", "post", "artifact.put", "artifact.get"}}, RegistrySpaceMember{Agent: "agent-b", Grants: []string{"read"}})
	data := []byte("no store here\n")
	sha := sha256Hex(data)
	for _, path := range []string{artifactPath("release-notes"), artifactPath("release-notes", sha, "complete"), artifactPath("release-notes", sha, "download")} {
		var body any
		if strings.HasSuffix(path, "/artifacts") {
			body = uploadBody(data, "text/plain")
		}
		if status, _, code := s.artifactCall(t, "agent-a", path, body); status != http.StatusServiceUnavailable || code != "artifacts_disabled" {
			t.Fatalf("POST %s without a store = %d %s", path, status, code)
		}
	}
	requireAudit(t, s.lastAudit(t, "artifact.download"), "agent-a", "release-notes/"+sha, "rejected", "artifacts_disabled")

	e := s.spaceEnvelope(t, "agent-a", "release-notes", "instruction", `{"task":"read"}`)
	e.Artifacts = []Artifact{{URI: ArtifactURI("release-notes", sha), MediaType: "text/plain", SHA256: sha, Size: int64(len(data))}}
	if err := SignEnvelope(&e, s.agents["agent-a"].private); err != nil {
		t.Fatal(err)
	}
	if _, status, apiErr := s.send(t, e, "agent-a"); status != http.StatusServiceUnavailable || apiErr == nil || apiErr.Error.Code != "artifacts_disabled" {
		t.Fatalf("send with an inbox-artifact reference and no store = %d %+v", status, apiErr)
	}
	// External references keep working as plain metadata.
	e = s.spaceEnvelope(t, "agent-a", "release-notes", "instruction", `{"task":"read"}`)
	e.Artifacts = []Artifact{{URI: "https://files.example.com/notes.md", MediaType: "text/plain", SHA256: sha, Size: int64(len(data))}}
	if err := SignEnvelope(&e, s.agents["agent-a"].private); err != nil {
		t.Fatal(err)
	}
	s.mustSend(t, e, "agent-a")

	if status, _, code := s.artifactCall(t, "agent-a", artifactPath("release-notes", sha, "remove"), nil); status != http.StatusNotFound || code != "not_found" {
		t.Fatalf("unknown artifact route = %d %s", status, code)
	}
	data2, status, err := s.clients["agent-a"].Do(context.Background(), http.MethodGet, artifactPath("release-notes", sha, "download"), nil)
	if err != nil || status != http.StatusNotFound {
		t.Fatalf("GET on an artifact route = %d %s (%v)", status, data2, err)
	}
}

// TestS3ObjectStoreAgainstEndpoint runs the artifact exchange against a real
// S3-compatible service, such as a local Garage, when INBOX_TEST_S3_URL is
// set: http://ACCESS_KEY_ID:SECRET@127.0.0.1:3900/BUCKET?region=garage. The
// bucket must exist and the key must be able to read and write it.
func TestS3ObjectStoreAgainstEndpoint(t *testing.T) {
	raw := os.Getenv("INBOX_TEST_S3_URL")
	if raw == "" {
		t.Skip("set INBOX_TEST_S3_URL to run against an S3-compatible endpoint")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		t.Fatalf("INBOX_TEST_S3_URL must be http(s)://KEY:SECRET@HOST/BUCKET?region=REGION: %v", err)
	}
	secret, _ := parsed.User.Password()
	region := parsed.Query().Get("region")
	if region == "" {
		region = "garage"
	}
	store, err := NewS3ObjectStore(S3Config{
		Endpoint: parsed.Scheme + "://" + parsed.Host, Bucket: strings.Trim(parsed.Path, "/"), Region: region,
		AccessKeyID: parsed.User.Username(), SecretAccessKey: secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id, _ := NewUUID()
	key := "test/" + id + "/team:notes"
	data := []byte(fmt.Sprintf("object store round trip %s\n", id))
	putURL, err := store.PresignPut(key, int64(len(data)), time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if status, body := transfer(t, http.MethodPut, putURL, append(bytes.Clone(data), 'x')); status == http.StatusOK {
		t.Fatalf("store accepted a body longer than the signed size: %s", body)
	}
	if status, body := transfer(t, http.MethodPut, putURL, data); status != http.StatusOK {
		t.Fatalf("presigned PUT = %d %s", status, body)
	}
	if size, err := store.Stat(ctx, key); err != nil || size != int64(len(data)) {
		t.Fatalf("Stat = %d %v", size, err)
	}
	body, err := store.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	read, _ := io.ReadAll(body)
	body.Close()
	if !bytes.Equal(read, data) {
		t.Fatalf("Open read %q", read)
	}
	getURL, _ := store.PresignGet(key, time.Now(), time.Minute)
	if status, got := transfer(t, http.MethodGet, getURL, nil); status != http.StatusOK || !bytes.Equal(got, data) {
		t.Fatalf("presigned GET = %d %q", status, got)
	}
	if status, _ := transfer(t, http.MethodGet, getURL+"x", nil); status == http.StatusOK {
		t.Fatal("store accepted an altered signature")
	}
	if err := store.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Stat(ctx, key); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("Stat after Delete = %v", err)
	}
	if err := store.Delete(ctx, key); err != nil {
		t.Fatalf("deleting a missing object = %v", err)
	}

	// The whole exchange, with the inbox verifying against the real store.
	s, _ := newArtifactSystem(t)
	s.service.config.Objects = store
	artifact := []byte(fmt.Sprintf("release notes %s\n", id))
	stored := s.storeArtifact(t, "release-notes", artifact)
	_, granted, _ := s.artifactCall(t, "agent-b", artifactPath("release-notes", stored.SHA256, "download"), nil)
	if status, got := transfer(t, http.MethodGet, granted.Download.URL, nil); status != http.StatusOK || !bytes.Equal(got, artifact) {
		t.Fatalf("download from the store = %d %q", status, got)
	}
	_, grant, _ := s.artifactCall(t, "agent-a", artifactPath("release-notes"), uploadBody([]byte("declared\n"), "text/plain"))
	transfer(t, http.MethodPut, grant.Upload.URL, []byte("forged!!\n"))
	if status, _, code := s.artifactCall(t, "agent-a", artifactPath("release-notes", sha256Hex([]byte("declared\n")), "complete"), nil); status != http.StatusUnprocessableEntity || code != "artifact_mismatch" {
		t.Fatalf("complete of forged bytes = %d %s", status, code)
	}
	if _, err := store.Stat(ctx, ArtifactObjectKey("tenant-one", "release-notes", sha256Hex([]byte("declared\n")))); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("forged object was not deleted: %v", err)
	}
	_ = store.Delete(ctx, ArtifactObjectKey("tenant-one", "release-notes", stored.SHA256))
}
