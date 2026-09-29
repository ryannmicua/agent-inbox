package inbox

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrMessageNotFound = errors.New("message not found")
	ErrNotRecipient    = errors.New("agent is not the message recipient")
	ErrWrongTenant     = errors.New("message belongs to another tenant")
	ErrMessageConflict = errors.New("message id already exists with different content")
	ErrReplay          = errors.New("request nonce already used")
)

type AuditRecord struct {
	Action    string
	ActorID   string
	SubjectID string
	TenantID  string
	Outcome   string
	Code      string
	Detail    map[string]any
}

type AuditEntry struct {
	Sequence  int64          `json:"sequence"`
	Occurred  time.Time      `json:"occurred_at"`
	Action    string         `json:"action"`
	ActorID   string         `json:"actor_id,omitempty"`
	SubjectID string         `json:"subject_id,omitempty"`
	TenantID  string         `json:"tenant_id,omitempty"`
	Outcome   string         `json:"outcome"`
	Code      string         `json:"code,omitempty"`
	Detail    map[string]any `json:"detail,omitempty"`
}

type NotificationCandidate struct {
	MessageID string
	Recipient string
	TenantID  string
	Sequence  int64
	Attempts  int
	Escalated bool
}

// Store is the persistence boundary shared by SQLite and PostgreSQL.
type Store interface {
	Ping(context.Context) error
	RecordNonce(context.Context, string, string, time.Time) error
	GetMessage(context.Context, string, string) (DeliveredMessage, error)
	GetReplyTarget(context.Context, string, string, string) (DeliveredMessage, error)
	CreateMessage(context.Context, Envelope, string) (DeliveredMessage, bool, error)
	ListMessages(context.Context, string, string, int) ([]DeliveredMessage, error)
	Acknowledge(context.Context, string, string, string) (bool, error)
	AppendAudit(context.Context, AuditRecord) error
	AuditEntries(context.Context, int) ([]AuditEntry, error)
	DueNotifications(context.Context, time.Time, time.Duration, int) ([]NotificationCandidate, error)
	MarkNotified(context.Context, string, time.Time) error
	MarkEscalated(context.Context, string, time.Time) (bool, error)
	Close() error
}

func OpenStore(backend, location string) (Store, error) {
	if backend == "" {
		backend = "sqlite"
	}
	switch backend {
	case "sqlite":
		return OpenSQLite(location)
	case "postgres":
		return OpenPostgres(location)
	default:
		return nil, fmt.Errorf("unsupported storage backend %q; use sqlite or postgres", backend)
	}
}

type SQLiteStore struct{ db *sql.DB }

