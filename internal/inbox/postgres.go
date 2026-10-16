package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// PostgresStore is the PostgreSQL implementation of Store. It uses the same
// transactional contract as SQLiteStore and keeps driver-specific schema and
// SQL in this file.
type PostgresStore struct{ db *sql.DB }

func OpenPostgres(databaseURL string) (*PostgresStore, error) {
	return openPostgres(databaseURL, true)
}

func OpenPostgresWithoutMigration(databaseURL string) (*PostgresStore, error) {
	return openPostgres(databaseURL, false)
}

func openPostgres(databaseURL string, migrate bool) (*PostgresStore, error) {
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
	if migrate {
		if err := store.migrate(ctx); err != nil {
			db.Close()
			return nil, err
		}
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
		`ALTER TABLE notification_state ADD COLUMN IF NOT EXISTS disposition TEXT`,
		`CREATE TABLE IF NOT EXISTS registry_state (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			sha256 TEXT NOT NULL,
			agents_json TEXT NOT NULL,
			observed_at TIMESTAMPTZ NOT NULL
		)`,
		`ALTER TABLE registry_state ADD COLUMN IF NOT EXISTS spaces_json TEXT`,
		`ALTER TABLE messages ADD COLUMN IF NOT EXISTS space_id TEXT NOT NULL DEFAULT ''`,
		`CREATE TABLE IF NOT EXISTS deliveries (
			message_id TEXT NOT NULL REFERENCES messages(id),
			recipient_id TEXT NOT NULL,
			tenant_id TEXT NOT NULL,
			acknowledged_at TIMESTAMPTZ,
			disposition TEXT,
			attempts INTEGER NOT NULL DEFAULT 0,
			last_notified_at TIMESTAMPTZ,
			escalated_at TIMESTAMPTZ,
			PRIMARY KEY(message_id, recipient_id)
		)`,
		`CREATE INDEX IF NOT EXISTS deliveries_recipient ON deliveries(recipient_id, tenant_id, acknowledged_at)`,
		deliveryBackfill,
		`CREATE TABLE IF NOT EXISTS notes (
			tenant_id TEXT NOT NULL,
			space_id TEXT NOT NULL,
			note_id TEXT NOT NULL,
			head_revision BIGINT NOT NULL,
			created_by TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY(tenant_id, space_id, note_id)
		)`,
		`CREATE TABLE IF NOT EXISTS note_revisions (
			tenant_id TEXT NOT NULL,
			space_id TEXT NOT NULL,
			note_id TEXT NOT NULL,
			revision BIGINT NOT NULL,
			base_revision BIGINT NOT NULL,
			revision_id TEXT NOT NULL UNIQUE,
			author_id TEXT NOT NULL,
			key_id TEXT NOT NULL,
			created_at TEXT NOT NULL,
			media_type TEXT NOT NULL,
			sha256 TEXT NOT NULL,
			body TEXT NOT NULL,
			signature TEXT NOT NULL,
			accepted_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY(tenant_id, space_id, note_id, revision)
		)`,
		`CREATE OR REPLACE FUNCTION agent_inbox_reject_note_revision_mutation() RETURNS trigger AS $$
			BEGIN RAISE EXCEPTION 'note revisions are append-only'; END;
		$$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS note_revisions_no_update ON note_revisions`,
		`CREATE TRIGGER note_revisions_no_update BEFORE UPDATE ON note_revisions FOR EACH ROW EXECUTE FUNCTION agent_inbox_reject_note_revision_mutation()`,
		`DROP TRIGGER IF EXISTS note_revisions_no_delete ON note_revisions`,
		`CREATE TRIGGER note_revisions_no_delete BEFORE DELETE ON note_revisions FOR EACH ROW EXECUTE FUNCTION agent_inbox_reject_note_revision_mutation()`,
		`CREATE TABLE IF NOT EXISTS artifacts (
			tenant_id TEXT NOT NULL,
			space_id TEXT NOT NULL,
			sha256 TEXT NOT NULL,
			size BIGINT NOT NULL,
			media_type TEXT NOT NULL,
			state TEXT NOT NULL CHECK (state IN ('pending', 'available')),
			uploaded_by TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			stored_at TIMESTAMPTZ,
			PRIMARY KEY(tenant_id, space_id, sha256)
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
	_, _ = s.db.ExecContext(ctx, `DELETE FROM request_nonces WHERE created_at < $1`, now.UTC().Add(-requestNonceRetention))
	return nil
}

func (s *PostgresStore) GetMessage(ctx context.Context, id, tenant string) (DeliveredMessage, error) {
	return scanMessage(s.db.QueryRowContext(ctx,
		`SELECT envelope_json, tenant_id, sequence, accepted_at, acknowledged_at FROM messages WHERE id = $1 AND tenant_id = $2`, id, tenant))
}

func (s *PostgresStore) GetReplyTarget(ctx context.Context, id, tenant, recipient string) (DeliveredMessage, error) {
	return scanMessage(s.db.QueryRowContext(ctx,
		`SELECT m.envelope_json, m.tenant_id, m.sequence, m.accepted_at, d.acknowledged_at FROM messages m
		JOIN deliveries d ON d.message_id = m.id AND d.recipient_id = $3 WHERE m.id = $1 AND m.tenant_id = $2`, id, tenant, recipient))
}

func (s *PostgresStore) CreateMessage(ctx context.Context, e Envelope, tenant string, recipients []string, auditDetail map[string]any) (DeliveredMessage, bool, error) {
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
	err = tx.QueryRowContext(ctx, `INSERT INTO messages(id, sender_id, recipient_id, space_id, tenant_id, kind, task_id, thread_id, reply_to, envelope_json, accepted_at)
		VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT(id) DO NOTHING RETURNING sequence`,
		e.ID, e.SenderID, e.RecipientID, e.SpaceID, tenant, e.Kind, e.TaskID, e.ThreadID, e.ReplyTo, string(encoded), now).Scan(&sequence)
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
	for _, recipient := range recipients {
		if _, err := tx.ExecContext(ctx, `INSERT INTO deliveries(message_id, recipient_id, tenant_id) VALUES($1, $2, $3)`, e.ID, recipient, tenant); err != nil {
			return DeliveredMessage{}, false, err
		}
	}
	if err := insertPostgresAudit(ctx, tx, AuditRecord{Action: "send", ActorID: e.SenderID, SubjectID: e.ID, TenantID: tenant, Outcome: "accepted", Detail: auditDetail}, now); err != nil {
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

func (s *PostgresStore) ListMessages(ctx context.Context, recipient, tenant string, readableSpaces []string, limit int) ([]DeliveredMessage, error) {
	spaceFilter, spaceArgs := readableSpaceFilter(readableSpaces, func(i int) string { return fmt.Sprintf("$%d", i+3) })
	args := append([]any{recipient, tenant}, spaceArgs...)
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT m.envelope_json, m.tenant_id, m.sequence, m.accepted_at, d.acknowledged_at
		FROM deliveries d JOIN messages m ON m.id = d.message_id
		WHERE d.recipient_id = $1 AND d.tenant_id = $2 AND d.acknowledged_at IS NULL AND %s
		ORDER BY m.sequence ASC LIMIT $%d`, spaceFilter, len(args)), args...)
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

