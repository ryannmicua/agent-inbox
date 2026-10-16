package inbox

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrMessageNotFound  = errors.New("message not found")
	ErrNotRecipient     = errors.New("agent is not the message recipient")
	ErrNotMember        = errors.New("agent no longer reads the message's space")
	ErrWrongTenant      = errors.New("message belongs to another tenant")
	ErrMessageConflict  = errors.New("message id already exists with different content")
	ErrReplay           = errors.New("request nonce already used")
	ErrArtifactNotFound = errors.New("artifact not found")
)

const requestNonceRetention = 24 * time.Hour

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

// NotificationCandidate is one unacknowledged delivery: a message and one of
// its recipients. SpaceID is set for a group message.
type NotificationCandidate struct {
	MessageID string
	Recipient string
	TenantID  string
	SpaceID   string
	Sequence  int64
	Attempts  int
	Escalated bool
}

// Store is the persistence boundary shared by SQLite and PostgreSQL.
type Store interface {
	Ping(context.Context) error
	RecordNonce(context.Context, string, string, time.Time) error
	GetMessage(context.Context, string, string) (DeliveredMessage, error)
	// GetReplyTarget returns a message in the tenant that was delivered to the
	// given recipient.
	GetReplyTarget(context.Context, string, string, string) (DeliveredMessage, error)
	// CreateMessage stores a new message, one delivery per recipient, and its
	// send accepted audit row (with the given detail) in one transaction. An
	// identical retry returns the stored message and true.
	CreateMessage(ctx context.Context, e Envelope, tenant string, recipients []string, auditDetail map[string]any) (DeliveredMessage, bool, error)
	// ListMessages returns the recipient's unacknowledged deliveries in sequence
	// order. Group messages appear only for spaces in readableSpaces.
	ListMessages(ctx context.Context, recipient, tenant string, readableSpaces []string, limit int) ([]DeliveredMessage, error)
	// Acknowledge marks only the caller's own delivery. A group message whose
	// space is not in readableSpaces returns ErrNotMember.
	Acknowledge(ctx context.Context, id, agent, tenant string, readableSpaces []string) (bool, error)
	AppendAudit(context.Context, AuditRecord) error
	AuditEntries(context.Context, int) ([]AuditEntry, error)
	DueNotifications(context.Context, time.Time, time.Duration, int) ([]NotificationCandidate, error)
	MarkNotified(ctx context.Context, messageID, recipient string, now time.Time) error
	MarkEscalated(ctx context.Context, messageID, recipient string, now time.Time) (bool, error)
	// MarkRecipientRevoked stops doorbells for an unacknowledged delivery whose
	// recipient can no longer collect it and audits that once. It reports
	// whether this call made the transition.
	MarkRecipientRevoked(ctx context.Context, messageID, recipient, reason string, now time.Time) (bool, error)
	// RegistryState returns the last registry state recorded by
	// RecordRegistryChange, or an empty hash when none has been recorded.
	RegistryState(context.Context) (RecordedRegistry, error)
	// RecordRegistryChange replaces the recorded registry state and appends its
	// audit record in one transaction, only if the recorded hash still equals
	// the given previous hash. It reports whether this call recorded the change.
	RecordRegistryChange(context.Context, RegistryChange) (bool, error)
	// PutNoteRevision stores a revision whose base is the note's head as
	// head+1, with its audit row and notice deliveries, in one transaction. An
	// identical retry returns the stored revision; a stale base returns
	// *NoteConflictError and stores nothing.
	PutNoteRevision(context.Context, NoteWrite) (NoteWriteResult, error)
	// GetNoteRevision returns one revision of a note, or its head when
	// revision is 0, or ErrNoteNotFound.
	GetNoteRevision(ctx context.Context, tenant, space, note string, revision int64) (StoredNoteRevision, error)
	// ListNoteRevisions returns a note's revisions newest first, or
	// ErrNoteNotFound.
	ListNoteRevisions(ctx context.Context, tenant, space, note string, limit int) ([]StoredNoteRevision, error)
	// GetArtifact returns one artifact recorded in a tenant's space, or
	// ErrArtifactNotFound.
	GetArtifact(ctx context.Context, tenant, space, sha256 string) (ArtifactRecord, error)
	// GrantArtifactUpload records a pending upload and its audit row in one
	// transaction; a pending row takes the new size, media type, and uploader.
	// An available artifact is returned unchanged and nothing is written.
	GrantArtifactUpload(context.Context, ArtifactRecord, AuditRecord) (ArtifactRecord, error)
	// MarkArtifactStored makes a pending artifact available and appends its
	// audit row in one transaction. It reports whether this call made the
	// transition.
	MarkArtifactStored(ctx context.Context, tenant, space, sha256 string, now time.Time, audit AuditRecord) (bool, error)
	// DiscardArtifactUpload removes a pending artifact and appends its audit
	// row in one transaction. An available artifact is kept.
	DiscardArtifactUpload(ctx context.Context, tenant, space, sha256 string, audit AuditRecord) error
	Close() error
}

