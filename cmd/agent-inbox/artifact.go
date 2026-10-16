package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ryannmicua/agent-inbox/internal/inbox"
)

var (
	spaceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	sha256Pattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// transferClient moves bytes to and from the object store. Presigned URLs
// name one exact request, so redirects are never followed.
var transferClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// runArtifact handles artifact put and artifact get: files shared through a
// space, whose bytes go directly between this machine and the inbox's object
// store under short-lived grants.
func runArtifact(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || (args[0] != "put" && args[0] != "get") {
		return errors.New("artifact needs a subcommand: put or get (run agent-inbox help)")
	}
	command := args[0]
	flags := flag.NewFlagSet("artifact "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	server := flags.String("server", envOr("INBOX_SERVER", "https://inbox.example.com"), "inbox server URL")
	agent := flags.String("agent", os.Getenv("INBOX_AGENT"), "registered agent id")
	keyPath := flags.String("key", os.Getenv("INBOX_KEY"), "base64 private key file")
	keyID := flags.String("key-id", envOr("INBOX_KEY_ID", "primary"), "registered signing key id")
	space := flags.String("space", "", "space that holds the artifact")
	file := flags.String("file", "", "put: local file to upload")
	mediaType := flags.String("media-type", "", "put: media type (default: from the file extension)")
	sha := flags.String("sha256", "", "get: artifact sha256")
	uri := flags.String("uri", "", "get: inbox-artifact://SPACE/SHA256 reference (instead of --space and --sha256)")
	out := flags.String("out", "", "get: new file to write the verified bytes to")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *agent == "" || *keyPath == "" {
		return errors.New("--agent and --key (or INBOX_AGENT and INBOX_KEY) are required")
	}
	if command == "get" && *uri != "" {
		if *space != "" || *sha != "" {
			return errors.New("use either --uri or --space and --sha256")
		}
		var ok bool
		if *space, *sha, ok = splitArtifactURI(*uri); !ok {
			return errors.New("--uri must be inbox-artifact://SPACE/SHA256")
		}
	}
	if !spaceIDPattern.MatchString(*space) {
		return errors.New("--space must be a space ID")
	}
	private, err := inbox.LoadPrivateKey(*keyPath)
	if err != nil {
		return fmt.Errorf("load signing key: %w", err)
	}
	client := &inbox.Client{Server: *server, Agent: *agent, KeyID: *keyID, Private: private}
	ctx := context.Background()
	if command == "put" {
		if *file == "" {
			return errors.New("artifact put requires --file")
		}
		return putArtifact(ctx, client, *space, *file, *mediaType, stdout)
	}
	if !sha256Pattern.MatchString(*sha) {
		return errors.New("artifact get requires --sha256 (64 lowercase hex characters) or --uri")
	}
	if *out == "" {
		return errors.New("artifact get requires --out")
	}
	return getArtifact(ctx, client, *space, *sha, *out, stdout)
}

func splitArtifactURI(uri string) (string, string, bool) {
	rest, found := strings.CutPrefix(uri, inbox.ArtifactURIScheme+"://")
	space, sha, slash := strings.Cut(rest, "/")
	return space, sha, found && slash && spaceIDPattern.MatchString(space) && sha256Pattern.MatchString(sha)
}

// artifactCall makes one signed artifact request and decodes its response.
func artifactCall(ctx context.Context, client *inbox.Client, path string, body any) (inbox.ArtifactResponse, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return inbox.ArtifactResponse{}, err
	}
	data, status, err := client.Do(ctx, http.MethodPost, path, encoded)
	if err != nil {
		return inbox.ArtifactResponse{}, err
	}
	if status < 200 || status >= 300 {
		return inbox.ArtifactResponse{}, inbox.PrintAPIError(data, status)
	}
	var response inbox.ArtifactResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return inbox.ArtifactResponse{}, fmt.Errorf("decode artifact response: %w", err)
	}
	return response, nil
}

type artifactPutOutput struct {
	Artifact inbox.ArtifactInfo `json:"artifact"`
	// ArtifactRef is the value for send --artifact.
	ArtifactRef string `json:"artifact_ref"`
}

func putArtifact(ctx context.Context, client *inbox.Client, space, path, mediaType string, stdout io.Writer) error {
	if mediaType == "" {
		mediaType = mediaTypeFor(path)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	digest := sha256.New()
	size, err := io.Copy(digest, file)
	if err != nil {
		return err
	}
	sha := hex.EncodeToString(digest.Sum(nil))
	base := "/v1/spaces/" + space + "/artifacts"
	granted, err := artifactCall(ctx, client, base, map[string]any{"sha256": sha, "size": size, "media_type": mediaType})
	if err != nil {
		return err
	}
	stored := granted.Artifact
	if granted.Upload != nil {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, granted.Upload.Method, granted.Upload.URL, io.LimitReader(file, size))
		if err != nil {
			return err
		}
		req.ContentLength = size
		if size == 0 {
			req.Body = http.NoBody
		}
		response, err := transferClient.Do(req)
		if err != nil {
			return fmt.Errorf("upload to the object store: %w", err)
		}
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return fmt.Errorf("object store refused the upload (HTTP %d): %s", response.StatusCode, strings.TrimSpace(string(detail)))
		}
		completed, err := artifactCall(ctx, client, base+"/"+sha+"/complete", struct{}{})
		if err != nil {
			return err
		}
		stored = completed.Artifact
	}
	return json.NewEncoder(stdout).Encode(artifactPutOutput{Artifact: stored, ArtifactRef: fmt.Sprintf("%s|%s|%s|%d", stored.URI, stored.MediaType, stored.SHA256, stored.Size)})
}

type artifactGetOutput struct {
	Artifact inbox.ArtifactInfo `json:"artifact"`
	Path     string             `json:"path"`
}

// getArtifact downloads under a fresh grant and writes the file only once its
// size and SHA-256 match the artifact. The bytes are still untrusted data.
func getArtifact(ctx context.Context, client *inbox.Client, space, sha, out string, stdout io.Writer) error {
	if _, err := os.Lstat(out); err == nil {
		return fmt.Errorf("%s already exists; artifact get writes only new files", out)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	granted, err := artifactCall(ctx, client, "/v1/spaces/"+space+"/artifacts/"+sha+"/download", struct{}{})
	if err != nil {
		return err
	}
	if granted.Download == nil || granted.Artifact.SHA256 != sha {
		return errors.New("server did not return a download grant for this artifact")
	}
	req, err := http.NewRequestWithContext(ctx, granted.Download.Method, granted.Download.URL, nil)
	if err != nil {
		return err
	}
	response, err := transferClient.Do(req)
	if err != nil {
		return fmt.Errorf("download from the object store: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("object store refused the download (HTTP %d)", response.StatusCode)
	}
	temp, err := os.CreateTemp(filepath.Dir(out), ".artifact-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	digest := sha256.New()
	size, err := io.Copy(io.MultiWriter(temp, digest), io.LimitReader(response.Body, granted.Artifact.Size+1))
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("download from the object store: %w", err)
	}
	if got := hex.EncodeToString(digest.Sum(nil)); size != granted.Artifact.Size || got != sha {
		return fmt.Errorf("downloaded bytes do not match the artifact (got %d bytes with sha256 %s); nothing was written", size, got)
	}
	if err := os.Link(temp.Name(), out); err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(artifactGetOutput{Artifact: granted.Artifact, Path: out})
}