func (s *PostgresStore) Acknowledge(ctx context.Context, id, agent, tenant string, readableSpaces []string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var messageTenant, space string
	err = tx.QueryRowContext(ctx, `SELECT tenant_id, space_id FROM messages WHERE id = $1`, id).Scan(&messageTenant, &space)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrMessageNotFound
	}
	if err != nil {
		return false, err
	}
	if messageTenant != tenant {
		return false, ErrWrongTenant
	}
	var acknowledged any
	err = tx.QueryRowContext(ctx, `SELECT acknowledged_at FROM deliveries WHERE message_id = $1 AND recipient_id = $2 FOR UPDATE`, id, agent).Scan(&acknowledged)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotRecipient
	}
	if err != nil {
		return false, err
	}
	if space != "" && !slices.Contains(readableSpaces, space) {
		return false, ErrNotMember
	}
	duplicate := acknowledged != nil
	if !duplicate {
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET acknowledged_at = $1 WHERE message_id = $2 AND recipient_id = $3`, now, id, agent); err != nil {
			return false, err
		}
		// A direct message keeps its legacy acknowledgement in step, so an
		// older release run after a rollback does not deliver it again.
		if space == "" {
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET acknowledged_at = $1 WHERE id = $2`, now, id); err != nil {
				return false, err
			}
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