// Artifact states: an upload is pending until the inbox has re-read the
// object from its own store and found the declared hash and size.
const (
	ArtifactPending   = "pending"
	ArtifactAvailable = "available"
)

// ArtifactRecord is one content-addressed file in a space. Its object lives
// in the configured object store under ArtifactObjectKey.
type ArtifactRecord struct {
	TenantID   string
	SpaceID    string
	SHA256     string
	Size       int64
	MediaType  string
	State      string
	UploadedBy string
	CreatedAt  time.Time
	StoredAt   *time.Time
}

// RecordedRegistry is the last observed registry: its hash and the parsed
// agents and spaces to diff the next edit against. SpacesJSON is nil when the
// row was written before spaces existed.
type RecordedRegistry struct {
	SHA256     string
	AgentsJSON []byte
	SpacesJSON []byte
}

// RegistryChange is one observed registry edit: the hash transition, the
// parsed agents and spaces to diff the next edit against, and the audit
// record.
type RegistryChange struct {
	PreviousSHA256 string
	SHA256         string
	AgentsJSON     []byte
	SpacesJSON     []byte
	Audit          AuditRecord
	ObservedAt     time.Time
}

// DispositionRecipientRevoked marks pending work whose recipient was revoked,
// removed, or reassigned to another tenant, so it will not be collected.
const DispositionRecipientRevoked = "recipient_revoked"

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
	return openSQLite(path, true)
}

func OpenSQLiteWithoutMigration(path string) (*SQLiteStore, error) {
	return openSQLite(path, false)
}

