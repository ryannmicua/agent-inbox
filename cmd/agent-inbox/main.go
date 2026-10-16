package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ryannmicua/agent-inbox/internal/inbox"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "agent-inbox:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		usage(stdout)
		return nil
	}
	if args[0] == "version" || args[0] == "--version" {
		_, err := fmt.Fprintln(stdout, "agent-inbox", inbox.BuildVersion())
		return err
	}
	if args[0] == "keygen" {
		return runKeygen(args[1:], stdout)
	}
	if args[0] == "artifact-ref" {
		return runArtifactRef(args[1:], stdout, stderr)
	}
	if args[0] == "note" {
		return runNote(args[1:], stdout, stderr)
	}
	if args[0] == "artifact" {
		return runArtifact(args[1:], stdout, stderr)
	}
	command := args[0]
	if command != "send" && command != "poll" && command != "ack" && command != "wait" && command != "spaces" {
		return fmt.Errorf("unknown command %q (run agent-inbox help)", args[0])
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	newClient := clientFlags(flags)
	to := flags.String("to", "", "explicit recipient agent id")
	space := flags.String("space", "", "space id: send to every member holding read (instead of --to)")
	kind := flags.String("kind", "", "message kind")
	payloadText := flags.String("payload", "", "JSON payload (default {})")
	payloadFile := flags.String("payload-file", "", "read JSON payload from file")
	taskID := flags.String("task", "", "task id (defaults to thread id)")
	threadID := flags.String("thread", "", "thread/correlation id")
	replyTo := flags.String("reply-to", "", "message id this reply answers")
	messageID := flags.String("id", "", "message UUID (set to retry idempotently)")
	authority := flags.String("asserted-authority", "", "sender assertion; receiver must re-derive authority")
	limit := flags.Int("limit", 100, "maximum messages to return")
	waitFor := flags.Duration("wait", 0, "poll: wait up to this long (max 10m) for a message before returning")
	ackID := flags.String("message", "", "message UUID to acknowledge")
	var artifacts repeatedStrings
	flags.Var(&artifacts, "artifact", "artifact reference URI|media-type|sha256|size (repeatable)")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	client, err := newClient()
	if err != nil {
		return err
	}
	ctx := context.Background()
	switch command {
	case "send":
		return runSend(ctx, client, *to, *space, *kind, *payloadText, *payloadFile, *taskID, *threadID, *replyTo, *messageID, *authority, artifacts, stdout)
	case "poll":
		if *limit < 1 || *limit > 100 {
			return errors.New("--limit must be between 1 and 100")
		}
		if *waitFor < 0 || *waitFor > inbox.MaxPollWait {
			return fmt.Errorf("--wait must be between 0 and %s", inbox.MaxPollWait)
		}
		path := fmt.Sprintf("/v1/messages?limit=%d", *limit)
		if *waitFor > 0 {
			data, err := client.PollWait(ctx, path, *waitFor, stderr)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(stdout, strings.TrimSpace(string(data)))
			return err
		}
		return requestJSON(ctx, client, "GET", path, nil, stdout)
	case "ack":
		if *ackID == "" {
			return errors.New("--message is required")
		}
		body, _ := json.Marshal(inbox.AckRequest{Processed: true})
		return requestJSON(ctx, client, "POST", "/v1/messages/"+*ackID+"/ack", body, stdout)
	case "spaces":
		return requestJSON(ctx, client, "GET", "/v1/spaces", nil, stdout)
	case "wait":
		for {
			response, err := client.OpenEvents(ctx)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return nil
				}
				var connectErr *inbox.EventStreamConnectError
				if !errors.As(err, &connectErr) || !inbox.RetryableNetworkError(connectErr.Err) {
					return err
				}
				fmt.Fprintln(stderr, "doorbell connection failed; reconnecting:", err)
				time.Sleep(2 * time.Second)
				continue
			}
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				return inbox.CopySSE(ctx, response, stdout)
			}
			err = inbox.CopySSE(ctx, response, stdout)
			if errors.Is(err, context.Canceled) {
				return nil
			}
			if err != nil && !errors.Is(err, io.EOF) {
				var outputErr *inbox.SSEOutputError
				if errors.As(err, &outputErr) || !retryableStreamReadError(err) {
					return err
				}
				fmt.Fprintln(stderr, "doorbell stream disconnected; reconnecting:", err)
			}
			time.Sleep(2 * time.Second)
		}
	}
	return nil
}

// clientFlags registers the flags every API command shares and returns a
// function that builds the signing client once the flags are parsed.
func clientFlags(flags *flag.FlagSet) func() (*inbox.Client, error) {
	server := flags.String("server", envOr("INBOX_SERVER", "https://inbox.example.com"), "inbox server URL")
	agent := flags.String("agent", os.Getenv("INBOX_AGENT"), "registered agent id")
	keyPath := flags.String("key", os.Getenv("INBOX_KEY"), "base64 private key file")
	keyID := flags.String("key-id", envOr("INBOX_KEY_ID", "primary"), "registered signing key id")
	return func() (*inbox.Client, error) {
		if *agent == "" || *keyPath == "" {
			return nil, errors.New("--agent and --key (or INBOX_AGENT and INBOX_KEY) are required")
		}
		private, err := inbox.LoadPrivateKey(*keyPath)
		if err != nil {
			return nil, fmt.Errorf("load signing key: %w", err)
		}
		return &inbox.Client{Server: *server, Agent: *agent, KeyID: *keyID, Private: ed25519.PrivateKey(private)}, nil
	}
}