func OpenSQLite(path string) (*SQLiteStore, error) {
	if path == "" {
		return nil, errors.New("database path is required")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // One service owns the file; serialize writes for the pilot.
	s := &SQLiteStore{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLiteStore) Close() error { return s.db.Close() }

// BackupTo creates a transactionally consistent SQLite snapshot while the
// service remains open. VACUUM INTO includes committed WAL contents in the
// destination and refuses to overwrite an existing file.
func (s *SQLiteStore) BackupTo(ctx context.Context, destination string) error {
	if destination == "" {
		return errors.New("backup destination is required")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return err
	}
	quoted := strings.ReplaceAll(destination, "'", "''")
	_, err := s.db.ExecContext(ctx, `VACUUM INTO '`+quoted+`'`)
	return err
}

func (s *SQLiteStore) migrate(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS messages (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT,
			id TEXT NOT NULL UNIQUE,
			sender_id TEXT NOT NULL,
			recipient_id TEXT NOT NULL,
			tenant_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			task_id TEXT NOT NULL,
			thread_id TEXT NOT NULL,
			reply_to TEXT NOT NULL,
			envelope_json TEXT NOT NULL,
			accepted_at TEXT NOT NULL,
			acknowledged_at TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS messages_recipient_sequence ON messages(recipient_id, sequence)`,
		`CREATE INDEX IF NOT EXISTS messages_thread ON messages(thread_id, sequence)`,
		`CREATE TABLE IF NOT EXISTS request_nonces (
			agent_id TEXT NOT NULL,
			nonce TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY(agent_id, nonce)
		)`,
		`CREATE TABLE IF NOT EXISTS audit_log (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT,
			occurred_at TEXT NOT NULL,
			action TEXT NOT NULL,
			actor_id TEXT NOT NULL DEFAULT '',
			subject_id TEXT NOT NULL DEFAULT '',
			tenant_id TEXT NOT NULL DEFAULT '',
			outcome TEXT NOT NULL,
			code TEXT NOT NULL DEFAULT '',
			detail_json TEXT NOT NULL DEFAULT '{}'
		)`,
		`CREATE TRIGGER IF NOT EXISTS audit_log_no_update BEFORE UPDATE ON audit_log BEGIN SELECT RAISE(ABORT, 'audit log is append-only'); END`,
		`CREATE TRIGGER IF NOT EXISTS audit_log_no_delete BEFORE DELETE ON audit_log BEGIN SELECT RAISE(ABORT, 'audit log is append-only'); END`,
		`CREATE TABLE IF NOT EXISTS notification_state (
			message_id TEXT PRIMARY KEY REFERENCES messages(id),
			attempts INTEGER NOT NULL DEFAULT 0,
			last_notified_at TEXT,
			escalated_at TEXT
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize sqlite schema: %w", err)
		}
	}
	for _, column := range []string{"ack_by", "ack_note"} {
		if err := dropSQLiteColumn(ctx, s.db, "messages", column); err != nil {
			return fmt.Errorf("remove obsolete acknowledgement field %s: %w", column, err)
		}
	}
	return nil
}

func dropSQLiteColumn(ctx context.Context, db *sql.DB, table, column string) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notnull, primary int
		var name, dataType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &dataType, &notnull, &defaultValue, &primary); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !found {
		return nil
	}
	_, err = db.ExecContext(ctx, `ALTER TABLE `+table+` DROP COLUMN `+column)
	return err
}