func openSQLite(path string, migrate bool) (*SQLiteStore, error) {
	if path == "" {
		return nil, errors.New("database path is required")
	}
	if migrate && path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	if !migrate {
		// A file: URL needs an absolute path; a relative one fails to open with a
		// misleading SQLite error.
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve database path: %w", err)
		}
		fileURL := url.URL{Scheme: "file", Path: absolute}
		query := fileURL.Query()
		query.Set("mode", "rw")
		query.Add("_pragma", "busy_timeout(5000)")
		query.Add("_pragma", "foreign_keys(1)")
		fileURL.RawQuery = query.Encode()
		dsn = fileURL.String()
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // One service owns the file; serialize writes for the pilot.
	s := &SQLiteStore{db: db}
	if migrate {
		err = s.migrate(context.Background())
	} else {
		err = db.PingContext(context.Background())
	}
	if err != nil {
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
		`CREATE TABLE IF NOT EXISTS registry_state (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			sha256 TEXT NOT NULL,
			agents_json TEXT NOT NULL,
			observed_at TEXT NOT NULL
		)`,
		sqliteDeliveriesTable,
		`CREATE INDEX IF NOT EXISTS deliveries_recipient ON deliveries(recipient_id, tenant_id, acknowledged_at)`,
		`CREATE TABLE IF NOT EXISTS notes (
			tenant_id TEXT NOT NULL,
			space_id TEXT NOT NULL,
			note_id TEXT NOT NULL,
			head_revision INTEGER NOT NULL,
			created_by TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY(tenant_id, space_id, note_id)
		)`,
		`CREATE TABLE IF NOT EXISTS note_revisions (
			tenant_id TEXT NOT NULL,
			space_id TEXT NOT NULL,
			note_id TEXT NOT NULL,
			revision INTEGER NOT NULL,
			base_revision INTEGER NOT NULL,
			revision_id TEXT NOT NULL UNIQUE,
			author_id TEXT NOT NULL,
			key_id TEXT NOT NULL,
			created_at TEXT NOT NULL,
			media_type TEXT NOT NULL,
			sha256 TEXT NOT NULL,
			body TEXT NOT NULL,
			signature TEXT NOT NULL,
			accepted_at TEXT NOT NULL,
			PRIMARY KEY(tenant_id, space_id, note_id, revision)
		)`,
		`CREATE TRIGGER IF NOT EXISTS note_revisions_no_update BEFORE UPDATE ON note_revisions BEGIN SELECT RAISE(ABORT, 'note revisions are append-only'); END`,
		`CREATE TRIGGER IF NOT EXISTS note_revisions_no_delete BEFORE DELETE ON note_revisions BEGIN SELECT RAISE(ABORT, 'note revisions are append-only'); END`,
		`CREATE TABLE IF NOT EXISTS artifacts (
			tenant_id TEXT NOT NULL,
			space_id TEXT NOT NULL,
			sha256 TEXT NOT NULL,
			size INTEGER NOT NULL,
			media_type TEXT NOT NULL,
			state TEXT NOT NULL CHECK (state IN ('pending', 'available')),
			uploaded_by TEXT NOT NULL,
			created_at TEXT NOT NULL,
			stored_at TEXT,
			PRIMARY KEY(tenant_id, space_id, sha256)
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize sqlite schema: %w", err)
		}
	}
	if err := addSQLiteColumn(ctx, s.db, "messages", "space_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("add message space: %w", err)
	}
	if err := addSQLiteColumn(ctx, s.db, "notification_state", "disposition", "TEXT"); err != nil {
		return fmt.Errorf("add notification disposition: %w", err)
	}
	if err := addSQLiteColumn(ctx, s.db, "registry_state", "spaces_json", "TEXT"); err != nil {
		return fmt.Errorf("add registry spaces state: %w", err)
	}
	for _, column := range []string{"ack_by", "ack_note"} {
		if err := dropSQLiteColumn(ctx, s.db, "messages", column); err != nil {
			return fmt.Errorf("remove obsolete acknowledgement field %s: %w", column, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, deliveryBackfill); err != nil {
		return fmt.Errorf("backfill deliveries: %w", err)
	}
	return nil
}

// sqliteDeliveriesTable holds one row per message and recipient: the
// recipient's own acknowledgement and doorbell state. It supersedes
// messages.acknowledged_at and notification_state, which stay for rollback.
const sqliteDeliveriesTable = `CREATE TABLE IF NOT EXISTS deliveries (
	message_id TEXT NOT NULL REFERENCES messages(id),
	recipient_id TEXT NOT NULL,
	tenant_id TEXT NOT NULL,
	acknowledged_at TEXT,
	disposition TEXT,
	attempts INTEGER NOT NULL DEFAULT 0,
	last_notified_at TEXT,
	escalated_at TEXT,
	PRIMARY KEY(message_id, recipient_id)
)`

// deliveryBackfill gives every direct message written before deliveries
// existed its one delivery, carrying over its acknowledgement and doorbell
// state. It runs at every startup: existing deliveries are kept, and only an
// acknowledgement recorded on the message (by an older release after a
// rollback) is copied onto a still-pending delivery. The statement is valid
// on SQLite and PostgreSQL.
const deliveryBackfill = `INSERT INTO deliveries(message_id, recipient_id, tenant_id, acknowledged_at, disposition, attempts, last_notified_at, escalated_at)
	SELECT m.id, m.recipient_id, m.tenant_id, m.acknowledged_at, n.disposition, COALESCE(n.attempts, 0), n.last_notified_at, n.escalated_at
	FROM messages m LEFT JOIN notification_state n ON n.message_id = m.id
	WHERE m.recipient_id <> ''
	ON CONFLICT(message_id, recipient_id) DO UPDATE SET acknowledged_at = excluded.acknowledged_at
	WHERE deliveries.acknowledged_at IS NULL AND excluded.acknowledged_at IS NOT NULL`

func dropSQLiteColumn(ctx context.Context, db *sql.DB, table, column string) error {
	found, err := sqliteColumnExists(ctx, db, table, column)
	if err != nil || !found {
		return err
	}
	_, err = db.ExecContext(ctx, `ALTER TABLE `+table+` DROP COLUMN `+column)
	return err
}

func addSQLiteColumn(ctx context.Context, db *sql.DB, table, column, definition string) error {
	found, err := sqliteColumnExists(ctx, db, table, column)
	if err != nil || found {
		return err
	}
	_, err = db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+column+` `+definition)
	return err
}

func sqliteColumnExists(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, err
	}
	found := false
	for rows.Next() {
		var cid, notnull, primary int
		var name, dataType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &dataType, &notnull, &defaultValue, &primary); err != nil {
			rows.Close()
			return false, err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	return found, rows.Close()
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
	_, _ = s.db.ExecContext(ctx, `DELETE FROM request_nonces WHERE created_at < ?`, now.UTC().Add(-requestNonceRetention).Format(time.RFC3339Nano))
	return nil
}

func (s *SQLiteStore) GetMessage(ctx context.Context, id, tenant string) (DeliveredMessage, error) {
	return scanMessage(s.db.QueryRowContext(ctx, `SELECT envelope_json, tenant_id, sequence, accepted_at, acknowledged_at FROM messages WHERE id = ? AND tenant_id = ?`, id, tenant))
}

func (s *SQLiteStore) GetReplyTarget(ctx context.Context, id, tenant, recipient string) (DeliveredMessage, error) {
	return scanMessage(s.db.QueryRowContext(ctx, `SELECT m.envelope_json, m.tenant_id, m.sequence, m.accepted_at, d.acknowledged_at FROM messages m JOIN deliveries d ON d.message_id = m.id AND d.recipient_id = ? WHERE m.id = ? AND m.tenant_id = ?`, recipient, id, tenant))
}

func (s *SQLiteStore) CreateMessage(ctx context.Context, e Envelope, tenant string, recipients []string, auditDetail map[string]any) (DeliveredMessage, bool, error) {
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
		`INSERT INTO messages(id, sender_id, recipient_id, space_id, tenant_id, kind, task_id, thread_id, reply_to, envelope_json, accepted_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.SenderID, e.RecipientID, e.SpaceID, tenant, e.Kind, e.TaskID, e.ThreadID, e.ReplyTo, string(encoded), now.Format(time.RFC3339Nano))
	if err != nil {
		return DeliveredMessage{}, false, err
	}
	sequence, err := result.LastInsertId()
	if err != nil {
		return DeliveredMessage{}, false, err
	}
	for _, recipient := range recipients {
		if _, err := tx.ExecContext(ctx, `INSERT INTO deliveries(message_id, recipient_id, tenant_id) VALUES(?, ?, ?)`, e.ID, recipient, tenant); err != nil {
			return DeliveredMessage{}, false, err
		}
	}
	if err := insertAudit(ctx, tx, AuditRecord{Action: "send", ActorID: e.SenderID, SubjectID: e.ID, TenantID: tenant, Outcome: "accepted", Detail: auditDetail}, now); err != nil {
		return DeliveredMessage{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return DeliveredMessage{}, false, err
	}
	return DeliveredMessage{Envelope: e, TenantID: tenant, Sequence: sequence, AcceptedAt: now}, false, nil
}

func (s *SQLiteStore) ListMessages(ctx context.Context, recipient, tenant string, readableSpaces []string, limit int) ([]DeliveredMessage, error) {
	spaceFilter, spaceArgs := readableSpaceFilter(readableSpaces, func(int) string { return "?" })
	args := append([]any{recipient, tenant}, spaceArgs...)
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT m.envelope_json, m.tenant_id, m.sequence, m.accepted_at, d.acknowledged_at
		FROM deliveries d JOIN messages m ON m.id = d.message_id
		WHERE d.recipient_id = ? AND d.tenant_id = ? AND d.acknowledged_at IS NULL AND `+spaceFilter+`
		ORDER BY m.sequence ASC LIMIT ?`, args...)
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

// readableSpaceFilter limits a query on messages m to direct messages and
// group messages in the given spaces. placeholder(i) renders the i-th
// (zero-based) space argument.
func readableSpaceFilter(spaces []string, placeholder func(int) string) (string, []any) {
	if len(spaces) == 0 {
		return "m.space_id = ''", nil
	}
	marks := make([]string, len(spaces))
	args := make([]any, len(spaces))
	for i, space := range spaces {
		marks[i] = placeholder(i)
		args[i] = space
	}
	return "(m.space_id = '' OR m.space_id IN (" + strings.Join(marks, ", ") + "))", args
}

func (s *SQLiteStore) Acknowledge(ctx context.Context, id, agent, tenant string, readableSpaces []string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var messageTenant, space string
	err = tx.QueryRowContext(ctx, `SELECT tenant_id, space_id FROM messages WHERE id = ?`, id).Scan(&messageTenant, &space)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrMessageNotFound
	}
	if err != nil {
		return false, err
	}
	if messageTenant != tenant {
		return false, ErrWrongTenant
	}
	var acknowledged sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT acknowledged_at FROM deliveries WHERE message_id = ? AND recipient_id = ?`, id, agent).Scan(&acknowledged)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotRecipient
	}
	if err != nil {
		return false, err
	}
	if space != "" && !slices.Contains(readableSpaces, space) {
		return false, ErrNotMember
	}
	duplicate := acknowledged.Valid
	if !duplicate {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET acknowledged_at = ? WHERE message_id = ? AND recipient_id = ?`, now, id, agent); err != nil {
			return false, err
		}
		// A direct message keeps its legacy acknowledgement in step, so an
		// older release run after a rollback does not deliver it again.
		if space == "" {
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET acknowledged_at = ? WHERE id = ?`, now, id); err != nil {
				return false, err
			}
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

// sqlExecer is a database or transaction that can run a statement.
type sqlExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func insertAudit(ctx context.Context, exec sqlExecer, record AuditRecord, now time.Time) error {
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
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, d.recipient_id, d.tenant_id, m.space_id, m.sequence, d.attempts, d.escalated_at
		FROM deliveries d JOIN messages m ON m.id = d.message_id
		WHERE d.acknowledged_at IS NULL AND d.disposition IS NULL AND (d.last_notified_at IS NULL OR d.last_notified_at <= ?)
		AND (d.attempts < ? OR d.escalated_at IS NULL) ORDER BY m.sequence ASC, d.recipient_id ASC`, cutoff, maxAttempts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]NotificationCandidate, 0)
	for rows.Next() {
		var c NotificationCandidate
		var escalated sql.NullString
		if err := rows.Scan(&c.MessageID, &c.Recipient, &c.TenantID, &c.SpaceID, &c.Sequence, &c.Attempts, &escalated); err != nil {
			return nil, err
		}
		c.Escalated = escalated.Valid
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) MarkNotified(ctx context.Context, id, recipient string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE deliveries SET attempts = attempts + 1, last_notified_at = ? WHERE message_id = ? AND recipient_id = ?`, now.UTC().Format(time.RFC3339Nano), id, recipient)
	return err
}

