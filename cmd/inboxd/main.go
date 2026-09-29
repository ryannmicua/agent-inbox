package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/ryannmicua/agent-inbox/internal/inbox"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "inboxd:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "healthcheck":
			return runHealthcheck(args[1:], stdout, stderr)
		case "audit":
			return runAudit(args[1:], stdout, stderr)
		case "backup":
			return runBackup(args[1:], stdout, stderr)
		case "help", "--help", "-h":
			usage(stdout)
			return nil
		}
	}
	listen := envOr("INBOX_LISTEN_ADDR", ":8080")
	dbPath := envOr("INBOX_DB_PATH", "/var/lib/agent-inbox/inbox.db")
	registryPath := envOr("INBOX_REGISTRY_PATH", "/etc/agent-inbox/registry.json")
	store, err := inbox.OpenStore(envOr("INBOX_STORAGE", "sqlite"), dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	registry := inbox.FileRegistry{Path: registryPath}
	if err := registry.Validate(); err != nil {
		return fmt.Errorf("registry is not ready: %w", err)
	}
	config := inbox.DefaultServerConfig()
	config.RequestSkew = durationEnv("INBOX_REQUEST_SKEW", config.RequestSkew)
	config.RetryInterval = durationEnv("INBOX_RETRY_INTERVAL", config.RetryInterval)
	config.MaxDoorbellAttempts = intEnv("INBOX_MAX_DOORBELL_ATTEMPTS", config.MaxDoorbellAttempts)
	notifier := inbox.Notifier(inbox.NoopNotifier{})
	if webhook := os.Getenv("INBOX_WEBHOOK_URL"); webhook != "" {
		notifier = inbox.WebhookNotifier{URL: webhook, Client: &http.Client{Timeout: 5 * time.Second}}
	}
	service := inbox.NewServer(store, registry, notifier, config)
	server := &http.Server{Addr: listen, Handler: service, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go service.RunNotifications(ctx)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("agent-inbox listening on %s", listen)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func runAudit(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("audit", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", envOr("INBOX_DB_PATH", "/var/lib/agent-inbox/inbox.db"), "SQLite database path")
	limit := flags.Int("limit", 100, "number of most recent audit entries (max 1000)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *limit < 1 || *limit > 1000 {
		return errors.New("--limit must be between 1 and 1000")
	}
	if err := requireDatabaseFile(*dbPath); err != nil {
		return err
	}
	store, err := inbox.OpenSQLite(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	entries, err := store.AuditEntries(context.Background(), *limit)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(entries)
}

func runBackup(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("backup", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", envOr("INBOX_DB_PATH", "/var/lib/agent-inbox/inbox.db"), "SQLite database path")
	destination := flags.String("out", "", "new backup destination path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *destination == "" {
		return errors.New("--out is required")
	}
	if err := requireDatabaseFile(*dbPath); err != nil {
		return err
	}
	store, err := inbox.OpenSQLite(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.BackupTo(context.Background(), *destination); err != nil {
		return err
	}
	fmt.Fprintln(stdout, *destination)
	return nil
}

func requireDatabaseFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("database file %q is unavailable: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("database path %q is not a regular file", path)
	}
	return nil
}

func runHealthcheck(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("url", "http://127.0.0.1:8080/healthz", "health endpoint URL")
	if err := flags.Parse(args); err != nil {
		return err
	}
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get(*endpoint)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("health endpoint returned HTTP %d", response.StatusCode)
	}
	fmt.Fprintln(stdout, "healthy")
	return nil
}

func durationEnv(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		log.Printf("ignoring invalid %s value", name)
		return fallback
	}
	return parsed
}

func intEnv(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		log.Printf("ignoring invalid %s value", name)
		return fallback
	}
	return parsed
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `inboxd: standalone SQLite-backed agent inbox server

Run server with environment: INBOX_LISTEN_ADDR, INBOX_DB_PATH, INBOX_REGISTRY_PATH.
Operator commands:
  inboxd healthcheck [--url URL]
  inboxd audit --db PATH [--limit 100]
  inboxd backup --db PATH --out NEW_PATH`)
}
