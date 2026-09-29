package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// PostgresStore is the PostgreSQL implementation of Store. It uses the same
// transactional contract as SQLiteStore and keeps driver-specific schema and
// SQL in this file.
type PostgresStore struct{ db *sql.DB }

func OpenPostgres(databaseURL string) (*PostgresStore, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return nil, errors.New("PostgreSQL database URL is required")
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	store := &PostgresStore{db: db}
	if err := store.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *PostgresStore) Close() error { return s.db.Close() }

func (s *PostgresStore) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *PostgresStore) migrate(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS messages (
			sequence BIGSERIAL PRIMARY KEY,
			id TEXT NOT NULL UNIQUE,
			sender_id TEXT NOT NULL,
			recipient_id TEXT NOT NULL,
			tenant_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			task_id TEXT NOT NULL,
			thread_id TEXT NOT NULL,
			reply_to TEXT NOT NULL,
			envelope_json TEXT NOT NULL,
			accepted_at TIMESTAMPTZ NOT NULL,
			acknowledged_at TIMESTAMPTZ
		)`,
		`CREATE INDEX IF NOT EXISTS messages_recipient_tenant_sequence ON messages(recipient_id, tenant_id, sequence)`,
		`CREATE INDEX IF NOT EXISTS messages_thread ON messages(thread_id, sequence)`,
		`CREATE TABLE IF NOT EXISTS request_nonces (
			agent_id TEXT NOT NULL,
			nonce TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY(agent_id, nonce)
		)`,
		`CREATE TABLE IF NOT EXISTS audit_log (
			sequence BIGSERIAL PRIMARY KEY,
			occurred_at TIMESTAMPTZ NOT NULL,
			action TEXT NOT NULL,
			actor_id TEXT NOT NULL DEFAULT '',
			subject_id TEXT NOT NULL DEFAULT '',
			tenant_id TEXT NOT NULL DEFAULT '',
			outcome TEXT NOT NULL,
			code TEXT NOT NULL DEFAULT '',
			detail_json TEXT NOT NULL DEFAULT '{}'
		)`,
		`CREATE OR REPLACE FUNCTION agent_inbox_reject_audit_mutation() RETURNS trigger AS $$
			BEGIN RAISE EXCEPTION 'audit log is append-only'; END;
		$$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS audit_log_no_update ON audit_log`,
		`CREATE TRIGGER audit_log_no_update BEFORE UPDATE ON audit_log FOR EACH ROW EXECUTE FUNCTION agent_inbox_reject_audit_mutation()`,
		`DROP TRIGGER IF EXISTS audit_log_no_delete ON audit_log`,
		`CREATE TRIGGER audit_log_no_delete BEFORE DELETE ON audit_log FOR EACH ROW EXECUTE FUNCTION agent_inbox_reject_audit_mutation()`,
		`CREATE TABLE IF NOT EXISTS notification_state (
			message_id TEXT PRIMARY KEY REFERENCES messages(id),
			attempts INTEGER NOT NULL DEFAULT 0,
			last_notified_at TIMESTAMPTZ,
			escalated_at TIMESTAMPTZ
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize postgres schema: %w", err)
		}
	}
	return nil
}

func (s *PostgresStore) RecordNonce(ctx context.Context, agent, nonce string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO request_nonces(agent_id, nonce, created_at) VALUES($1, $2, $3)`,
		agent, nonce, now.UTC())
	if isPostgresUniqueViolation(err) {
		return ErrReplay
	}
	if err != nil {
		return err
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM request_nonces WHERE created_at < $1`, now.UTC().Add(-24*time.Hour))
	return nil
}

func (s *PostgresStore) GetMessage(ctx context.Context, id, tenant string) (DeliveredMessage, error) {
	return scanMessage(s.db.QueryRowContext(ctx,
		`SELECT envelope_json, tenant_id, sequence, accepted_at, acknowledged_at FROM messages WHERE id = $1 AND tenant_id = $2`, id, tenant))
}

func (s *PostgresStore) GetReplyTarget(ctx context.Context, id, tenant, recipient string) (DeliveredMessage, error) {
	return scanMessage(s.db.QueryRowContext(ctx,
		`SELECT envelope_json, tenant_id, sequence, accepted_at, acknowledged_at FROM messages WHERE id = $1 AND tenant_id = $2 AND recipient_id = $3`, id, tenant, recipient))
}