func insertPostgresAudit(ctx context.Context, exec sqlExecer, record AuditRecord, now time.Time) error {
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
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, d.recipient_id, d.tenant_id, m.space_id, m.sequence, d.attempts, d.escalated_at
		FROM deliveries d JOIN messages m ON m.id = d.message_id
		WHERE d.acknowledged_at IS NULL AND d.disposition IS NULL AND (d.last_notified_at IS NULL OR d.last_notified_at <= $1)
		AND (d.attempts < $2 OR d.escalated_at IS NULL) ORDER BY m.sequence ASC, d.recipient_id ASC`, cutoff, maxAttempts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	candidates := make([]NotificationCandidate, 0)
	for rows.Next() {
		var candidate NotificationCandidate
		var escalated any
		if err := rows.Scan(&candidate.MessageID, &candidate.Recipient, &candidate.TenantID, &candidate.SpaceID, &candidate.Sequence, &candidate.Attempts, &escalated); err != nil {
			return nil, err
		}
		candidate.Escalated = escalated != nil
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

func (s *PostgresStore) MarkNotified(ctx context.Context, id, recipient string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE deliveries SET attempts = attempts + 1, last_notified_at = $1 WHERE message_id = $2 AND recipient_id = $3`, now.UTC(), id, recipient)
	return err
}

func (s *PostgresStore) MarkEscalated(ctx context.Context, id, recipient string, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var tenant string
	var acknowledged any
	if err := tx.QueryRowContext(ctx, `SELECT tenant_id, acknowledged_at FROM deliveries WHERE message_id = $1 AND recipient_id = $2 FOR UPDATE`, id, recipient).Scan(&tenant, &acknowledged); err != nil {
		return false, err
	}
	if acknowledged != nil {
		return false, tx.Commit()
	}
	result, err := tx.ExecContext(ctx, `UPDATE deliveries SET escalated_at = $1
		WHERE message_id = $2 AND recipient_id = $3 AND escalated_at IS NULL AND disposition IS NULL AND acknowledged_at IS NULL`, now.UTC(), id, recipient)
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
	if err := insertPostgresAudit(ctx, tx, escalationAudit(id, recipient, tenant), now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *PostgresStore) MarkRecipientRevoked(ctx context.Context, id, recipient, reason string, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var tenant string
	var acknowledged any
	if err := tx.QueryRowContext(ctx, `SELECT tenant_id, acknowledged_at FROM deliveries WHERE message_id = $1 AND recipient_id = $2 FOR UPDATE`, id, recipient).Scan(&tenant, &acknowledged); err != nil {
		return false, err
	}
	if acknowledged != nil {
		return false, tx.Commit()
	}
	result, err := tx.ExecContext(ctx, `UPDATE deliveries SET disposition = $1
		WHERE message_id = $2 AND recipient_id = $3 AND disposition IS NULL AND escalated_at IS NULL`, DispositionRecipientRevoked, id, recipient)
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
	if err := insertPostgresAudit(ctx, tx, recipientRevokedAudit(id, recipient, tenant, reason), now); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *PostgresStore) RegistryState(ctx context.Context) (RecordedRegistry, error) {
	return scanRecordedRegistry(s.db.QueryRowContext(ctx, `SELECT sha256, agents_json, spaces_json FROM registry_state WHERE id = 1`))
}

func (s *PostgresStore) RecordRegistryChange(ctx context.Context, change RegistryChange) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var result sql.Result
	if change.PreviousSHA256 == "" {
		result, err = tx.ExecContext(ctx, `INSERT INTO registry_state(id, sha256, agents_json, spaces_json, observed_at) VALUES(1, $1, $2, $3, $4) ON CONFLICT(id) DO NOTHING`, change.SHA256, string(change.AgentsJSON), string(change.SpacesJSON), change.ObservedAt.UTC())
	} else {
		result, err = tx.ExecContext(ctx, `UPDATE registry_state SET sha256 = $1, agents_json = $2, spaces_json = $3, observed_at = $4 WHERE id = 1 AND sha256 = $5`, change.SHA256, string(change.AgentsJSON), string(change.SpacesJSON), change.ObservedAt.UTC(), change.PreviousSHA256)
	}
	if err != nil {
		return false, err
	}
	if updated, err := result.RowsAffected(); err != nil || updated == 0 {
		return false, err
	}
	if err := insertPostgresAudit(ctx, tx, change.Audit, change.ObservedAt); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *PostgresStore) PutNoteRevision(ctx context.Context, w NoteWrite) (NoteWriteResult, error) {
	return postgresNotes.put(ctx, s.db, w)
}

