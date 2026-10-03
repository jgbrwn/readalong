package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"time"
)

type DB struct{ *sql.DB }

var (
	ErrBookNotFound            = errors.New("book not found")
	ErrRetranscriptionNotReady = errors.New("book is not ready for re-transcription")
	ErrBookJobInProgress       = errors.New("book already has a job in progress")
	ErrBookOperationInProgress = errors.New("book is being copied or transferred")
	ErrBookCoverInProgress     = errors.New("book cover is being updated")
	ErrBookDeleteInProgress    = errors.New("book is being deleted")
)

type Book struct {
	ID                      string   `json:"id"`
	Title                   string   `json:"title"`
	Author                  string   `json:"author"`
	SourceKind              string   `json:"source_kind"`
	Mode                    string   `json:"mode"`
	GutenbergID             string   `json:"gutenberg_id,omitempty"`
	EbookSourceURL          string   `json:"ebook_source_url,omitempty"`
	AlignmentQuality        *float64 `json:"alignment_quality,omitempty"`
	Status                  string   `json:"status"`
	JobStatus               string   `json:"job_status,omitempty"`
	DurationMS              int64    `json:"duration_ms"`
	Stage                   string   `json:"stage,omitempty"`
	Progress                float64  `json:"progress,omitempty"`
	Error                   string   `json:"error,omitempty"`
	PositionMS              int64    `json:"position_ms,omitempty"`
	PlaybackRate            float64  `json:"playback_rate,omitempty"`
	SyncOffsetMS            int64    `json:"sync_offset_ms,omitempty"`
	AppearanceJSON          string   `json:"appearance_json,omitempty"`
	CoverStatus             string   `json:"cover_status,omitempty"`
	CoverRegenerationQueued bool     `json:"cover_regeneration_queued,omitempty"`
	CoverKind               string   `json:"cover_kind,omitempty"`
	CoverProvider           string   `json:"cover_provider,omitempty"`
	CoverURL                string   `json:"cover_url,omitempty"`
	CoverCandidateURL       string   `json:"cover_candidate_url,omitempty"`
	CoverCandidateProvider  string   `json:"cover_candidate_provider,omitempty"`
	CoverCandidateYear      int      `json:"cover_candidate_year,omitempty"`
	CoverAICandidateURL     string   `json:"cover_ai_candidate_url,omitempty"`
	CoverAICandidateYear    int      `json:"cover_ai_candidate_year,omitempty"`
	CoverReviewNeeded       bool     `json:"cover_review_needed,omitempty"`
	CoverYear               int      `json:"cover_year,omitempty"`
	CreatedAt               string   `json:"created_at"`
	UpdatedAt               string   `json:"updated_at"`
	OwnerUserID             string   `json:"-"`
	SourceURL               string   `json:"-"`
	AudioRelPath            string   `json:"-"`
	EpubRelPath             string   `json:"-"`
	EbookJSONRelPath        string   `json:"-"`
	AlignmentRelPath        string   `json:"-"`
	TranscriptRelPath       string   `json:"-"`
}

type NewBook struct {
	ID, OwnerUserID, Title, Author, SourceKind, SourceURL, AudioRelPath, EpubRelPath string
	EbookSourceURL, GutenbergID, JobID                                               string
}

type Job struct {
	ID, OwnerUserID, BookID, Kind, Status, Stage, Error, NotBeforeAt string
	Progress                                                         float64
	Attempt                                                          int
}

type Chapter struct {
	ID      string `json:"id"`
	BookID  string `json:"book_id"`
	Ordinal int    `json:"ordinal"`
	Title   string `json:"title"`
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
}

type Chunk struct {
	ID              string
	BookID          string
	Ordinal         int
	StartMS         int64
	EndMS           int64
	Status          string
	AudioRelPath    string
	ResponseRelPath string
	NotBeforeAt     string
	Error           string
}

type User struct {
	ID         string `json:"id"`
	Email      string `json:"email"`
	Role       string `json:"role"`
	Status     string `json:"status"`
	BookCount  int64  `json:"book_count"`
	CreatedAt  string `json:"created_at"`
	LastSeenAt string `json:"last_seen_at"`
}