func (s *SQLiteStore) MarkEscalated(ctx context.Context, id, recipient string, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var tenant string
	var acknowledged sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT tenant_id, acknowledged_at FROM deliveries WHERE message_id = ? AND recipient_id = ?`, id, recipient).Scan(&tenant, &acknowledged); err != nil {
		return false, err
	}
	if acknowledged.Valid {
		return false, tx.Commit()
	}
	result, err := tx.ExecContext(ctx, `UPDATE deliveries SET escalated_at = ?
		WHERE message_id = ? AND recipient_id = ? AND escalated_at IS NULL AND disposition IS NULL AND acknowledged_at IS NULL`, now.UTC().Format(time.RFC3339Nano), id, recipient)
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
	if err := insertAudit(ctx, tx, escalationAudit(id, recipient, tenant), now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *SQLiteStore) MarkRecipientRevoked(ctx context.Context, id, recipient, reason string, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var tenant string
	var acknowledged sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT tenant_id, acknowledged_at FROM deliveries WHERE message_id = ? AND recipient_id = ?`, id, recipient).Scan(&tenant, &acknowledged); err != nil {
		return false, err
	}
	if acknowledged.Valid {
		return false, tx.Commit()
	}
	result, err := tx.ExecContext(ctx, `UPDATE deliveries SET disposition = ?
		WHERE message_id = ? AND recipient_id = ? AND disposition IS NULL AND escalated_at IS NULL`, DispositionRecipientRevoked, id, recipient)
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
	if err := insertAudit(ctx, tx, recipientRevokedAudit(id, recipient, tenant, reason), now); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func recipientRevokedAudit(id, recipient, tenant, reason string) AuditRecord {
	return AuditRecord{Action: "delivery.recipient_revoked", ActorID: "system", SubjectID: id, TenantID: tenant, Outcome: "attempted", Code: reason, Detail: map[string]any{"recipient_id": recipient}}
}