func (s *PostgresStore) GetNoteRevision(ctx context.Context, tenant, space, note string, revision int64) (StoredNoteRevision, error) {
	return postgresNotes.get(ctx, s.db, tenant, space, note, revision)
}

func (s *PostgresStore) ListNoteRevisions(ctx context.Context, tenant, space, note string, limit int) ([]StoredNoteRevision, error) {
	return postgresNotes.list(ctx, s.db, tenant, space, note, limit)
}

func (s *PostgresStore) GetArtifact(ctx context.Context, tenant, space, sha string) (ArtifactRecord, error) {
	return scanArtifact(s.db.QueryRowContext(ctx, `SELECT `+artifactColumns+` FROM artifacts WHERE tenant_id = $1 AND space_id = $2 AND sha256 = $3`, tenant, space, sha))
}

func (s *PostgresStore) GrantArtifactUpload(ctx context.Context, record ArtifactRecord, audit AuditRecord) (ArtifactRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ArtifactRecord{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO artifacts(tenant_id, space_id, sha256, size, media_type, state, uploaded_by, created_at)
		VALUES($1, $2, $3, $4, $5, 'pending', $6, $7)
		ON CONFLICT(tenant_id, space_id, sha256) DO UPDATE SET size = excluded.size, media_type = excluded.media_type, uploaded_by = excluded.uploaded_by
		WHERE artifacts.state = 'pending'`,
		record.TenantID, record.SpaceID, record.SHA256, record.Size, record.MediaType, record.UploadedBy, record.CreatedAt.UTC())
	if err != nil {
		return ArtifactRecord{}, err
	}
	if updated, err := result.RowsAffected(); err != nil {
		return ArtifactRecord{}, err
	} else if updated == 1 {
		if err := insertPostgresAudit(ctx, tx, audit, record.CreatedAt); err != nil {
			return ArtifactRecord{}, err
		}
	}
	stored, err := scanArtifact(tx.QueryRowContext(ctx, `SELECT `+artifactColumns+` FROM artifacts WHERE tenant_id = $1 AND space_id = $2 AND sha256 = $3`, record.TenantID, record.SpaceID, record.SHA256))
	if err != nil {
		return ArtifactRecord{}, err
	}
	return stored, tx.Commit()
}

func (s *PostgresStore) MarkArtifactStored(ctx context.Context, tenant, space, sha string, now time.Time, audit AuditRecord) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE artifacts SET state = 'available', stored_at = $1 WHERE tenant_id = $2 AND space_id = $3 AND sha256 = $4 AND state = 'pending'`,
		now.UTC(), tenant, space, sha)
	if err != nil {
		return false, err
	}
	if updated, err := result.RowsAffected(); err != nil || updated == 0 {
		return false, err
	}
	if err := insertPostgresAudit(ctx, tx, audit, now); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *PostgresStore) DiscardArtifactUpload(ctx context.Context, tenant, space, sha string, audit AuditRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM artifacts WHERE tenant_id = $1 AND space_id = $2 AND sha256 = $3 AND state = 'pending'`, tenant, space, sha); err != nil {
		return err
	}
	if err := insertPostgresAudit(ctx, tx, audit, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

type postgresStateError interface {
	error
	SQLState() string
}

func isPostgresUniqueViolation(err error) bool {
	var stateErr postgresStateError
	return errors.As(err, &stateErr) && stateErr.SQLState() == "23505"
}