func Open(dataDir string) (*DB, error) {
	if err := os.MkdirAll(dataDir, 0750); err != nil {
		return nil, err
	}
	p := filepath.Join(dataDir, "app.db")
	s, err := sql.Open("sqlite", "file:"+p+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	d := &DB{s}
	if err = d.migrate(); err != nil {
		s.Close()
		return nil, err
	}
	return d, nil
}

func (d *DB) migrate() error {
	schema := `
CREATE TABLE IF NOT EXISTS users (
 id TEXT PRIMARY KEY, email TEXT NOT NULL, role TEXT NOT NULL DEFAULT 'user',
 status TEXT NOT NULL DEFAULT 'active', created_at TEXT NOT NULL, last_seen_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS admin_bootstrap_claims (
 email TEXT PRIMARY KEY, user_id TEXT NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
 claimed_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS books (
 id TEXT PRIMARY KEY, owner_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 title TEXT NOT NULL DEFAULT '', author TEXT NOT NULL DEFAULT '', source_kind TEXT NOT NULL DEFAULT '', source_url TEXT,
 mode TEXT NOT NULL DEFAULT 'transcript', status TEXT NOT NULL DEFAULT 'queued', duration_ms INTEGER NOT NULL DEFAULT 0,
 audio_relpath TEXT, epub_relpath TEXT, ebook_json_relpath TEXT, transcript_relpath TEXT,
 alignment_relpath TEXT, alignment_quality REAL, gutenberg_id TEXT, ebook_source_url TEXT,
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS books_owner_created ON books(owner_user_id, created_at DESC);
CREATE TABLE IF NOT EXISTS chapters (
 id TEXT PRIMARY KEY, book_id TEXT NOT NULL REFERENCES books(id) ON DELETE CASCADE, ordinal INTEGER NOT NULL,
 title TEXT NOT NULL DEFAULT '', start_ms INTEGER NOT NULL DEFAULT 0, end_ms INTEGER NOT NULL DEFAULT 0,
 transcript_state TEXT NOT NULL DEFAULT 'pending', alignment_quality REAL
);
CREATE TABLE IF NOT EXISTS jobs (
 id TEXT PRIMARY KEY, owner_user_id TEXT NOT NULL, book_id TEXT NOT NULL REFERENCES books(id) ON DELETE CASCADE,
 kind TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'queued', stage TEXT NOT NULL DEFAULT '', progress REAL NOT NULL DEFAULT 0,
 attempt INTEGER NOT NULL DEFAULT 0, error TEXT, not_before_at TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS jobs_status ON jobs(status, not_before_at, created_at);
CREATE INDEX IF NOT EXISTS jobs_book_created ON jobs(book_id, created_at DESC);
CREATE TABLE IF NOT EXISTS chunks (
 id TEXT PRIMARY KEY, book_id TEXT NOT NULL REFERENCES books(id) ON DELETE CASCADE,
 ordinal INTEGER NOT NULL, start_ms INTEGER NOT NULL, end_ms INTEGER NOT NULL,
 status TEXT NOT NULL DEFAULT 'queued', audio_relpath TEXT NOT NULL DEFAULT '',
 response_relpath TEXT NOT NULL DEFAULT '', not_before_at TEXT, error TEXT,
 UNIQUE(book_id, ordinal)
);
CREATE INDEX IF NOT EXISTS chunks_book_order ON chunks(book_id, ordinal);
CREATE TABLE IF NOT EXISTS reading_progress (
 user_id TEXT NOT NULL, book_id TEXT NOT NULL REFERENCES books(id) ON DELETE CASCADE,
 position_ms INTEGER NOT NULL DEFAULT 0, playback_rate REAL NOT NULL DEFAULT 1.0, sync_offset_ms INTEGER NOT NULL DEFAULT 0,
 appearance_json TEXT NOT NULL DEFAULT '{}', updated_at TEXT NOT NULL,
 PRIMARY KEY(user_id, book_id)
);
CREATE TABLE IF NOT EXISTS book_cover_state (
 book_id TEXT PRIMARY KEY REFERENCES books(id) ON DELETE CASCADE,
 status TEXT NOT NULL DEFAULT 'pending',
 selected_kind TEXT NOT NULL DEFAULT '', selected_relpath TEXT, selected_url TEXT,
 selected_provider TEXT, selected_year INTEGER NOT NULL DEFAULT 0,
 candidate_kind TEXT NOT NULL DEFAULT '', candidate_relpath TEXT, candidate_url TEXT,
	candidate_provider TEXT, candidate_year INTEGER NOT NULL DEFAULT 0,
	ai_candidate_relpath TEXT, ai_candidate_year INTEGER NOT NULL DEFAULT 0,
	last_checked_at TEXT, next_check_at TEXT, no_match_count INTEGER NOT NULL DEFAULT 0,
	failure_count INTEGER NOT NULL DEFAULT 0, lookup_paused INTEGER NOT NULL DEFAULT 0,
	local_scan_needed INTEGER NOT NULL DEFAULT 1, deleting INTEGER NOT NULL DEFAULT 0,
	regeneration_requested INTEGER NOT NULL DEFAULT 0,
	lease_until TEXT, updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS book_cover_due ON book_cover_state(lookup_paused,next_check_at,lease_until);
CREATE TABLE IF NOT EXISTS cover_lookup_cache (
 cache_key TEXT PRIMARY KEY, result_json TEXT NOT NULL DEFAULT '', found INTEGER NOT NULL DEFAULT 0,
 no_match_count INTEGER NOT NULL DEFAULT 0, checked_at TEXT NOT NULL, next_check_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS cover_provider_usage (
 provider TEXT NOT NULL, usage_day TEXT NOT NULL, request_count INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(provider,usage_day)
);
CREATE TABLE IF NOT EXISTS app_settings (
 key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS admin_book_operations (
 id TEXT PRIMARY KEY, actor_user_id TEXT NOT NULL, mode TEXT NOT NULL, book_id TEXT NOT NULL,
 source_owner_user_id TEXT NOT NULL, target_owner_user_id TEXT NOT NULL, target_book_id TEXT NOT NULL,
 status TEXT NOT NULL DEFAULT 'queued', stage TEXT NOT NULL DEFAULT 'queued',
 copied_bytes INTEGER NOT NULL DEFAULT 0, total_bytes INTEGER NOT NULL DEFAULT 0,
 error TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS admin_book_operations_active
 ON admin_book_operations(book_id) WHERE status IN ('queued','running');
CREATE INDEX IF NOT EXISTS admin_book_operations_queue ON admin_book_operations(status,created_at);`
	_, err := d.Exec(schema)
	if err != nil {
		return err
	}
	if err := d.ensureColumn("book_cover_state", "deleting", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := d.ensureColumn("book_cover_state", "local_scan_needed", "INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	if err := d.ensureColumn("book_cover_state", "ai_candidate_relpath", "TEXT"); err != nil {
		return err
	}
	if err := d.ensureColumn("book_cover_state", "ai_candidate_year", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := d.ensureColumn("book_cover_state", "regeneration_requested", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := d.Exec(`INSERT OR IGNORE INTO book_cover_state(book_id,status,next_check_at,updated_at)
		SELECT id,'pending',?,? FROM books`, now, now); err != nil {
		return err
	}
	// Upgrade databases created by earlier scaffold revisions.
	for _, col := range []struct{ name, definition string }{
		{"role", "TEXT NOT NULL DEFAULT 'user'"},
		{"status", "TEXT NOT NULL DEFAULT 'active'"},
	} {
		if err := d.ensureColumn("users", col.name, col.definition); err != nil {
			return err
		}
	}
	for _, col := range []struct{ name, definition string }{
		{"epub_relpath", "TEXT"},
		{"ebook_json_relpath", "TEXT"},
		{"alignment_relpath", "TEXT"},
		{"alignment_quality", "REAL"},
		{"gutenberg_id", "TEXT"},
		{"ebook_source_url", "TEXT"},
	} {
		if err := d.ensureColumn("books", col.name, col.definition); err != nil {
			return err
		}
	}
	return nil
}

type Setting struct {
	Value     string
	UpdatedAt string
}

func (d *DB) AppSetting(ctx context.Context, key string) (Setting, bool, error) {
	var setting Setting
	err := d.QueryRowContext(ctx, `SELECT value,updated_at FROM app_settings WHERE key=?`, key).
		Scan(&setting.Value, &setting.UpdatedAt)
	if err == sql.ErrNoRows {
		return Setting{}, false, nil
	}
	return setting, err == nil, err
}

func (d *DB) SetAppSetting(ctx context.Context, key, value string) error {
	if key == "" || len(key) > 100 || len(value) > 2<<20 {
		return fmt.Errorf("invalid application setting")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.ExecContext(ctx, `INSERT INTO app_settings(key,value,updated_at) VALUES(?,?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`,
		key, value, now)
	return err
}

func (d *DB) ensureColumn(table, name, definition string) error {
	rows, err := d.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var col, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &col, &typ, &notnull, &defaultValue, &pk); err != nil {
			return err
		}
		if col == name {
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = d.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + name + ` ` + definition)
	return err
}

// UpsertUser provisions every identity authenticated by the exe.dev proxy.
// Bootstrap-email admin access is claimed once and then bound to the stable
// exe.dev user ID, so a later email change cannot transfer the role.
func (d *DB) UpsertUser(ctx context.Context, id, email string, configuredAdmin, bootstrapAdmin bool) (User, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO users(id,email,created_at,last_seen_at) VALUES(?,?,?,?)
      ON CONFLICT(id) DO UPDATE SET email=excluded.email,last_seen_at=excluded.last_seen_at`, id, email, now, now)
	if err != nil {
		return User{}, err
	}
	if bootstrapAdmin {
		_, err = tx.ExecContext(ctx, `INSERT INTO admin_bootstrap_claims(email,user_id,claimed_at)
			VALUES(?,?,?) ON CONFLICT(email) DO NOTHING`, email, id, now)
		if err != nil {
			return User{}, err
		}
	}
	var claimed int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_bootstrap_claims WHERE user_id=?`, id).Scan(&claimed)
	if err != nil {
		return User{}, err
	}
	role := "user"
	if configuredAdmin || claimed > 0 {
		role = "admin"
	}
	_, err = tx.ExecContext(ctx, `UPDATE users SET role=? WHERE id=?`, role, id)
	if err != nil {
		return User{}, err
	}
	var u User
	err = tx.QueryRowContext(ctx, `SELECT id,email,role,status,created_at,last_seen_at
		FROM users WHERE id=?`, id).Scan(&u.ID, &u.Email, &u.Role, &u.Status, &u.CreatedAt, &u.LastSeenAt)
	if err != nil {
		return User{}, err
	}
	if err = tx.Commit(); err != nil {
		return User{}, err
	}
	return u, nil
}

func (d *DB) BooksForUser(ctx context.Context, uid string) ([]Book, error) {
	return d.SearchBooksForUser(ctx, uid, "")
}

func (d *DB) SearchBooksForUser(ctx context.Context, uid, query string) ([]Book, error) {
	return d.SearchBooksForUserSorted(ctx, uid, query, "recent")
}

func (d *DB) SearchBooksForUserSorted(ctx context.Context, uid, query, sortBy string) ([]Book, error) {
	orderBy, ok := map[string]string{
		"recent":   "b.created_at DESC,b.id",
		"title":    "lower(b.title),lower(b.author),b.id",
		"author":   "CASE WHEN trim(b.author)='' THEN 1 ELSE 0 END,lower(b.author),lower(b.title),b.id",
		"lastread": "CASE WHEN p.updated_at IS NULL THEN 1 ELSE 0 END,p.updated_at DESC,b.created_at DESC,b.id",
		"progress": "CASE WHEN b.duration_ms>0 THEN min(1.0,max(0.0,COALESCE(p.position_ms,0)*1.0/b.duration_ms)) ELSE 0 END DESC,b.created_at DESC,b.id",
	}[sortBy]
	if !ok {
		orderBy = "b.created_at DESC,b.id"
	}
	rows, err := d.QueryContext(ctx, `SELECT `+bookSelectColumns+`
		WHERE b.owner_user_id=? AND (?='' OR instr(lower(b.title),lower(?))>0 OR instr(lower(b.author),lower(?))>0)
			AND NOT EXISTS(SELECT 1 FROM admin_book_operations op WHERE op.target_book_id=b.id
				AND op.status IN ('queued','running')
				AND (op.mode='clone' OR b.owner_user_id=op.target_owner_user_id))
		ORDER BY `+orderBy, uid, query, query, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Book
	for rows.Next() {
		b, err := scanBook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

const bookSelectColumns = `b.id,b.title,b.author,b.source_kind,b.mode,b.status,b.duration_ms,
	COALESCE(j.status,''),COALESCE(j.stage,''),COALESCE(j.progress,0),COALESCE(j.error,''),
	COALESCE(p.position_ms,0),COALESCE(p.playback_rate,1),COALESCE(p.sync_offset_ms,0),
	COALESCE(p.appearance_json,'{}'),
	b.created_at,b.updated_at,b.owner_user_id,COALESCE(b.source_url,''),COALESCE(b.audio_relpath,''),
	COALESCE(b.epub_relpath,''),COALESCE(b.ebook_json_relpath,''),COALESCE(b.transcript_relpath,''),
	COALESCE(b.alignment_relpath,''),b.alignment_quality,COALESCE(b.gutenberg_id,''),
	COALESCE(b.ebook_source_url,''),COALESCE(cs.status,'pending'),COALESCE(cs.regeneration_requested,0),
	COALESCE(cs.selected_kind,''),
	CASE WHEN COALESCE(cs.selected_url,'')<>'' THEN cs.selected_url
		WHEN COALESCE(cs.selected_relpath,'')<>'' THEN '/api/books/'||b.id||'/cover/selected' ELSE '' END,
	CASE WHEN COALESCE(cs.candidate_url,'')<>'' THEN cs.candidate_url
		WHEN COALESCE(cs.candidate_relpath,'')<>'' THEN '/api/books/'||b.id||'/cover/candidate' ELSE '' END,
	CASE WHEN COALESCE(cs.ai_candidate_relpath,'')<>'' THEN '/api/books/'||b.id||'/cover/ai-candidate' ELSE '' END,
	COALESCE(cs.ai_candidate_year,0),
	CASE WHEN COALESCE(cs.candidate_url,'')<>'' OR COALESCE(cs.candidate_relpath,'')<>'' OR
		COALESCE(cs.ai_candidate_relpath,'')<>'' THEN 1 ELSE 0 END,
	COALESCE(cs.selected_year,0),COALESCE(cs.selected_provider,''),COALESCE(cs.candidate_provider,''),
	COALESCE(cs.candidate_year,0)
	FROM books b
	LEFT JOIN jobs j ON j.id=(SELECT j2.id FROM jobs j2 WHERE j2.book_id=b.id ORDER BY j2.created_at DESC,j2.rowid DESC LIMIT 1)
	LEFT JOIN reading_progress p ON p.book_id=b.id AND p.user_id=b.owner_user_id
	LEFT JOIN book_cover_state cs ON cs.book_id=b.id`

type rowScanner interface {
	Scan(...any) error
}

func scanBook(row rowScanner) (Book, error) {
	var b Book
	var alignmentQuality sql.NullFloat64
	err := row.Scan(&b.ID, &b.Title, &b.Author, &b.SourceKind, &b.Mode, &b.Status, &b.DurationMS,
		&b.JobStatus, &b.Stage, &b.Progress, &b.Error, &b.PositionMS, &b.PlaybackRate, &b.SyncOffsetMS,
		&b.AppearanceJSON, &b.CreatedAt, &b.UpdatedAt, &b.OwnerUserID, &b.SourceURL, &b.AudioRelPath,
		&b.EpubRelPath, &b.EbookJSONRelPath, &b.TranscriptRelPath, &b.AlignmentRelPath,
		&alignmentQuality, &b.GutenbergID, &b.EbookSourceURL, &b.CoverStatus,
		&b.CoverRegenerationQueued, &b.CoverKind,
		&b.CoverURL, &b.CoverCandidateURL, &b.CoverAICandidateURL, &b.CoverAICandidateYear,
		&b.CoverReviewNeeded, &b.CoverYear,
		&b.CoverProvider, &b.CoverCandidateProvider, &b.CoverCandidateYear)
	if alignmentQuality.Valid {
		b.AlignmentQuality = &alignmentQuality.Float64
	}
	return b, err
}

func (d *DB) BookForUser(ctx context.Context, uid, bid string) (Book, error) {
	b, err := scanBook(d.QueryRowContext(ctx, `SELECT `+bookSelectColumns+`
		WHERE b.owner_user_id=? AND b.id=?
		AND NOT EXISTS(SELECT 1 FROM admin_book_operations op WHERE op.target_book_id=b.id
			AND op.status IN ('queued','running')
			AND (op.mode='clone' OR b.owner_user_id=op.target_owner_user_id))`, uid, bid))
	if err != nil {
		return Book{}, ErrBookNotFound
	}
	return b, nil
}

func (d *DB) BookByID(ctx context.Context, bid string) (Book, error) {
	b, err := scanBook(d.QueryRowContext(ctx, `SELECT `+bookSelectColumns+` WHERE b.id=?`, bid))
	if err != nil {
		return Book{}, err
	}
	return b, nil
}

func (d *DB) CreateBookAndJob(ctx context.Context, in NewBook) error {
	now := time.Now().UTC().Format(time.RFC3339)
	mode := "transcript"
	if in.EpubRelPath != "" || in.GutenbergID != "" {
		mode = "aligned"
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO books
		(id,owner_user_id,title,author,source_kind,source_url,mode,status,audio_relpath,epub_relpath,
		 gutenberg_id,ebook_source_url,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,'queued',?,?,?,?,?,?)`,
		in.ID, in.OwnerUserID, in.Title, in.Author, in.SourceKind, nullableString(in.SourceURL), mode,
		nullableString(in.AudioRelPath), nullableString(in.EpubRelPath), nullableString(in.GutenbergID),
		nullableString(in.EbookSourceURL), now, now)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO jobs
		(id,owner_user_id,book_id,kind,status,stage,progress,created_at,updated_at)
		VALUES(?,?,?,'book','queued','queued',0,?,?)`, in.JobID, in.OwnerUserID, in.ID, now, now)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO book_cover_state(book_id,status,next_check_at,updated_at)
		VALUES(?,'pending',?,?)`, in.ID, now, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// QueueRetranscription atomically admits one fresh transcription run for an
// already-ready, owner-scoped book that has both audio and an existing transcript.
func (d *DB) QueueRetranscription(ctx context.Context, ownerID, bookID, jobID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := d.ExecContext(ctx, `INSERT INTO jobs
		(id,owner_user_id,book_id,kind,status,stage,progress,created_at,updated_at)
		SELECT ?,?,?,'retranscribe','queued','retranscribing',0,?,?
		WHERE EXISTS (
			SELECT 1 FROM books b
			WHERE b.id=? AND b.owner_user_id=? AND b.status='ready'
				AND COALESCE(b.audio_relpath,'')<>'' AND COALESCE(b.transcript_relpath,'')<>''
				AND NOT EXISTS(SELECT 1 FROM book_cover_state c WHERE c.book_id=b.id AND c.deleting=1)
		) AND NOT EXISTS (
			SELECT 1 FROM jobs j
			WHERE j.book_id=? AND j.owner_user_id=? AND j.status IN ('queued','running')
		) AND NOT EXISTS (
			SELECT 1 FROM admin_book_operations op WHERE op.book_id=? AND op.status IN ('queued','running')
		)`, jobID, ownerID, bookID, now, now, bookID, ownerID, bookID, ownerID, bookID)
	if err != nil {
		return err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if inserted == 1 {
		return nil
	}
	var exists, ready, active, operationActive, deleting int
	err = d.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM books WHERE id=? AND owner_user_id=?),
		EXISTS(SELECT 1 FROM books WHERE id=? AND owner_user_id=? AND status='ready'
			AND COALESCE(audio_relpath,'')<>'' AND COALESCE(transcript_relpath,'')<>''),
		EXISTS(SELECT 1 FROM jobs WHERE book_id=? AND owner_user_id=? AND status IN ('queued','running')),
		EXISTS(SELECT 1 FROM admin_book_operations WHERE book_id=? AND status IN ('queued','running')),
		EXISTS(SELECT 1 FROM book_cover_state WHERE book_id=? AND deleting=1)`,
		bookID, ownerID, bookID, ownerID, bookID, ownerID, bookID, bookID).Scan(&exists, &ready, &active, &operationActive, &deleting)
	if err != nil {
		return err
	}
	switch {
	case exists == 0:
		return ErrBookNotFound
	case active != 0:
		return ErrBookJobInProgress
	case operationActive != 0:
		return ErrBookOperationInProgress
	case deleting != 0:
		return ErrBookDeleteInProgress
	case ready == 0:
		return ErrRetranscriptionNotReady
	default:
		return ErrBookJobInProgress
	}
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (d *DB) SetEbookJSONPath(ctx context.Context, id, relPath string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.ExecContext(ctx, `UPDATE books SET ebook_json_relpath=?,updated_at=? WHERE id=?`, relPath, now, id)
	if err == nil {
		err = d.QueueLocalCoverScan(ctx, id)
	}
	return err
}

func (d *DB) SetEpubPath(ctx context.Context, id, relPath string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.ExecContext(ctx, `UPDATE books SET epub_relpath=?,updated_at=? WHERE id=?`, relPath, now, id)
	if err == nil {
		err = d.QueueLocalCoverScan(ctx, id)
	}
	return err
}

func (d *DB) SetAlignment(ctx context.Context, id, relPath string, quality float64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.ExecContext(ctx, `UPDATE books SET alignment_relpath=?,alignment_quality=?,updated_at=? WHERE id=?`,
		relPath, quality, now, id)
	return err
}

func (d *DB) SetBookMedia(ctx context.Context, id, title, author, audioRelPath string, durationMS int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.ExecContext(ctx, `UPDATE books SET
		title=CASE WHEN title='' OR title='Untitled' THEN ? ELSE title END,
		author=?,audio_relpath=?,duration_ms=?,updated_at=? WHERE id=?`,
		title, author, audioRelPath, durationMS, now, id)
	if err == nil {
		err = d.QueueLocalCoverScan(ctx, id)
	}
	return err
}

func (d *DB) SetTranscriptPath(ctx context.Context, id, transcriptRelPath string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.ExecContext(ctx, `UPDATE books SET transcript_relpath=?,updated_at=? WHERE id=?`,
		transcriptRelPath, now, id)
	return err
}

// SetTranscriptionArtifacts switches the public transcript/alignment pointers
// in one SQLite statement after the complete fresh generation is on disk.
func (d *DB) SetTranscriptionArtifacts(ctx context.Context, id, transcriptRelPath, alignmentRelPath string, quality *float64) error {
	if transcriptRelPath == "" {
		return fmt.Errorf("transcript path is required")
	}
	var alignmentQuality any
	if quality != nil {
		alignmentQuality = *quality
	}
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := d.ExecContext(ctx, `UPDATE books SET transcript_relpath=?,alignment_relpath=?,
		alignment_quality=?,updated_at=? WHERE id=?`,
		transcriptRelPath, nullableString(alignmentRelPath), alignmentQuality, now, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrBookNotFound
	}
	return nil
}

func (d *DB) SetBookAudioPath(ctx context.Context, id, audioRelPath string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.ExecContext(ctx, `UPDATE books SET audio_relpath=?,updated_at=? WHERE id=?`,
		audioRelPath, now, id)
	if err == nil {
		err = d.QueueLocalCoverScan(ctx, id)
	}
	return err
}

func (d *DB) ReplaceChapters(ctx context.Context, bookID string, chapters []Chapter) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM chapters WHERE book_id=?`, bookID); err != nil {
		return err
	}
	for _, c := range chapters {
		if _, err = tx.ExecContext(ctx, `INSERT INTO chapters(id,book_id,ordinal,title,start_ms,end_ms)
			VALUES(?,?,?,?,?,?)`, c.ID, bookID, c.Ordinal, c.Title, c.StartMS, c.EndMS); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) Chapters(ctx context.Context, bookID string) ([]Chapter, error) {
	rows, err := d.QueryContext(ctx, `SELECT id,book_id,ordinal,title,start_ms,end_ms
		FROM chapters WHERE book_id=? ORDER BY ordinal`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Chapter
	for rows.Next() {
		var c Chapter
		if err := rows.Scan(&c.ID, &c.BookID, &c.Ordinal, &c.Title, &c.StartMS, &c.EndMS); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (d *DB) EnsureChunks(ctx context.Context, chunks []Chunk) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, c := range chunks {
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO chunks
			(id,book_id,ordinal,start_ms,end_ms,audio_relpath,response_relpath) VALUES(?,?,?,?,?,?,?)`,
			c.ID, c.BookID, c.Ordinal, c.StartMS, c.EndMS, c.AudioRelPath, c.ResponseRelPath)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) Chunks(ctx context.Context, bookID string) ([]Chunk, error) {
	rows, err := d.QueryContext(ctx, `SELECT id,book_id,ordinal,start_ms,end_ms,status,audio_relpath,
		response_relpath,COALESCE(not_before_at,''),COALESCE(error,'')
		FROM chunks WHERE book_id=? ORDER BY ordinal`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Chunk
	for rows.Next() {
		var c Chunk
		if err := rows.Scan(&c.ID, &c.BookID, &c.Ordinal, &c.StartMS, &c.EndMS, &c.Status,
			&c.AudioRelPath, &c.ResponseRelPath, &c.NotBeforeAt, &c.Error); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (d *DB) UpdateChunk(ctx context.Context, c Chunk) error {
	_, err := d.ExecContext(ctx, `UPDATE chunks SET status=?,response_relpath=?,not_before_at=?,error=? WHERE id=?`,
		c.Status, c.ResponseRelPath, nullableString(c.NotBeforeAt), nullableString(c.Error), c.ID)
	return err
}

func (d *DB) ResetInterruptedJobs(ctx context.Context) error {
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := d.ExecContext(ctx, `UPDATE jobs SET status='queued',updated_at=?
		WHERE status='running'`, now); err != nil {
		return err
	}
	_, err := d.ExecContext(ctx, `UPDATE chunks SET status='queued'
		WHERE status='running'`)
	return err
}

func (d *DB) ClaimNextJob(ctx context.Context) (Job, bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, err
	}
	defer tx.Rollback()
	var j Job
	err = tx.QueryRowContext(ctx, `SELECT id,owner_user_id,book_id,kind,status,stage,progress,attempt,
		COALESCE(error,''),COALESCE(not_before_at,'') FROM jobs
		WHERE status='queued' AND (not_before_at IS NULL OR not_before_at<=?)
			AND NOT EXISTS(SELECT 1 FROM book_cover_state c WHERE c.book_id=jobs.book_id AND c.deleting=1)
		ORDER BY created_at,rowid LIMIT 1`, now).Scan(&j.ID, &j.OwnerUserID, &j.BookID, &j.Kind, &j.Status,
		&j.Stage, &j.Progress, &j.Attempt, &j.Error, &j.NotBeforeAt)
	if err == sql.ErrNoRows {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET status='running',attempt=attempt+1,error=NULL,updated_at=?
		WHERE id=? AND status='queued'`, now, j.ID)
	if err != nil {
		return Job{}, false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return Job{}, false, err
	}
	if affected == 0 {
		return Job{}, false, nil
	}
	j.Status = "running"
	j.Attempt++
	if err := tx.Commit(); err != nil {
		return Job{}, false, err
	}
	return j, true, nil
}

func (d *DB) SetJobProgress(ctx context.Context, id, stage, bookStatus string, progress float64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE jobs SET stage=?,progress=?,error=NULL,updated_at=? WHERE id=?`, stage, progress, now, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE books SET status=?,updated_at=?
		WHERE id=(SELECT book_id FROM jobs WHERE id=?)`, bookStatus, now, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) CompleteJob(ctx context.Context, id string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE jobs SET status='completed',stage='ready',progress=1,
		error=NULL,not_before_at=NULL,updated_at=? WHERE id=?`, now, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE books SET status='ready',updated_at=?
		WHERE id=(SELECT book_id FROM jobs WHERE id=?)`, now, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) DeferJob(ctx context.Context, id, stage, bookStatus, notBefore string, progress float64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	message := ""
	if stage == "rate_limited" {
		message = "Groq rate limit reached; this book is queued to resume."
	}
	if _, err = tx.ExecContext(ctx, `UPDATE jobs SET status='queued',stage=?,progress=?,not_before_at=?,
		error=?,updated_at=? WHERE id=?`, stage, progress, notBefore, nullableString(message), now, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE books SET status=?,updated_at=?
		WHERE id=(SELECT book_id FROM jobs WHERE id=?)`, bookStatus, now, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) FailJob(ctx context.Context, id, stage, bookStatus, message string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE jobs SET status='error',stage=?,error=?,not_before_at=NULL,
		updated_at=? WHERE id=?`, stage, message, now, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE books SET status=?,updated_at=?
		WHERE id=(SELECT book_id FROM jobs WHERE id=?)`, bookStatus, now, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) RetryJob(ctx context.Context, ownerID, bookID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET status='queued',stage='queued',error=NULL,
		not_before_at=NULL,updated_at=? WHERE id=(
			SELECT j.id FROM jobs j
			WHERE j.book_id=? AND j.owner_user_id=? AND j.kind='book' AND j.status='error'
				AND EXISTS(SELECT 1 FROM books b WHERE b.id=j.book_id AND b.owner_user_id=?)
				AND j.id=(SELECT latest.id FROM jobs latest WHERE latest.book_id=j.book_id
					ORDER BY latest.created_at DESC,latest.rowid DESC LIMIT 1)
				AND NOT EXISTS (SELECT 1 FROM jobs active WHERE active.book_id=j.book_id
					AND active.status IN ('queued','running'))
				AND NOT EXISTS (SELECT 1 FROM admin_book_operations op WHERE op.book_id=j.book_id
					AND op.status IN ('queued','running'))
				AND NOT EXISTS (SELECT 1 FROM book_cover_state c WHERE c.book_id=j.book_id AND c.lease_until>?)
				AND NOT EXISTS (SELECT 1 FROM book_cover_state c WHERE c.book_id=j.book_id AND c.deleting=1)
			LIMIT 1
		)`, now, bookID, ownerID, ownerID, now)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		if err := d.AssertBookOwner(ctx, ownerID, bookID); err != nil {
			return ErrBookNotFound
		}
		if active, err := d.BookHasAdminOperation(ctx, bookID); err == nil && active {
			return ErrBookOperationInProgress
		}
		if active, err := d.BookCoverIsRunning(ctx, bookID, now); err == nil && active {
			return ErrBookCoverInProgress
		}
		if deleting, err := d.BookIsDeleting(ctx, ownerID, bookID); err == nil && deleting {
			return ErrBookDeleteInProgress
		}
		return fmt.Errorf("retry not available")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE books SET status=CASE WHEN transcript_relpath IS NULL OR transcript_relpath=''
		THEN 'queued' ELSE 'ready' END,updated_at=? WHERE id=? AND owner_user_id=?`, now, bookID, ownerID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE chunks SET status='queued',not_before_at=NULL,error=NULL
		WHERE book_id=? AND status='error'`, bookID); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) DeleteBook(ctx context.Context, ownerID, bookID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := d.ExecContext(ctx, `DELETE FROM books WHERE id=? AND owner_user_id=?
		AND NOT EXISTS(SELECT 1 FROM admin_book_operations op WHERE op.book_id=books.id
			AND op.status IN ('queued','running'))
		AND EXISTS(SELECT 1 FROM book_cover_state c WHERE c.book_id=books.id AND c.deleting=1)
		AND NOT EXISTS(SELECT 1 FROM book_cover_state c WHERE c.book_id=books.id AND c.lease_until>?)`,
		bookID, ownerID, now)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		if active, err := d.BookHasAdminOperation(ctx, bookID); err == nil && active {
			return ErrBookOperationInProgress
		}
		if active, err := d.BookCoverIsRunning(ctx, bookID, now); err == nil && active {
			return ErrBookCoverInProgress
		}
		return fmt.Errorf("book not found")
	}
	return nil
}

func (d *DB) SaveProgress(ctx context.Context, userID, bookID string, positionMS int64, rate float64, offsetMS int64, appearance string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := d.ExecContext(ctx, `INSERT INTO reading_progress
		(user_id,book_id,position_ms,playback_rate,sync_offset_ms,appearance_json,updated_at)
		SELECT ?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM books WHERE id=? AND owner_user_id=?)
			AND NOT EXISTS(SELECT 1 FROM admin_book_operations op WHERE op.target_book_id=?
				AND op.target_owner_user_id=? AND op.status IN ('queued','running'))
		ON CONFLICT(user_id,book_id) DO UPDATE SET
		position_ms=excluded.position_ms,playback_rate=excluded.playback_rate,
		sync_offset_ms=excluded.sync_offset_ms,appearance_json=excluded.appearance_json,updated_at=excluded.updated_at`,
		userID, bookID, positionMS, rate, offsetMS, appearance, now, bookID, userID, bookID, userID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		var operationActive bool
		if err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM admin_book_operations
			WHERE target_book_id=? AND target_owner_user_id=? AND status IN ('queued','running'))`,
			bookID, userID).Scan(&operationActive); err == nil && operationActive {
			return ErrBookOperationInProgress
		}
		return ErrBookNotFound
	}
	return nil
}

func (d *DB) JobStatus(ctx context.Context, bookID string) (Job, error) {
	var j Job
	err := d.QueryRowContext(ctx, `SELECT id,owner_user_id,book_id,kind,status,stage,progress,attempt,
		COALESCE(error,''),COALESCE(not_before_at,'') FROM jobs WHERE book_id=?
		ORDER BY created_at DESC,rowid DESC LIMIT 1`, bookID).Scan(&j.ID, &j.OwnerUserID, &j.BookID, &j.Kind, &j.Status,
		&j.Stage, &j.Progress, &j.Attempt, &j.Error, &j.NotBeforeAt)
	return j, err
}

func (d *DB) AssertBookOwner(ctx context.Context, uid, bid string) error {
	var x int
	if err := d.QueryRowContext(ctx, `SELECT 1 FROM books WHERE id=? AND owner_user_id=?`, bid, uid).Scan(&x); err != nil {
		return fmt.Errorf("book not found")
	}
	return nil
}

func (d *DB) Users(ctx context.Context) ([]User, error) {
	rows, err := d.QueryContext(ctx, `SELECT u.id,u.email,u.role,u.status,
		u.created_at,u.last_seen_at,COUNT(b.id)
		FROM users u LEFT JOIN books b ON b.owner_user_id=u.id
		GROUP BY u.id ORDER BY u.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &u.Role, &u.Status,
			&u.CreatedAt, &u.LastSeenAt, &u.BookCount); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (d *DB) UserByID(ctx context.Context, id string) (User, error) {
	var user User
	err := d.QueryRowContext(ctx, `SELECT id,email,role,status,created_at,last_seen_at,
		(SELECT COUNT(*) FROM books WHERE owner_user_id=users.id)
		FROM users WHERE id=?`, id).Scan(&user.ID, &user.Email, &user.Role, &user.Status,
		&user.CreatedAt, &user.LastSeenAt, &user.BookCount)
	return user, err
}

func (d *DB) SetUserStatus(ctx context.Context, id, status string) error {
	res, err := d.ExecContext(ctx, `UPDATE users SET status=? WHERE id=?`, status, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("user not found")
	}
	return nil
}