func (s *PostgresStore) CreateMessage(ctx context.Context, e Envelope, tenant string) (DeliveredMessage, bool, error) {
	encoded, err := json.Marshal(e)
	if err != nil {
		return DeliveredMessage{}, false, err
	}
	canonical, err := envelopeRetryBytes(e)
	if err != nil {
		return DeliveredMessage{}, false, err
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeliveredMessage{}, false, err
	}
	defer tx.Rollback()
	if message, found, err := s.duplicate(ctx, tx, e, tenant, canonical, now); err != nil {
		return DeliveredMessage{}, false, err
	} else if found {
		if err := tx.Commit(); err != nil {
			return DeliveredMessage{}, false, err
		}
		return message, true, nil
	}

	var sequence int64
	err = tx.QueryRowContext(ctx, `INSERT INTO messages(id, sender_id, recipient_id, tenant_id, kind, task_id, thread_id, reply_to, envelope_json, accepted_at)
		VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT(id) DO NOTHING RETURNING sequence`,
		e.ID, e.SenderID, e.RecipientID, tenant, e.Kind, e.TaskID, e.ThreadID, e.ReplyTo, string(encoded), now).Scan(&sequence)
	if errors.Is(err, sql.ErrNoRows) {
		message, found, duplicateErr := s.duplicate(ctx, tx, e, tenant, canonical, now)
		if duplicateErr != nil {
			return DeliveredMessage{}, false, duplicateErr
		}
		if !found {
			return DeliveredMessage{}, false, errors.New("message id conflict could not be loaded")
		}
		if err := tx.Commit(); err != nil {
			return DeliveredMessage{}, false, err
		}
		return message, true, nil
	}
	if err != nil {
		return DeliveredMessage{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_state(message_id) VALUES($1)`, e.ID); err != nil {
		return DeliveredMessage{}, false, err
	}
	if err := insertPostgresAudit(ctx, tx, AuditRecord{Action: "send", ActorID: e.SenderID, SubjectID: e.ID, TenantID: tenant, Outcome: "accepted"}, now); err != nil {
		return DeliveredMessage{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return DeliveredMessage{}, false, err
	}
	return DeliveredMessage{Envelope: e, TenantID: tenant, Sequence: sequence, AcceptedAt: now}, false, nil
}

func (s *PostgresStore) duplicate(ctx context.Context, tx *sql.Tx, e Envelope, tenant string, canonical []byte, now time.Time) (DeliveredMessage, bool, error) {
	var existingJSON, existingTenant string
	err := tx.QueryRowContext(ctx, `SELECT envelope_json, tenant_id FROM messages WHERE id = $1`, e.ID).Scan(&existingJSON, &existingTenant)
	if errors.Is(err, sql.ErrNoRows) {
		return DeliveredMessage{}, false, nil
	}
	if err != nil {
		return DeliveredMessage{}, false, err
	}
	if existingTenant != tenant {
		return DeliveredMessage{}, false, ErrMessageConflict
	}
	var existing Envelope
	if err := json.Unmarshal([]byte(existingJSON), &existing); err != nil {
		return DeliveredMessage{}, false, errors.New("stored message is invalid")
	}
	oldCanonical, err := envelopeRetryBytes(existing)
	if err != nil || string(oldCanonical) != string(canonical) {
		return DeliveredMessage{}, false, ErrMessageConflict
	}
	message, err := scanMessage(tx.QueryRowContext(ctx,
		`SELECT envelope_json, tenant_id, sequence, accepted_at, acknowledged_at FROM messages WHERE id = $1 AND tenant_id = $2`, e.ID, tenant))
	if err != nil {
		return DeliveredMessage{}, false, err
	}
	if err := insertPostgresAudit(ctx, tx, AuditRecord{Action: "send", ActorID: e.SenderID, SubjectID: e.ID, TenantID: tenant, Outcome: "duplicate", Code: "duplicate_message"}, now); err != nil {
		return DeliveredMessage{}, false, err
	}
	return message, true, nil
}

func (s *PostgresStore) ListMessages(ctx context.Context, recipient, tenant string, limit int) ([]DeliveredMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT envelope_json, tenant_id, sequence, accepted_at, acknowledged_at FROM messages
		WHERE recipient_id = $1 AND tenant_id = $2 AND acknowledged_at IS NULL ORDER BY sequence ASC LIMIT $3`, recipient, tenant, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := make([]DeliveredMessage, 0)
	for rows.Next() {
		message, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

func (s *PostgresStore) Acknowledge(ctx context.Context, id, agent, tenant string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var recipient, messageTenant string
	var acknowledged any
	err = tx.QueryRowContext(ctx, `SELECT recipient_id, tenant_id, acknowledged_at FROM messages WHERE id = $1 FOR UPDATE`, id).Scan(&recipient, &messageTenant, &acknowledged)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrMessageNotFound
	}
	if err != nil {
		return false, err
	}
	if recipient != agent || messageTenant != tenant {
		return false, ErrNotRecipient
	}
	duplicate := acknowledged != nil
	if !duplicate {
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET acknowledged_at = $1 WHERE id = $2`, time.Now().UTC(), id); err != nil {
			return false, err
		}
	}
	outcome, code := "accepted", ""
	if duplicate {
		outcome, code = "duplicate", "already_acknowledged"
	}
	if err := insertPostgresAudit(ctx, tx, AuditRecord{Action: "ack", ActorID: agent, SubjectID: id, TenantID: tenant, Outcome: outcome, Code: code}, time.Now().UTC()); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return duplicate, nil
}

func (s *PostgresStore) AppendAudit(ctx context.Context, record AuditRecord) error {
	return insertPostgresAudit(ctx, s.db, record, time.Now().UTC())
}

func insertPostgresAudit(ctx context.Context, exec interface {
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
	_, err = exec.ExecContext(ctx, `INSERT INTO audit_log(occurred_at, action, actor_id, subject_id, tenant_id, outcome, code, detail_json)
		VALUES($1, $2, $3, $4, $5, $6, $7, $8)`, now.UTC(), record.Action, record.ActorID, record.SubjectID, record.TenantID, record.Outcome, record.Code, string(encoded))
	return err
}

func (s *PostgresStore) AuditEntries(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sequence, occurred_at, action, actor_id, subject_id, tenant_id, outcome, code, detail_json
		FROM audit_log ORDER BY sequence DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make([]AuditEntry, 0)
	for rows.Next() {
		var entry AuditEntry
		var occurred any
		var detail string
		if err := rows.Scan(&entry.Sequence, &occurred, &entry.Action, &entry.ActorID, &entry.SubjectID, &entry.TenantID, &entry.Outcome, &entry.Code, &detail); err != nil {
			return nil, err
		}
		entry.Occurred, err = parseDatabaseTime(occurred)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(detail), &entry.Detail); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func (s *PostgresStore) DueNotifications(ctx context.Context, now time.Time, retry time.Duration, maxAttempts int) ([]NotificationCandidate, error) {
	cutoff := now.UTC().Add(-retry)
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.recipient_id, m.tenant_id, m.sequence, n.attempts, n.escalated_at
		FROM messages m JOIN notification_state n ON n.message_id = m.id
		WHERE m.acknowledged_at IS NULL AND (n.last_notified_at IS NULL OR n.last_notified_at <= $1)
		AND (n.attempts < $2 OR n.escalated_at IS NULL) ORDER BY m.sequence ASC`, cutoff, maxAttempts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	candidates := make([]NotificationCandidate, 0)
	for rows.Next() {
		var candidate NotificationCandidate
		var escalated any
		if err := rows.Scan(&candidate.MessageID, &candidate.Recipient, &candidate.TenantID, &candidate.Sequence, &candidate.Attempts, &escalated); err != nil {
			return nil, err
		}
		candidate.Escalated = escalated != nil
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

func (s *PostgresStore) MarkNotified(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE notification_state SET attempts = attempts + 1, last_notified_at = $1 WHERE message_id = $2`, now.UTC(), id)
	return err
}

func (s *PostgresStore) MarkEscalated(ctx context.Context, id string, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var tenant string
	var acknowledged any
	if err := tx.QueryRowContext(ctx, `SELECT tenant_id, acknowledged_at FROM messages WHERE id = $1 FOR UPDATE`, id).Scan(&tenant, &acknowledged); err != nil {
		return false, err
	}
	if acknowledged != nil {
		return false, tx.Commit()
	}
	result, err := tx.ExecContext(ctx, `UPDATE notification_state SET escalated_at = $1
		WHERE message_id = $2 AND escalated_at IS NULL
		AND EXISTS (SELECT 1 FROM messages WHERE id = $2 AND acknowledged_at IS NULL)`, now.UTC(), id)
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
	if err := insertPostgresAudit(ctx, tx, AuditRecord{Action: "message.unacknowledged_escalation", ActorID: "system", SubjectID: id, TenantID: tenant, Outcome: "attempted"}, now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

type postgresStateError interface {
	error
	SQLState() string
}

func isPostgresUniqueViolation(err error) bool {
	var stateErr postgresStateError
	return errors.As(err, &stateErr) && stateErr.SQLState() == "23505"
}