func (s *SQLiteStore) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *SQLiteStore) RecordNonce(ctx context.Context, agent, nonce string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO request_nonces(agent_id, nonce, created_at) VALUES(?, ?, ?)`,
		agent, nonce, now.UTC().Format(time.RFC3339Nano))
	if err != nil && (strings.Contains(strings.ToLower(err.Error()), "unique constraint") || strings.Contains(strings.ToLower(err.Error()), "primary key")) {
		return ErrReplay
	}
	if err != nil {
		return err
	}
	// Request timestamps are accepted only within a short skew window, so old
	// nonce rows can be pruned after one day without weakening replay defense.
	_, _ = s.db.ExecContext(ctx, `DELETE FROM request_nonces WHERE created_at < ?`, now.UTC().Add(-24*time.Hour).Format(time.RFC3339Nano))
	return nil
}

func (s *SQLiteStore) GetMessage(ctx context.Context, id, tenant string) (DeliveredMessage, error) {
	return scanMessage(s.db.QueryRowContext(ctx, `SELECT envelope_json, tenant_id, sequence, accepted_at, acknowledged_at FROM messages WHERE id = ? AND tenant_id = ?`, id, tenant))
}

func (s *SQLiteStore) GetReplyTarget(ctx context.Context, id, tenant, recipient string) (DeliveredMessage, error) {
	return scanMessage(s.db.QueryRowContext(ctx, `SELECT envelope_json, tenant_id, sequence, accepted_at, acknowledged_at FROM messages WHERE id = ? AND tenant_id = ? AND recipient_id = ?`, id, tenant, recipient))
}

func (s *SQLiteStore) CreateMessage(ctx context.Context, e Envelope, tenant string) (DeliveredMessage, bool, error) {
	encoded, err := json.Marshal(e)
	if err != nil {
		return DeliveredMessage{}, false, err
	}
	canon, err := envelopeRetryBytes(e)
	if err != nil {
		return DeliveredMessage{}, false, err
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeliveredMessage{}, false, err
	}
	defer tx.Rollback()
	var existingJSON, existingTenant string
	err = tx.QueryRowContext(ctx, `SELECT envelope_json, tenant_id FROM messages WHERE id = ?`, e.ID).Scan(&existingJSON, &existingTenant)
	if err == nil {
		if existingTenant != tenant {
			return DeliveredMessage{}, false, ErrMessageConflict
		}
		var existing Envelope
		if json.Unmarshal([]byte(existingJSON), &existing) != nil {
			return DeliveredMessage{}, false, errors.New("stored message is invalid")
		}
		oldCanon, canonErr := envelopeRetryBytes(existing)
		if canonErr != nil || !bytes.Equal(oldCanon, canon) {
			return DeliveredMessage{}, false, ErrMessageConflict
		}
		message, scanErr := scanMessage(tx.QueryRowContext(ctx, `SELECT envelope_json, tenant_id, sequence, accepted_at, acknowledged_at FROM messages WHERE id = ? AND tenant_id = ?`, e.ID, tenant))
		if scanErr != nil {
			return DeliveredMessage{}, false, scanErr
		}
		if err := insertAudit(ctx, tx, AuditRecord{Action: "send", ActorID: e.SenderID, SubjectID: e.ID, TenantID: tenant, Outcome: "duplicate", Code: "duplicate_message"}, now); err != nil {
			return DeliveredMessage{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return DeliveredMessage{}, false, err
		}
		return message, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return DeliveredMessage{}, false, err
	}
	result, err := tx.ExecContext(ctx,
		`INSERT INTO messages(id, sender_id, recipient_id, tenant_id, kind, task_id, thread_id, reply_to, envelope_json, accepted_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.SenderID, e.RecipientID, tenant, e.Kind, e.TaskID, e.ThreadID, e.ReplyTo, string(encoded), now.Format(time.RFC3339Nano))
	if err != nil {
		return DeliveredMessage{}, false, err
	}
	sequence, err := result.LastInsertId()
	if err != nil {
		return DeliveredMessage{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_state(message_id) VALUES(?)`, e.ID); err != nil {
		return DeliveredMessage{}, false, err
	}
	if err := insertAudit(ctx, tx, AuditRecord{Action: "send", ActorID: e.SenderID, SubjectID: e.ID, TenantID: tenant, Outcome: "accepted"}, now); err != nil {
		return DeliveredMessage{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return DeliveredMessage{}, false, err
	}
	return DeliveredMessage{Envelope: e, TenantID: tenant, Sequence: sequence, AcceptedAt: now}, false, nil
}

func (s *SQLiteStore) ListMessages(ctx context.Context, recipient, tenant string, limit int) ([]DeliveredMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT envelope_json, tenant_id, sequence, accepted_at, acknowledged_at FROM messages WHERE recipient_id = ? AND tenant_id = ? AND acknowledged_at IS NULL ORDER BY sequence ASC LIMIT ?`, recipient, tenant, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := make([]DeliveredMessage, 0)
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

func (s *SQLiteStore) Acknowledge(ctx context.Context, id, agent, tenant string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var recipient, messageTenant string
	var acknowledged sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT recipient_id, tenant_id, acknowledged_at FROM messages WHERE id = ?`, id).Scan(&recipient, &messageTenant, &acknowledged)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrMessageNotFound
	}
	if err != nil {
		return false, err
	}
	if messageTenant != tenant {
		return false, ErrWrongTenant
	}
	if recipient != agent {
		return false, ErrNotRecipient
	}
	duplicate := acknowledged.Valid
	if !duplicate {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET acknowledged_at = ? WHERE id = ?`, now, id); err != nil {
			return false, err
		}
	}
	outcome, code := "accepted", ""
	if duplicate {
		outcome, code = "duplicate", "already_acknowledged"
	}
	if err := insertAudit(ctx, tx, AuditRecord{Action: "ack", ActorID: agent, SubjectID: id, TenantID: tenant, Outcome: outcome, Code: code}, time.Now().UTC()); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return duplicate, nil
}

func (s *SQLiteStore) AppendAudit(ctx context.Context, record AuditRecord) error {
	return insertAudit(ctx, s.db, record, time.Now().UTC())
}

func insertAudit(ctx context.Context, exec interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, record AuditRecord, now time.Time) error {
	detail := record.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = exec.ExecContext(ctx, `INSERT INTO audit_log(occurred_at, action, actor_id, subject_id, tenant_id, outcome, code, detail_json) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		now.UTC().Format(time.RFC3339Nano), record.Action, record.ActorID, record.SubjectID, record.TenantID, record.Outcome, record.Code, string(encoded))
	return err
}

func (s *SQLiteStore) AuditEntries(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sequence, occurred_at, action, actor_id, subject_id, tenant_id, outcome, code, detail_json FROM audit_log ORDER BY sequence DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AuditEntry, 0)
	for rows.Next() {
		var e AuditEntry
		var occurred, detail string
		if err := rows.Scan(&e.Sequence, &occurred, &e.Action, &e.ActorID, &e.SubjectID, &e.TenantID, &e.Outcome, &e.Code, &detail); err != nil {
			return nil, err
		}
		e.Occurred, err = time.Parse(time.RFC3339Nano, occurred)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(detail), &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) DueNotifications(ctx context.Context, now time.Time, retry time.Duration, maxAttempts int) ([]NotificationCandidate, error) {
	cutoff := now.UTC().Add(-retry).Format(time.RFC3339Nano)
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.recipient_id, m.tenant_id, m.sequence, n.attempts, n.escalated_at FROM messages m JOIN notification_state n ON n.message_id = m.id WHERE m.acknowledged_at IS NULL AND (n.last_notified_at IS NULL OR n.last_notified_at <= ?) AND (n.attempts < ? OR n.escalated_at IS NULL) ORDER BY m.sequence ASC`, cutoff, maxAttempts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]NotificationCandidate, 0)
	for rows.Next() {
		var c NotificationCandidate
		var escalated sql.NullString
		if err := rows.Scan(&c.MessageID, &c.Recipient, &c.TenantID, &c.Sequence, &c.Attempts, &escalated); err != nil {
			return nil, err
		}
		c.Escalated = escalated.Valid
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) MarkNotified(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE notification_state SET attempts = attempts + 1, last_notified_at = ? WHERE message_id = ?`, now.UTC().Format(time.RFC3339Nano), id)
	return err
}

func (s *SQLiteStore) MarkEscalated(ctx context.Context, id string, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var tenant string
	var acknowledged sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT tenant_id, acknowledged_at FROM messages WHERE id = ?`, id).Scan(&tenant, &acknowledged); err != nil {
		return false, err
	}
	if acknowledged.Valid {
		return false, tx.Commit()
	}
	result, err := tx.ExecContext(ctx, `UPDATE notification_state SET escalated_at = ?
		WHERE message_id = ? AND escalated_at IS NULL
		AND EXISTS (SELECT 1 FROM messages WHERE id = ? AND acknowledged_at IS NULL)`, now.UTC().Format(time.RFC3339Nano), id, id)
	if err != nil {
		return false, err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if updated == 0 {
		return false, tx.Commit()
	}
	if err := insertAudit(ctx, tx, AuditRecord{Action: "message.unacknowledged_escalation", ActorID: "system", SubjectID: id, TenantID: tenant, Outcome: "attempted"}, now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

type rowScanner interface{ Scan(...any) error }

func scanMessage(row rowScanner) (DeliveredMessage, error) {
	var m DeliveredMessage
	var raw string
	var accepted, acked any
	if err := row.Scan(&raw, &m.TenantID, &m.Sequence, &accepted, &acked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeliveredMessage{}, ErrMessageNotFound
		}
		return DeliveredMessage{}, err
	}
	if err := json.Unmarshal([]byte(raw), &m.Envelope); err != nil {
		return DeliveredMessage{}, fmt.Errorf("decode stored envelope: %w", err)
	}
	var err error
	m.AcceptedAt, err = parseDatabaseTime(accepted)
	if err != nil {
		return DeliveredMessage{}, err
	}
	if acked != nil {
		parsed, err := parseDatabaseTime(acked)
		if err != nil {
			return DeliveredMessage{}, err
		}
		m.AcknowledgedAt = &parsed
	}
	return m, nil
}

func parseDatabaseTime(value any) (time.Time, error) {
	switch typed := value.(type) {
	case time.Time:
		return typed.UTC(), nil
	case string:
		return time.Parse(time.RFC3339Nano, typed)
	case []byte:
		return time.Parse(time.RFC3339Nano, string(typed))
	default:
		return time.Time{}, fmt.Errorf("unsupported database timestamp %T", value)
	}
}