func escalationAudit(id, recipient, tenant string) AuditRecord {
	return AuditRecord{Action: "message.unacknowledged_escalation", ActorID: "system", SubjectID: id, TenantID: tenant, Outcome: "attempted", Detail: map[string]any{"recipient_id": recipient}}
}

func (s *SQLiteStore) RegistryState(ctx context.Context) (RecordedRegistry, error) {
	return scanRecordedRegistry(s.db.QueryRowContext(ctx, `SELECT sha256, agents_json, spaces_json FROM registry_state WHERE id = 1`))
}

func scanRecordedRegistry(row rowScanner) (RecordedRegistry, error) {
	var sha, agents string
	var spaces sql.NullString
	err := row.Scan(&sha, &agents, &spaces)
	if errors.Is(err, sql.ErrNoRows) {
		return RecordedRegistry{}, nil
	}
	if err != nil {
		return RecordedRegistry{}, err
	}
	recorded := RecordedRegistry{SHA256: sha, AgentsJSON: []byte(agents)}
	if spaces.Valid {
		recorded.SpacesJSON = []byte(spaces.String)
	}
	return recorded, nil
}

func (s *SQLiteStore) RecordRegistryChange(ctx context.Context, change RegistryChange) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	observed := change.ObservedAt.UTC().Format(time.RFC3339Nano)
	var result sql.Result
	if change.PreviousSHA256 == "" {
		result, err = tx.ExecContext(ctx, `INSERT INTO registry_state(id, sha256, agents_json, spaces_json, observed_at) VALUES(1, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`, change.SHA256, string(change.AgentsJSON), string(change.SpacesJSON), observed)
	} else {
		result, err = tx.ExecContext(ctx, `UPDATE registry_state SET sha256 = ?, agents_json = ?, spaces_json = ?, observed_at = ? WHERE id = 1 AND sha256 = ?`, change.SHA256, string(change.AgentsJSON), string(change.SpacesJSON), observed, change.PreviousSHA256)
	}
	if err != nil {
		return false, err
	}
	if updated, err := result.RowsAffected(); err != nil || updated == 0 {
		return false, err
	}
	if err := insertAudit(ctx, tx, change.Audit, change.ObservedAt); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *SQLiteStore) PutNoteRevision(ctx context.Context, w NoteWrite) (NoteWriteResult, error) {
	return sqliteNotes.put(ctx, s.db, w)
}

