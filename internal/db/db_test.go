package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestAdminBootstrapClaimsStableUserID(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	admin, err := d.UpsertUser(ctx, "exe-user-1", "admin@example.org", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if admin.Role != "admin" || admin.Status != "active" {
		t.Fatalf("unexpected initial user: %#v", admin)
	}
	other, err := d.UpsertUser(ctx, "exe-user-2", "admin@example.org", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if other.Role != "user" {
		t.Fatalf("bootstrap email transferred admin role: %#v", other)
	}
	renamed, err := d.UpsertUser(ctx, "exe-user-1", "new-address@example.org", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Role != "admin" || renamed.Email != "new-address@example.org" {
		t.Fatalf("admin role did not stay bound to stable ID: %#v", renamed)
	}
	configured, err := d.UpsertUser(ctx, "configured-admin", "third@example.org", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if configured.Role != "admin" {
		t.Fatalf("configured ID did not get admin role: %#v", configured)
	}
}

func TestUserStatusAndBookShelfMetadata(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	if _, err := d.UpsertUser(ctx, "u1", "one@example.org", false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := d.UpsertUser(ctx, "u2", "two@example.org", false, false); err != nil {
		t.Fatal(err)
	}
	if err := d.SetUserStatus(ctx, "u2", "suspended"); err != nil {
		t.Fatal(err)
	}
	suspended, err := d.UpsertUser(ctx, "u2", "two@example.org", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if suspended.Status != "suspended" {
		t.Fatalf("status was not persisted across visits: %#v", suspended)
	}
	users, err := d.Users(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 {
		t.Fatalf("got %d users, want 2", len(users))
	}
}

func TestShelfSearchIsOwnerScopedAndMatchesTitleOrAuthor(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	if _, err := d.UpsertUser(ctx, "search-owner", "one@example.org", false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := d.UpsertUser(ctx, "other-owner", "two@example.org", false, false); err != nil {
		t.Fatal(err)
	}
	for _, book := range []struct{ id, owner, title, author string }{
		{"one", "search-owner", "Moby-Dick", "Herman Melville"},
		{"two", "search-owner", "Middlemarch", "George Eliot"},
		{"private", "other-owner", "Moby-Dick", "Herman Melville"},
	} {
		if _, err := d.ExecContext(ctx, `INSERT INTO books(id,owner_user_id,title,author,created_at,updated_at)
			VALUES(?,?,?,?,?,?)`, book.id, book.owner, book.title, book.author, "2026-01-01", "2026-01-01"); err != nil {
			t.Fatal(err)
		}
	}
	for query, want := range map[string]string{"moby": "one", "ELIOT": "two", "%": ""} {
		books, err := d.SearchBooksForUser(ctx, "search-owner", query)
		if err != nil {
			t.Fatal(err)
		}
		if want == "" {
			if len(books) != 0 {
				t.Fatalf("query %q matched unexpectedly: %#v", query, books)
			}
		} else if len(books) != 1 || books[0].ID != want {
			t.Fatalf("query %q results = %#v; want %s", query, books, want)
		}
	}
}

func TestQueueRetranscriptionKeepsCurrentArtifactsAndRejectsDuplicates(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	if _, err := d.UpsertUser(ctx, "retranscribe-owner", "owner@example.org", false, false); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateBookAndJob(ctx, NewBook{
		ID: "book", JobID: "initial-job", OwnerUserID: "retranscribe-owner",
		Title: "Test Book", SourceKind: "upload", AudioRelPath: "books/test/playback.mp3",
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetTranscriptPath(ctx, "book", "books/test/transcript.v1.json.gz"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetAlignment(ctx, "book", "books/test/alignment.v1.json.gz", 0.82); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteJob(ctx, "initial-job"); err != nil {
		t.Fatal(err)
	}

	if err := d.QueueRetranscription(ctx, "retranscribe-owner", "book", "fresh-job"); err != nil {
		t.Fatal(err)
	}
	book, err := d.BookByID(ctx, "book")
	if err != nil {
		t.Fatal(err)
	}
	if book.Status != "ready" || book.TranscriptRelPath != "books/test/transcript.v1.json.gz" ||
		book.AlignmentRelPath != "books/test/alignment.v1.json.gz" || book.AudioRelPath != "books/test/playback.mp3" {
		t.Fatalf("queueing changed live book artifacts: %#v", book)
	}
	job, err := d.JobStatus(ctx, "book")
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != "fresh-job" || job.Kind != "retranscribe" || job.Status != "queued" {
		t.Fatalf("unexpected fresh-transcription job: %#v", job)
	}
	if err := d.QueueRetranscription(ctx, "retranscribe-owner", "book", "duplicate-job"); err != ErrBookJobInProgress {
		t.Fatalf("second active run error = %v, want ErrBookJobInProgress", err)
	}
	if err := d.QueueRetranscription(ctx, "other-owner", "book", "other-user-job"); err != ErrBookNotFound {
		t.Fatalf("cross-owner queue error = %v, want ErrBookNotFound", err)
	}

	quality := 0.91
	if err := d.SetTranscriptionArtifacts(ctx, "book", "books/test/fresh/transcript.v1.json.gz",
		"books/test/fresh/alignment.v1.json.gz", &quality); err != nil {
		t.Fatal(err)
	}
	book, err = d.BookByID(ctx, "book")
	if err != nil {
		t.Fatal(err)
	}
	if book.TranscriptRelPath != "books/test/fresh/transcript.v1.json.gz" ||
		book.AlignmentRelPath != "books/test/fresh/alignment.v1.json.gz" ||
		book.AlignmentQuality == nil || *book.AlignmentQuality != quality {
		t.Fatalf("fresh artifact pointers were not published together: %#v", book)
	}
}

func TestQueueRetranscriptionRequiresReadyBookAndExistingTranscript(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	if _, err := d.UpsertUser(ctx, "retranscribe-owner", "owner@example.org", false, false); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateBookAndJob(ctx, NewBook{
		ID: "book", JobID: "initial-job", OwnerUserID: "retranscribe-owner",
		Title: "Processing Book", SourceKind: "upload", AudioRelPath: "books/test/playback.mp3",
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.QueueRetranscription(ctx, "retranscribe-owner", "book", "fresh-job"); err != ErrBookJobInProgress {
		t.Fatalf("active initial job error = %v, want ErrBookJobInProgress", err)
	}
	if err := d.CompleteJob(ctx, "initial-job"); err != nil {
		t.Fatal(err)
	}
	if err := d.QueueRetranscription(ctx, "retranscribe-owner", "book", "fresh-job"); err != ErrRetranscriptionNotReady {
		t.Fatalf("book without transcript error = %v, want ErrRetranscriptionNotReady", err)
	}
}

func TestGenericRetryDoesNotDuplicateOrRetryRetranscriptionJobs(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	if _, err := d.UpsertUser(ctx, "retranscribe-owner", "owner@example.org", false, false); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateBookAndJob(ctx, NewBook{
		ID: "book", JobID: "initial-job", OwnerUserID: "retranscribe-owner",
		Title: "Test book", SourceKind: "upload", AudioRelPath: "books/test/playback.mp3",
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetTranscriptPath(ctx, "book", "books/test/old/transcript.v1.json.gz"); err != nil {
		t.Fatal(err)
	}
	if err := d.FailJob(ctx, "initial-job", "transcribing", "ready", "first pass failed"); err != nil {
		t.Fatal(err)
	}
	if err := d.QueueRetranscription(ctx, "retranscribe-owner", "book", "fresh-job"); err != nil {
		t.Fatal(err)
	}
	if err := d.RetryJob(ctx, "retranscribe-owner", "book"); err == nil {
		t.Fatal("generic retry should not queue a second job while a fresh pass is active")
	}
	if err := d.FailJob(ctx, "fresh-job", "retranscribing", "ready", "fresh pass failed"); err != nil {
		t.Fatal(err)
	}
	if err := d.RetryJob(ctx, "retranscribe-owner", "book"); err == nil {
		t.Fatal("generic retry should not re-run a failed fresh-transcription job")
	}
	job, err := d.JobStatus(ctx, "book")
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != "fresh-job" || job.Kind != "retranscribe" || job.Status != "error" {
		t.Fatalf("unexpected final job state: %#v", job)
	}
}

func TestOpenMigratesOlderUsersTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.db")
	sqlDB, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = sqlDB.Exec(`CREATE TABLE users (
		id TEXT PRIMARY KEY, email TEXT NOT NULL, created_at TEXT NOT NULL, last_seen_at TEXT NOT NULL
	);
	INSERT INTO users VALUES('old-id','old@example.org','2020-01-01','2020-01-01');
	CREATE TABLE books (
		id TEXT PRIMARY KEY, owner_user_id TEXT NOT NULL, title TEXT NOT NULL DEFAULT '',
		author TEXT NOT NULL DEFAULT '', source_kind TEXT NOT NULL DEFAULT '', source_url TEXT,
		mode TEXT NOT NULL DEFAULT 'transcript', status TEXT NOT NULL DEFAULT 'queued',
		duration_ms INTEGER NOT NULL DEFAULT 0, audio_relpath TEXT, epub_relpath TEXT,
		transcript_relpath TEXT, alignment_relpath TEXT, alignment_quality REAL,
		created_at TEXT NOT NULL, updated_at TEXT NOT NULL
	)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	d, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	user, err := d.UpsertUser(context.Background(), "old-id", "old@example.org", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if user.Role != "user" || user.Status != "active" {
		t.Fatalf("legacy account defaults incorrect: %#v", user)
	}
	for _, column := range []string{"ebook_json_relpath", "gutenberg_id", "ebook_source_url"} {
		var found bool
		rows, err := d.Query(`PRAGMA table_info(books)`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var cid, notnull, pk int
			var name, typ string
			var defaultValue any
			if err := rows.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			found = found || name == column
		}
		_ = rows.Close()
		if !found {
			t.Errorf("migration did not add books.%s", column)
		}
	}
}
