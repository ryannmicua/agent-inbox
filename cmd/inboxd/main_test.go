package main

import (
	"database/sql"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryannmicua/agent-inbox/internal/inbox"
)

func TestServerStartupRequiresWebhook(t *testing.T) {
	t.Setenv("INBOX_WEBHOOK_URL", "")
	err := run(nil, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "INBOX_WEBHOOK_URL is required") {
		t.Fatalf("startup without a notification webhook returned %v", err)
	}
}

func TestConfiguredNotifierRequiresAbsoluteHTTPURL(t *testing.T) {
	for _, webhook := range []string{"", "not-a-url", "ftp://notify.example.com/hook"} {
		t.Setenv("INBOX_WEBHOOK_URL", webhook)
		if _, err := configuredNotifier(); err == nil {
			t.Fatalf("configured notifier accepted URL %q", webhook)
		}
	}
	t.Setenv("INBOX_WEBHOOK_URL", "http://notification-sink:8081/notifications")
	notifier, err := configuredNotifier()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := notifier.(inbox.WebhookNotifier); !ok {
		t.Fatalf("configured notifier selected %T, want inbox.WebhookNotifier", notifier)
	}
}

func TestConfiguredStoreRejectsPostgresqlAlias(t *testing.T) {
	t.Setenv("INBOX_STORAGE", "postgresql")
	t.Setenv("INBOX_DATABASE_URL", "postgres://example.invalid/inbox")
	if _, err := openConfiguredStore("unused.db"); err == nil || !strings.Contains(err.Error(), "unsupported storage backend") {
		t.Fatalf("configured storage accepted the postgresql alias: %v", err)
	}
}

func TestHealthcheckPingsConfiguredStorageAndListener(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "inbox.db")
	store, err := inbox.OpenSQLite(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("INBOX_DB_PATH", databasePath)
	t.Setenv("INBOX_STORAGE", "sqlite")
	t.Setenv("INBOX_LISTEN_ADDR", listener.Addr().String())

	if err := runHealthcheck(nil, io.Discard, io.Discard); err != nil {
		t.Fatalf("healthcheck did not accept a reachable database and listener: %v", err)
	}
	if err := os.Remove(databasePath); err != nil {
		t.Fatal(err)
	}
	if err := runHealthcheck(nil, io.Discard, io.Discard); err == nil {
		t.Fatal("healthcheck accepted an unavailable database")
	}
}

func TestSQLiteOperatorCommandsDoNotMigrate(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "legacy.db")
	missingPath := filepath.Join(t.TempDir(), "missing.db")
	if _, err := inbox.OpenSQLiteWithoutMigration(missingPath); err == nil {
		t.Fatal("non-migrating open accepted a missing database")
	}
	if _, err := os.Stat(missingPath); !os.IsNotExist(err) {
		t.Fatalf("non-migrating open created a missing database: %v", err)
	}
	store, err := inbox.OpenSQLite(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"ack_by", "ack_note"} {
		if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN ` + column + ` TEXT`); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	assertLegacyAckColumns(t, databasePath)

	t.Setenv("INBOX_STORAGE", "sqlite")
	t.Setenv("INBOX_DB_PATH", databasePath)
	var auditOutput strings.Builder
	if err := runAudit([]string{"--db", databasePath}, &auditOutput, io.Discard); err != nil {
		t.Fatalf("audit failed: %v", err)
	}
	assertLegacyAckColumns(t, databasePath)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("INBOX_LISTEN_ADDR", listener.Addr().String())
	if err := runHealthcheck(nil, io.Discard, io.Discard); err != nil {
		t.Fatalf("healthcheck failed: %v", err)
	}
	assertLegacyAckColumns(t, databasePath)

	backupPath := filepath.Join(t.TempDir(), "backup.db")
	if err := runBackup([]string{"--db", databasePath, "--out", backupPath}, io.Discard, io.Discard); err != nil {
		t.Fatalf("backup failed: %v", err)
	}
	assertLegacyAckColumns(t, databasePath)
	assertLegacyAckColumns(t, backupPath)
}

func assertLegacyAckColumns(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`PRAGMA table_info(messages)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primary int
		var name, dataType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primary); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ack_by", "ack_note"} {
		if !columns[name] {
			t.Fatalf("operator command migrated away legacy column %q in %s", name, path)
		}
	}
}