func (s *SQLiteStore) GetNoteRevision(ctx context.Context, tenant, space, note string, revision int64) (StoredNoteRevision, error) {
	return sqliteNotes.get(ctx, s.db, tenant, space, note, revision)
}

func (s *SQLiteStore) ListNoteRevisions(ctx context.Context, tenant, space, note string, limit int) ([]StoredNoteRevision, error) {
	return sqliteNotes.list(ctx, s.db, tenant, space, note, limit)
}

const artifactColumns = `tenant_id, space_id, sha256, size, media_type, state, uploaded_by, created_at, stored_at`

func (s *SQLiteStore) GetArtifact(ctx context.Context, tenant, space, sha string) (ArtifactRecord, error) {
	return scanArtifact(s.db.QueryRowContext(ctx, `SELECT `+artifactColumns+` FROM artifacts WHERE tenant_id = ? AND space_id = ? AND sha256 = ?`, tenant, space, sha))
}

func (s *SQLiteStore) GrantArtifactUpload(ctx context.Context, record ArtifactRecord, audit AuditRecord) (ArtifactRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ArtifactRecord{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO artifacts(tenant_id, space_id, sha256, size, media_type, state, uploaded_by, created_at)
		VALUES(?, ?, ?, ?, ?, 'pending', ?, ?)
		ON CONFLICT(tenant_id, space_id, sha256) DO UPDATE SET size = excluded.size, media_type = excluded.media_type, uploaded_by = excluded.uploaded_by
		WHERE artifacts.state = 'pending'`,
		record.TenantID, record.SpaceID, record.SHA256, record.Size, record.MediaType, record.UploadedBy, record.CreatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return ArtifactRecord{}, err
	}
	if updated, err := result.RowsAffected(); err != nil {
		return ArtifactRecord{}, err
	} else if updated == 1 {
		if err := insertAudit(ctx, tx, audit, record.CreatedAt); err != nil {
			return ArtifactRecord{}, err
		}
	}
	stored, err := scanArtifact(tx.QueryRowContext(ctx, `SELECT `+artifactColumns+` FROM artifacts WHERE tenant_id = ? AND space_id = ? AND sha256 = ?`, record.TenantID, record.SpaceID, record.SHA256))
	if err != nil {
		return ArtifactRecord{}, err
	}
	return stored, tx.Commit()
}

func (s *SQLiteStore) MarkArtifactStored(ctx context.Context, tenant, space, sha string, now time.Time, audit AuditRecord) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE artifacts SET state = 'available', stored_at = ? WHERE tenant_id = ? AND space_id = ? AND sha256 = ? AND state = 'pending'`,
		now.UTC().Format(time.RFC3339Nano), tenant, space, sha)
	if err != nil {
		return false, err
	}
	if updated, err := result.RowsAffected(); err != nil || updated == 0 {
		return false, err
	}
	if err := insertAudit(ctx, tx, audit, now); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *SQLiteStore) DiscardArtifactUpload(ctx context.Context, tenant, space, sha string, audit AuditRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM artifacts WHERE tenant_id = ? AND space_id = ? AND sha256 = ? AND state = 'pending'`, tenant, space, sha); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, audit, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

func scanArtifact(row rowScanner) (ArtifactRecord, error) {
	var record ArtifactRecord
	var created, stored any
	if err := row.Scan(&record.TenantID, &record.SpaceID, &record.SHA256, &record.Size, &record.MediaType, &record.State, &record.UploadedBy, &created, &stored); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ArtifactRecord{}, ErrArtifactNotFound
		}
		return ArtifactRecord{}, err
	}
	var err error
	if record.CreatedAt, err = parseDatabaseTime(created); err != nil {
		return ArtifactRecord{}, err
	}
	if stored != nil {
		parsed, err := parseDatabaseTime(stored)
		if err != nil {
			return ArtifactRecord{}, err
		}
		record.StoredAt = &parsed
	}
	return record, nil
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
	if len(m.Envelope.Payload) > 0 {
		if err := validateJSONMembers(m.Envelope.Payload); err != nil {
			return DeliveredMessage{}, fmt.Errorf("decode stored envelope payload: %w", err)
		}
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