func retryableStreamReadError(err error) bool {
	return errors.Is(err, io.ErrUnexpectedEOF) || inbox.RetryableNetworkError(err)
}

func runKeygen(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("keygen", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	private := flags.String("private-key", "agent-inbox.key", "output private key path (created mode 0600)")
	public := flags.String("public-key", "agent-inbox.pub", "output base64 public key path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := inbox.WriteKeyPair(*private, *public); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "created Ed25519 keypair\nprivate: %s\npublic:  %s\n", *private, *public)
	return nil
}

func runSend(ctx context.Context, client *inbox.Client, recipient, space, kind, payloadText, payloadFile, task, thread, replyTo, id, authority string, artifactValues []string, stdout io.Writer) error {
	if (recipient == "") == (space == "") || kind == "" {
		return errors.New("send requires --kind and exactly one of --to or --space; recipient is never guessed")
	}
	if payloadText != "" && payloadFile != "" {
		return errors.New("choose either --payload or --payload-file")
	}
	payload := []byte("{}")
	if payloadText != "" {
		payload = []byte(payloadText)
	} else if payloadFile != "" {
		file, err := os.Open(payloadFile)
		if err != nil {
			return err
		}
		defer file.Close()
		payload, err = io.ReadAll(io.LimitReader(file, inbox.MaxPayloadBytes+1))
		if err != nil {
			return err
		}
	}
	if len(payload) > inbox.MaxPayloadBytes {
		return errors.New("payload must be 16 KiB or smaller")
	}
	if !json.Valid(payload) {
		return errors.New("payload must be valid JSON")
	}
	if id == "" {
		var err error
		id, err = inbox.NewUUID()
		if err != nil {
			return err
		}
	}
	if thread == "" {
		thread = id
	}
	if task == "" {
		task = thread
	}
	artifacts := make([]inbox.Artifact, 0, len(artifactValues))
	for _, raw := range artifactValues {
		parts := strings.Split(raw, "|")
		if len(parts) != 4 {
			return fmt.Errorf("invalid --artifact %q: expected URI|media-type|sha256|size", raw)
		}
		size, err := strconv.ParseInt(parts[3], 10, 64)
		if err != nil {
			return fmt.Errorf("invalid --artifact size %q", parts[3])
		}
		artifacts = append(artifacts, inbox.Artifact{URI: parts[0], MediaType: parts[1], SHA256: parts[2], Size: size})
	}
	envelope := inbox.Envelope{Type: inbox.EnvelopeType, ID: id, TaskID: task, ThreadID: thread, ReplyTo: replyTo, SenderID: client.Agent, RecipientID: recipient, SpaceID: space, KeyID: client.KeyID, Kind: kind, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), AssertedAuthority: authority, ContentIsData: true, Payload: payload, Artifacts: artifacts}
	if err := inbox.SignEnvelope(&envelope, client.Private); err != nil {
		return err
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	return requestJSON(ctx, client, "POST", "/v1/messages", body, stdout)
}

func requestJSON(ctx context.Context, client *inbox.Client, method, path string, body []byte, stdout io.Writer) error {
	data, status, err := client.Do(ctx, method, path, body)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return inbox.PrintAPIError(data, status)
	}
	_, err = fmt.Fprintln(stdout, strings.TrimSpace(string(data)))
	return err
}

type repeatedStrings []string

func (r *repeatedStrings) String() string { return strings.Join(*r, ",") }
func (r *repeatedStrings) Set(value string) error {
	*r = append(*r, value)
	return nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `agent-inbox: signed CLI for the durable agent inbox

Commands:
  keygen --private-key PATH --public-key PATH
  send --server URL --agent ID --key PATH --to ID --kind instruction|result --payload-file FILE
  send ... --space ID --kind instruction|result   (group message to every member holding read)
  poll [--limit N] [--wait DURATION]     (--wait returns early when a message arrives)
  ack --message UUID
  wait                                   (listen for doorbell prompts)
  spaces                                 (list your spaces and your grants in each)
  note get --space ID --note ID [--revision N]
                                         (read a note's head, or one revision)
  note put --space ID --note ID --base N --file FILE [--media-type TYPE]
                                         (write a revision; --base 0 creates the note)
  note log --space ID --note ID [--limit N]
                                         (list revisions, newest first)
  artifact-ref --file PATH --uri URI [--media-type TYPE]
                                         (print an --artifact value with sha256 and size)
  artifact put --space ID --file PATH [--media-type TYPE]
                                         (upload a file to a space; prints its artifact_ref)
  artifact get (--space ID --sha256 HASH | --uri inbox-artifact://ID/HASH) --out PATH
                                         (download, verify sha256 and size, then write PATH)
  version                                (print the build version)

Each API command requires --agent and --key, or INBOX_AGENT and INBOX_KEY.
The doorbell only prompts; poll for durable delivery and acknowledge after processing.`)
}
