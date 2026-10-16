// Command agent-inbox-mcp exposes one agent's inbox to an MCP client over
// stdio. It is a thin client: it signs each tool call with the agent's key
// and sends it to the inbox server, which decides everything.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ryannmicua/agent-inbox/internal/inbox"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "agent-inbox-mcp:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("agent-inbox-mcp", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { usage(stderr) }
	server := flags.String("server", envOr("INBOX_SERVER", "https://inbox.example.com"), "inbox server URL")
	agent := flags.String("agent", os.Getenv("INBOX_AGENT"), "registered agent id")
	keyPath := flags.String("key", os.Getenv("INBOX_KEY"), "base64 private key file")
	keyID := flags.String("key-id", envOr("INBOX_KEY_ID", "primary"), "registered signing key id")
	version := flags.Bool("version", false, "print the build version and exit")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *version {
		_, err := fmt.Fprintln(stdout, "agent-inbox-mcp", inbox.BuildVersion())
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
	client := &inbox.Client{Server: *server, Agent: *agent, KeyID: *keyID, Private: private}
	return serve(ctx, &adapter{client: client, log: stderr}, stdin, stdout)
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `agent-inbox-mcp: MCP stdio server for one agent's inbox

Usage: agent-inbox-mcp --server URL --agent ID --key PATH [--key-id ID]
       agent-inbox-mcp --version

Each flag can instead come from INBOX_SERVER, INBOX_AGENT, INBOX_KEY and
INBOX_KEY_ID. The MCP client starts this command and speaks JSON-RPC on its
stdin and stdout; diagnostics go to stderr.

Tools: list_spaces, read_note, propose_note_revision, send_message,
poll_inbox, ack_message.`)
}
