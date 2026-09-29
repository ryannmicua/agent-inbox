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
	if args[0] == "keygen" {
		return runKeygen(args[1:], stdout)
	}
	command := args[0]
	if command != "send" && command != "poll" && command != "ack" && command != "wait" {
		return fmt.Errorf("unknown command %q (run agent-inbox help)", args[0])
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	server := flags.String("server", envOr("INBOX_SERVER", "https://inbox.example.com"), "inbox server URL")
	agent := flags.String("agent", os.Getenv("INBOX_AGENT"), "registered agent id")
	keyPath := flags.String("key", os.Getenv("INBOX_KEY"), "base64 private key file")
	keyID := flags.String("key-id", envOr("INBOX_KEY_ID", "primary"), "registered signing key id")
	to := flags.String("to", "", "explicit recipient agent id")
	kind := flags.String("kind", "", "message kind")
	payloadText := flags.String("payload", "", "JSON payload (default {})")
	payloadFile := flags.String("payload-file", "", "read JSON payload from file")
	taskID := flags.String("task", "", "task id (defaults to thread id)")
	threadID := flags.String("thread", "", "thread/correlation id")
	replyTo := flags.String("reply-to", "", "message id this reply answers")
	messageID := flags.String("id", "", "message UUID (set to retry idempotently)")
	authority := flags.String("asserted-authority", "", "sender assertion; receiver must re-derive authority")
	after := flags.Int64("after-seq", 0, "return messages after this server sequence")
	limit := flags.Int("limit", 100, "maximum messages to return")
	ackID := flags.String("message", "", "message UUID to acknowledge")
	note := flags.String("note", "", "short processing note for the audit record")
	var artifacts repeatedStrings
	flags.Var(&artifacts, "artifact", "artifact reference URI|media-type|sha256|size (repeatable)")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *agent == "" || *keyPath == "" {
		return errors.New("--agent and --key (or INBOX_AGENT and INBOX_KEY) are required")
	}
	private, err := inbox.LoadPrivateKey(*keyPath)
	if err != nil {
		return fmt.Errorf("load signing key: %w", err)
	}
	client := &inbox.Client{Server: *server, Agent: *agent, KeyID: *keyID, Private: ed25519.PrivateKey(private)}
	ctx := context.Background()
	switch command {
	case "send":
		return runSend(ctx, client, *to, *kind, *payloadText, *payloadFile, *taskID, *threadID, *replyTo, *messageID, *authority, artifacts, stdout)
	case "poll":
		if *after < 0 || *limit < 1 || *limit > 100 {
			return errors.New("--after-seq must be non-negative and --limit must be between 1 and 100")
		}
		path := fmt.Sprintf("/v1/messages?after_seq=%d&limit=%d", *after, *limit)
		return requestJSON(ctx, client, "GET", path, nil, stdout)
	case "ack":
		if *ackID == "" {
			return errors.New("--message is required")
		}
		body, _ := json.Marshal(inbox.AckRequest{Processed: true, ProcessedAt: time.Now().UTC().Format(time.RFC3339Nano), Note: *note})
		return requestJSON(ctx, client, "POST", "/v1/messages/"+*ackID+"/ack", body, stdout)
	case "wait":
		for {
			response, err := client.OpenEvents(ctx)
			if err != nil {
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
				fmt.Fprintln(stderr, "doorbell stream disconnected; reconnecting:", err)
			}
			time.Sleep(2 * time.Second)
		}
	}
	return nil
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

func runSend(ctx context.Context, client *inbox.Client, recipient, kind, payloadText, payloadFile, task, thread, replyTo, id, authority string, artifactValues []string, stdout io.Writer) error {
	if recipient == "" || kind == "" {
		return errors.New("send requires --to and --kind; recipient is never guessed")
	}
	if payloadText != "" && payloadFile != "" {
		return errors.New("choose either --payload or --payload-file")
	}
	payload := []byte("{}")
	if payloadText != "" {
		payload = []byte(payloadText)
	} else if payloadFile != "" {
		var err error
		payload, err = os.ReadFile(payloadFile)
		if err != nil {
			return err
		}
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
	envelope := inbox.Envelope{Type: inbox.EnvelopeType, ID: id, TaskID: task, ThreadID: thread, ReplyTo: replyTo, SenderID: client.Agent, RecipientID: recipient, KeyID: client.KeyID, Kind: kind, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), AssertedAuthority: authority, ContentIsData: true, Payload: payload, Artifacts: artifacts}
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
  poll [--after-seq N] [--limit N]
  ack --message UUID [--note TEXT]
  wait                                   (listen for doorbell prompts)

Each API command requires --agent and --key, or INBOX_AGENT and INBOX_KEY.
The doorbell only prompts; poll for durable delivery and acknowledge after processing.`)
}
