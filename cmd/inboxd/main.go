package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
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
	notifier, err := configuredNotifier()
	if err != nil {
		return err
	}
	store, err := openConfiguredStore(dbPath)
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
	service, err := inbox.NewServer(store, registry, notifier, config)
	if err != nil {
		return err
	}
	server := &http.Server{Addr: listen, Handler: service, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go service.RunNotifications(ctx)
	go service.RunPreAuthMetricsLog(ctx, time.Minute)
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
	if envOr("INBOX_STORAGE", "sqlite") == "sqlite" {
		if err := requireDatabaseFile(*dbPath); err != nil {
			return err
		}
	}
	store, err := openConfiguredStore(*dbPath)
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
	if envOr("INBOX_STORAGE", "sqlite") != "sqlite" {
		return errors.New("inboxd backup supports SQLite only; use pg_dump for PostgreSQL")
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
	if err := flags.Parse(args); err != nil {
		return err
	}
	dbPath := envOr("INBOX_DB_PATH", "/var/lib/agent-inbox/inbox.db")
	if envOr("INBOX_STORAGE", "sqlite") == "sqlite" {
		if err := requireDatabaseFile(dbPath); err != nil {
			return err
		}
	}
	store, err := openConfiguredStore(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := store.Ping(ctx); err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(envOr("INBOX_LISTEN_ADDR", ":8080"))
	if err != nil {
		return fmt.Errorf("parse listener address: %w", err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	connection, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 2*time.Second)
	if err != nil {
		return err
	}
	if err := connection.Close(); err != nil {
		return err
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

func openConfiguredStore(sqlitePath string) (inbox.Store, error) {
	backend := envOr("INBOX_STORAGE", "sqlite")
	location := sqlitePath
	if backend == "postgres" || backend == "postgresql" {
		location = os.Getenv("INBOX_DATABASE_URL")
	}
	return inbox.OpenStore(backend, location)
}

func configuredNotifier() (inbox.Notifier, error) {
	webhook := os.Getenv("INBOX_WEBHOOK_URL")
	disabledValue := os.Getenv("INBOX_NOTIFICATIONS_DISABLED")
	disabled := false
	if disabledValue != "" {
		parsed, err := strconv.ParseBool(disabledValue)
		if err != nil {
			return nil, errors.New("INBOX_NOTIFICATIONS_DISABLED must be true or false")
		}
		disabled = parsed
	}
	if webhook == "" && !disabled {
		return nil, errors.New("set INBOX_WEBHOOK_URL or explicitly set INBOX_NOTIFICATIONS_DISABLED=true")
	}
	if webhook != "" && disabled {
		return nil, errors.New("choose either INBOX_WEBHOOK_URL or INBOX_NOTIFICATIONS_DISABLED=true, not both")
	}
	if disabled {
		log.Printf("WARNING: human notifications are explicitly disabled; consequential inbox actions will not reach an operator")
		return inbox.NoopNotifier{}, nil
	}
	return inbox.WebhookNotifier{URL: webhook, Client: &http.Client{Timeout: 5 * time.Second}}, nil
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `inboxd: standalone agent inbox server (SQLite or PostgreSQL)

Run server with environment: INBOX_LISTEN_ADDR, INBOX_STORAGE, INBOX_DB_PATH,
INBOX_DATABASE_URL, INBOX_REGISTRY_PATH, INBOX_WEBHOOK_URL or
INBOX_NOTIFICATIONS_DISABLED=true.
Operator commands:
  inboxd healthcheck
  inboxd audit --db PATH [--limit 100]
  inboxd backup --db PATH --out NEW_PATH`)
}
