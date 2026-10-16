package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrNoteNotFound           = errors.New("note or revision not found")
	ErrNoteRevisionIDConflict = errors.New("revision id already exists with different content")
)

// NoteConflictError reports that a write's base revision is not the note's
// head. The head fields are empty when the note does not exist.
type NoteConflictError struct {
	HeadRevision int64
	HeadSHA256   string
	HeadAuthor   string
}

func (e *NoteConflictError) Error() string {
	return fmt.Sprintf("note head is revision %d", e.HeadRevision)
}

// NoteWrite is one proposed revision and what its acceptance writes in the
// same transaction: the audit row and, when Recipients is not empty, one
// notice delivered to each of them.
type NoteWrite struct {
	Revision   NoteRevision
	TenantID   string
	Notice     Envelope
	Recipients []string
	Audit      AuditRecord
}

// NoteWriteResult is the stored revision. Notice is set only when this call
// created it; Duplicate reports an identical retry of a stored revision.
type NoteWriteResult struct {
	Revision  StoredNoteRevision
	Notice    *DeliveredMessage
	Duplicate bool
}

// noteDialect is what differs between the backends for the note queries.
type noteDialect struct {
	rebind          func(string) string
	timestamp       func(time.Time) any
	audit           func(context.Context, sqlExecer, AuditRecord, time.Time) error
	uniqueViolation func(error) bool
}

var sqliteNotes = noteDialect{
	rebind:    func(query string) string { return query },
	timestamp: func(t time.Time) any { return t.UTC().Format(time.RFC3339Nano) },
	audit:     insertAudit,
	uniqueViolation: func(err error) bool {
		return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique constraint")
	},
}

var postgresNotes = noteDialect{
	rebind:          dollarPlaceholders,
	timestamp:       func(t time.Time) any { return t.UTC() },
	audit:           insertPostgresAudit,
	uniqueViolation: isPostgresUniqueViolation,
}

