package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// runArtifactRef prints the value for send --artifact: the reference URI, media
// type, lowercase SHA-256, and byte size of a local file. The inbox never stores
// or fetches the bytes; the receiver checks the hash after its own download.
func runArtifactRef(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("artifact-ref", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("file", "", "local file to describe")
	uri := flags.String("uri", "", "where receivers can fetch the file")
	mediaType := flags.String("media-type", "", "media type (default: from the file extension)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *path == "" || *uri == "" {
		return errors.New("artifact-ref requires --file and --uri")
	}
	if err := checkArtifactURI(*uri); err != nil {
		return err
	}
	if strings.Contains(*mediaType, "|") {
		return errors.New("--media-type must not contain |")
	}
	file, err := os.Open(*path)
	if err != nil {
		return err
	}
	defer file.Close()
	digest := sha256.New()
	size, err := io.Copy(digest, file)
	if err != nil {
		return err
	}
	kind := *mediaType
	if kind == "" {
		kind = mediaTypeFor(*path)
	}
	_, err = fmt.Fprintf(stdout, "%s|%s|%s|%d\n", *uri, kind, hex.EncodeToString(digest.Sum(nil)), size)
	return err
}

// checkArtifactURI applies the server's reference rules early, plus the one the
// CLI's URI|type|sha256|size format needs.
func checkArtifactURI(raw string) error {
	if strings.Contains(raw, "|") {
		return errors.New("--uri must not contain |")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.User != nil || strings.EqualFold(u.Scheme, "data") || strings.EqualFold(u.Scheme, "javascript") || len(raw) > 2048 {
		return errors.New("--uri must be an absolute reference without credentials, at most 2048 characters")
	}
	return nil
}

func mediaTypeFor(path string) string {
	if guessed := mime.TypeByExtension(filepath.Ext(path)); guessed != "" {
		if parsed, _, err := mime.ParseMediaType(guessed); err == nil {
			return parsed
		}
	}
	return "application/octet-stream"
}
