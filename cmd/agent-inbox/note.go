package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ryannmicua/agent-inbox/internal/inbox"
)

// runNote reads and writes a space's versioned notes:
//
//	note get --space S --note N [--revision R]
//	note put --space S --note N --base B --file F [--media-type T] [--id UUID]
//	note log --space S --note N [--limit L]
func runNote(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || (args[0] != "get" && args[0] != "put" && args[0] != "log") {
		return errors.New("note needs a subcommand: get, put, or log (run agent-inbox help)")
	}
	sub := args[0]
	flags := flag.NewFlagSet("note "+sub, flag.ContinueOnError)
	flags.SetOutput(stderr)
	newClient := clientFlags(flags)
	space := flags.String("space", "", "space id")
	note := flags.String("note", "", "note id")
	var revision, base *int64
	var file, mediaType, id *string
	var limit *int
	switch sub {
	case "get":
		revision = flags.Int64("revision", 0, "revision to read (default: the head)")
	case "put":
		base = flags.Int64("base", -1, "head revision this edit starts from; 0 creates the note")
		file = flags.String("file", "", "file holding the new body (UTF-8 text, at most 48 KiB)")
		mediaType = flags.String("media-type", "", "body media type (default: from the file extension; .md is text/markdown, unknown is text/plain)")
		id = flags.String("id", "", "revision UUID (set to retry idempotently)")
	case "log":
		limit = flags.Int("limit", 100, "maximum revisions to list, newest first")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *space == "" || *note == "" {
		return errors.New("--space and --note are required")
	}
	client, err := newClient()
	if err != nil {
		return err
	}
	ctx := context.Background()
	path := "/v1/spaces/" + url.PathEscape(*space) + "/notes/" + url.PathEscape(*note)
	switch sub {
	case "get":
		if *revision < 0 {
			return errors.New("--revision must be positive")
		}
		if *revision > 0 {
			path += fmt.Sprintf("?revision=%d", *revision)
		}
		return requestJSON(ctx, client, "GET", path, nil, stdout)
	case "log":
		if *limit < 1 || *limit > 100 {
			return errors.New("--limit must be between 1 and 100")
		}
		return requestJSON(ctx, client, "GET", fmt.Sprintf("%s/revisions?limit=%d", path, *limit), nil, stdout)
	}
	body, err := putNoteBody(client, *space, *note, *base, *file, *mediaType, *id)
	if err != nil {
		return err
	}
	data, status, err := client.Do(ctx, "PUT", path, body)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return inbox.PrintAPIError(data, status)
	}
	_, err = fmt.Fprintln(stdout, strings.TrimSpace(string(data)))
	return err
}

// putNoteBody reads the new body from file and returns the signed revision.
func putNoteBody(client *inbox.Client, space, note string, base int64, file, mediaType, id string) ([]byte, error) {
	if base < 0 || file == "" {
		return nil, errors.New("note put requires --base (0 to create the note, else the head revision you edited) and --file")
	}
	handle, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	content, err := io.ReadAll(io.LimitReader(handle, inbox.MaxNoteBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(content) > inbox.MaxNoteBodyBytes {
		return nil, errors.New("note body must be 48 KiB or smaller; reference larger documents as artifacts")
	}
	if !utf8.Valid(content) || strings.ContainsRune(string(content), 0) {
		return nil, errors.New("note body must be UTF-8 text without NUL characters")
	}
	if mediaType == "" {
		mediaType = noteMediaTypeFor(file)
	}
	if id == "" {
		if id, err = inbox.NewUUID(); err != nil {
			return nil, err
		}
	}
	revision := inbox.NoteRevision{
		Type: inbox.NoteRevisionType, RevisionID: id, SpaceID: space, NoteID: note, BaseRevision: base,
		AuthorID: client.Agent, KeyID: client.KeyID, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		ContentIsData: true, MediaType: mediaType, SHA256: inbox.NoteBodySHA256(string(content)), Body: string(content),
	}
	if err := inbox.SignNoteRevision(&revision, client.Private); err != nil {
		return nil, err
	}
	return json.Marshal(revision)
}

// noteMediaTypeFor guesses a note body's media type from its file name. A
// note is text, so an unknown extension is text/plain.
func noteMediaTypeFor(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown":
		return "text/markdown"
	}
	if guessed := mediaTypeFor(path); guessed != "application/octet-stream" {
		return guessed
	}
	return "text/plain"
}