// dollarPlaceholders numbers each ? in query as $1, $2, ... for PostgreSQL.
// The note queries contain no ? inside literals.
func dollarPlaceholders(query string) string {
	var out strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			fmt.Fprintf(&out, "$%d", n)
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

const noteRevisionColumns = `tenant_id, space_id, note_id, revision, base_revision, revision_id, author_id, key_id, created_at, media_type, sha256, body, signature, accepted_at`

func (d noteDialect) put(ctx context.Context, db *sql.DB, w NoteWrite) (NoteWriteResult, error) {
	n := w.Revision
	// PostgreSQL keeps microseconds; the write's answer matches later reads.
	now := time.Now().UTC().Truncate(time.Microsecond)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return NoteWriteResult{}, err
	}
	defer tx.Rollback()
	if result, found, err := d.retry(ctx, tx, w, now); found || err != nil {
		return result, err
	}
	var advanced sql.Result
	if n.BaseRevision == 0 {
		advanced, err = tx.ExecContext(ctx, d.rebind(`INSERT INTO notes(tenant_id, space_id, note_id, head_revision, created_by, created_at) VALUES(?, ?, ?, 1, ?, ?)
			ON CONFLICT(tenant_id, space_id, note_id) DO NOTHING`), w.TenantID, n.SpaceID, n.NoteID, n.AuthorID, d.timestamp(now))
	} else {
		advanced, err = tx.ExecContext(ctx, d.rebind(`UPDATE notes SET head_revision = ? WHERE tenant_id = ? AND space_id = ? AND note_id = ? AND head_revision = ?`),
			n.BaseRevision+1, w.TenantID, n.SpaceID, n.NoteID, n.BaseRevision)
	}
	if err != nil {
		return NoteWriteResult{}, err
	}
	if updated, err := advanced.RowsAffected(); err != nil {
		return NoteWriteResult{}, err
	} else if updated == 0 {
		// The head moved. A concurrent identical retry may be what moved it.
		if result, found, err := d.retry(ctx, tx, w, now); found || err != nil {
			return result, err
		}
		head, err := scanNoteRevision(tx.QueryRowContext(ctx, d.rebind(`SELECT `+noteRevisionColumns+` FROM note_revisions
			WHERE tenant_id = ? AND space_id = ? AND note_id = ? ORDER BY revision DESC LIMIT 1`), w.TenantID, n.SpaceID, n.NoteID))
		if errors.Is(err, ErrNoteNotFound) {
			return NoteWriteResult{}, &NoteConflictError{}
		}
		if err != nil {
			return NoteWriteResult{}, err
		}
		return NoteWriteResult{}, &NoteConflictError{HeadRevision: head.Revision, HeadSHA256: head.SHA256, HeadAuthor: head.AuthorID}
	}
	stored := StoredNoteRevision{NoteRevision: n, TenantID: w.TenantID, Revision: n.BaseRevision + 1, AcceptedAt: now}
	if _, err := tx.ExecContext(ctx, d.rebind(`INSERT INTO note_revisions(`+noteRevisionColumns+`) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		w.TenantID, n.SpaceID, n.NoteID, stored.Revision, n.BaseRevision, n.RevisionID, n.AuthorID, n.KeyID, n.CreatedAt, n.MediaType, n.SHA256, n.Body, n.Signature, d.timestamp(now)); err != nil {
		if d.uniqueViolation(err) {
			return NoteWriteResult{}, ErrNoteRevisionIDConflict
		}
		return NoteWriteResult{}, err
	}
	result := NoteWriteResult{Revision: stored}
	if len(w.Recipients) > 0 {
		notice, err := d.insertNotice(ctx, tx, w, now)
		if err != nil {
			return NoteWriteResult{}, err
		}
		result.Notice = &notice
	}
	if err := d.audit(ctx, tx, w.Audit, now); err != nil {
		return NoteWriteResult{}, err
	}
	return result, tx.Commit()
}

// retry returns the stored revision when w repeats one: the same revision ID
// with identical signed content apart from created_at and key_id. Reusing the
// ID for anything else is ErrNoteRevisionIDConflict.
func (d noteDialect) retry(ctx context.Context, tx *sql.Tx, w NoteWrite, now time.Time) (NoteWriteResult, bool, error) {
	stored, err := scanNoteRevision(tx.QueryRowContext(ctx, d.rebind(`SELECT `+noteRevisionColumns+` FROM note_revisions WHERE revision_id = ?`), w.Revision.RevisionID))
	if errors.Is(err, ErrNoteNotFound) {
		return NoteWriteResult{}, false, nil
	}
	if err != nil {
		return NoteWriteResult{}, true, err
	}
	storedBytes, storedErr := noteRetryBytes(stored.NoteRevision)
	retryBytes, retryErr := noteRetryBytes(w.Revision)
	if stored.TenantID != w.TenantID || storedErr != nil || retryErr != nil || string(storedBytes) != string(retryBytes) {
		return NoteWriteResult{}, true, ErrNoteRevisionIDConflict
	}
	audit := AuditRecord{Action: "note.revision", ActorID: w.Revision.AuthorID, SubjectID: NoteSubject(stored.SpaceID, stored.NoteID, stored.Revision), TenantID: w.TenantID, Outcome: "duplicate", Code: "duplicate_revision"}
	if err := d.audit(ctx, tx, audit, now); err != nil {
		return NoteWriteResult{}, true, err
	}
	return NoteWriteResult{Revision: stored, Duplicate: true}, true, tx.Commit()
}

// insertNotice stores the server-authored notice as a group message in the
// note's space with one delivery per recipient, so it is polled, rung,
// acknowledged, and escalated like any other delivery.
func (d noteDialect) insertNotice(ctx context.Context, tx *sql.Tx, w NoteWrite, now time.Time) (DeliveredMessage, error) {
	e := w.Notice
	encoded, err := json.Marshal(e)
	if err != nil {
		return DeliveredMessage{}, err
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, d.rebind(`INSERT INTO messages(id, sender_id, recipient_id, space_id, tenant_id, kind, task_id, thread_id, reply_to, envelope_json, accepted_at)
		VALUES(?, '', '', ?, ?, ?, '', '', '', ?, ?) RETURNING sequence`),
		e.ID, e.SpaceID, w.TenantID, e.Kind, string(encoded), d.timestamp(now)).Scan(&sequence); err != nil {
		return DeliveredMessage{}, err
	}
	for _, recipient := range w.Recipients {
		if _, err := tx.ExecContext(ctx, d.rebind(`INSERT INTO deliveries(message_id, recipient_id, tenant_id) VALUES(?, ?, ?)`), e.ID, recipient, w.TenantID); err != nil {
			return DeliveredMessage{}, err
		}
	}
	return DeliveredMessage{Envelope: e, TenantID: w.TenantID, Sequence: sequence, AcceptedAt: now}, nil
}

// get returns one revision of a note, or its head when revision is 0.
func (d noteDialect) get(ctx context.Context, db *sql.DB, tenant, space, note string, revision int64) (StoredNoteRevision, error) {
	if revision == 0 {
		return scanNoteRevision(db.QueryRowContext(ctx, d.rebind(`SELECT `+noteRevisionColumns+` FROM note_revisions
			WHERE tenant_id = ? AND space_id = ? AND note_id = ? ORDER BY revision DESC LIMIT 1`), tenant, space, note))
	}
	return scanNoteRevision(db.QueryRowContext(ctx, d.rebind(`SELECT `+noteRevisionColumns+` FROM note_revisions
		WHERE tenant_id = ? AND space_id = ? AND note_id = ? AND revision = ?`), tenant, space, note, revision))
}

// list returns a note's newest revisions first, or ErrNoteNotFound.
func (d noteDialect) list(ctx context.Context, db *sql.DB, tenant, space, note string, limit int) ([]StoredNoteRevision, error) {
	rows, err := db.QueryContext(ctx, d.rebind(`SELECT `+noteRevisionColumns+` FROM note_revisions
		WHERE tenant_id = ? AND space_id = ? AND note_id = ? ORDER BY revision DESC LIMIT ?`), tenant, space, note, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	revisions := make([]StoredNoteRevision, 0)
	for rows.Next() {
		revision, err := scanNoteRevision(rows)
		if err != nil {
			return nil, err
		}
		revisions = append(revisions, revision)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(revisions) == 0 {
		return nil, ErrNoteNotFound
	}
	return revisions, nil
}

func scanNoteRevision(row rowScanner) (StoredNoteRevision, error) {
	var r StoredNoteRevision
	var accepted any
	err := row.Scan(&r.TenantID, &r.SpaceID, &r.NoteID, &r.Revision, &r.BaseRevision, &r.RevisionID, &r.AuthorID, &r.KeyID, &r.CreatedAt, &r.MediaType, &r.SHA256, &r.Body, &r.Signature, &accepted)
	if errors.Is(err, sql.ErrNoRows) {
		return StoredNoteRevision{}, ErrNoteNotFound
	}
	if err != nil {
		return StoredNoteRevision{}, err
	}
	r.Type = NoteRevisionType
	r.ContentIsData = true
	r.AcceptedAt, err = parseDatabaseTime(accepted)
	return r, err
}

// NoteSubject names a note, or one of its revisions when revision is
// positive, as an audit subject.
func NoteSubject(space, note string, revision int64) string {
	subject := space + "/" + note
	if revision > 0 {
		subject += fmt.Sprintf("@%d", revision)
	}
	return subject
}
