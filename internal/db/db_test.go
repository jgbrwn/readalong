package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
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

func TestShelfSortOrdersAreStableAndOwnerScoped(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	if _, err := d.UpsertUser(ctx, "sort-owner", "sort@example.org", false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := d.UpsertUser(ctx, "other-owner", "other@example.org", false, false); err != nil {
		t.Fatal(err)
	}
	for _, book := range []struct {
		id, owner, title, author, created string
		duration                          int64
	}{
		{"book-b", "sort-owner", "Zebra", "Zoe Author", "2026-01-01", 1000},
		{"book-a", "sort-owner", "Alpha", "Amy Author", "2026-02-01", 2000},
		{"book-c", "sort-owner", "Middle", "Amy Author", "2026-03-01", 1000},
		{"private", "other-owner", "Aardvark", "A. Private", "2026-04-01", 1000},
	} {
		if _, err := d.ExecContext(ctx, `INSERT INTO books(id,owner_user_id,title,author,duration_ms,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?)`, book.id, book.owner, book.title, book.author, book.duration, book.created, book.created); err != nil {
			t.Fatal(err)
		}
	}
	for _, progress := range []struct {
		bookID, updated string
		position        int64
	}{
		{"book-b", "2026-03-01T00:00:00Z", 900},
		{"book-a", "2026-04-01T00:00:00Z", 1000},
	} {
		if _, err := d.ExecContext(ctx, `INSERT INTO reading_progress(user_id,book_id,position_ms,updated_at)
			VALUES('sort-owner',?,?,?)`, progress.bookID, progress.position, progress.updated); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		sort string
		want []string
	}{
		{"recent", []string{"book-c", "book-a", "book-b"}},
		{"title", []string{"book-a", "book-c", "book-b"}},
		{"author", []string{"book-a", "book-c", "book-b"}},
		{"lastread", []string{"book-a", "book-b", "book-c"}},
		{"progress", []string{"book-b", "book-a", "book-c"}},
	}
	for _, test := range tests {
		books, err := d.SearchBooksForUserSorted(ctx, "sort-owner", "", test.sort)
		if err != nil {
			t.Fatalf("sort %q: %v", test.sort, err)
		}
		got := make([]string, len(books))
		for i := range books {
			got[i] = books[i].ID
		}
		if !reflect.DeepEqual(got, test.want) {
			t.Errorf("sort %q = %v, want %v", test.sort, got, test.want)
		}
	}
}

func TestAdminCloneCreatesIndependentReadyBookWithoutProgressOrJobs(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	for _, user := range []struct{ id, email string }{
		{"book-owner", "owner@example.org"},
		{"recipient", "recipient@example.org"},
		{"admin", "admin@example.org"},
	} {
		if _, err := d.UpsertUser(ctx, user.id, user.email, false, false); err != nil {
			t.Fatal(err)
		}
	}
	quality := 0.93
	if _, err := d.ExecContext(ctx, `INSERT INTO books
		(id,owner_user_id,title,author,source_kind,mode,status,duration_ms,audio_relpath,epub_relpath,
		 ebook_json_relpath,transcript_relpath,alignment_relpath,alignment_quality,gutenberg_id,ebook_source_url,created_at,updated_at)
		VALUES('original','book-owner','Example','Ada Author','upload','aligned','ready',1000,
		 'books/owner/original/playback.mp3','books/owner/original/book.epub',
		 'books/owner/original/ebook.json.gz','books/owner/original/transcript.json.gz',
		 'books/owner/original/alignment.json.gz',?,'123','https://www.gutenberg.org/ebooks/123','2026-01-01','2026-01-01')`,
		quality); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO jobs(id,owner_user_id,book_id,kind,status,stage,progress,created_at,updated_at)
		VALUES('original-job','book-owner','original','book','completed','ready',1,'2026-01-01','2026-01-01')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO book_cover_state
		(book_id,status,selected_kind,selected_relpath,selected_provider,selected_year,
		 ai_candidate_relpath,ai_candidate_year,lookup_paused,updated_at)
		VALUES('original','selected','ai_svg','books/owner/original/cover/generated.svg',
			'Readalong vector art',1935,'books/owner/original/cover/generated-review.svg',1940,1,'2026-01-01')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO chapters(id,book_id,ordinal,title,start_ms,end_ms,transcript_state,alignment_quality)
		VALUES('original-chapter','original',2,'Chapter Two',100,900,'complete',?)`, quality); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO reading_progress(user_id,book_id,position_ms,playback_rate,updated_at)
		VALUES('book-owner','original',800,1.5,'2026-01-02')`); err != nil {
		t.Fatal(err)
	}

	operation := AdminBookOperation{
		ID: "clone-operation", ActorUserID: "admin", Mode: "clone", BookID: "original",
		SourceOwnerUserID: "book-owner", TargetOwnerUserID: "recipient", TargetBookID: "clone-id",
	}
	if err := d.QueueAdminBookOperation(ctx, operation); err != nil {
		t.Fatal(err)
	}
	if _, found, err := d.ClaimNextAdminBookOperation(ctx); err != nil || !found {
		t.Fatal(err)
	}
	if err := d.CloneReadyBookForAdmin(ctx, operation, BookArtifactPaths{
		Audio: "books/recipient/clone-id/playback.mp3", EPUB: "books/recipient/clone-id/book.epub",
		EbookJSON: "books/recipient/clone-id/ebook.json.gz", Transcript: "books/recipient/clone-id/transcript.json.gz",
		Alignment:        "books/recipient/clone-id/alignment.json.gz",
		CoverSelected:    "books/recipient/clone-id/cover/generated.svg",
		CoverAICandidate: "books/recipient/clone-id/cover/generated-review.svg",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.BookForUser(ctx, "recipient", "clone-id"); !errors.Is(err, ErrBookNotFound) {
		t.Fatalf("recipient saw clone before publication completed: %v", err)
	}
	books, err := d.BooksForUser(ctx, "recipient")
	if err != nil || len(books) != 0 {
		t.Fatalf("recipient bookshelf exposed in-progress clone: books=%#v err=%v", books, err)
	}
	if err := d.SaveProgress(ctx, "recipient", "clone-id", 200, 1, 0, "{}"); !errors.Is(err, ErrBookOperationInProgress) {
		t.Fatalf("recipient wrote progress during clone publication: %v", err)
	}
	if err := d.CompleteAdminBookOperation(ctx, operation.ID); err != nil {
		t.Fatal(err)
	}
	clone, err := d.BookForUser(ctx, "recipient", "clone-id")
	if err != nil {
		t.Fatal(err)
	}
	if clone.Title != "Example" || clone.Author != "Ada Author" || clone.Status != "ready" ||
		clone.GutenbergID != "123" || clone.AlignmentQuality == nil || *clone.AlignmentQuality != quality ||
		clone.AudioRelPath != "books/recipient/clone-id/playback.mp3" || clone.JobStatus != "completed" ||
		clone.CoverKind != "ai_svg" || clone.CoverYear != 1935 ||
		clone.CoverURL != "/api/books/clone-id/cover/selected" ||
		clone.CoverAICandidateURL != "/api/books/clone-id/cover/ai-candidate" ||
		clone.CoverAICandidateYear != 1940 {
		t.Fatalf("unexpected cloned book: %#v", clone)
	}
	coverState, err := d.CoverStateForUser(ctx, "recipient", "clone-id")
	if err != nil || coverState.AICandidateRelPath != "books/recipient/clone-id/cover/generated-review.svg" {
		t.Fatalf("generated cover candidate was not remapped on clone: %#v err=%v", coverState, err)
	}
	if _, err := d.BookForUser(ctx, "book-owner", "original"); err != nil {
		t.Fatalf("source book was not retained: %v", err)
	}
	if job, err := d.JobStatus(ctx, "clone-id"); err != nil || job.Kind != "book" ||
		job.Status != "completed" || job.ID != "clone-id-clone" {
		t.Fatalf("clone completion marker=%#v err=%v", job, err)
	}
	chapters, err := d.Chapters(ctx, "clone-id")
	if err != nil || len(chapters) != 1 {
		t.Fatalf("clone chapters=%#v err=%v", chapters, err)
	}
	var chapterQuality sql.NullFloat64
	var chapterID, transcriptState string
	if err := d.QueryRowContext(ctx, `SELECT id,transcript_state,alignment_quality FROM chapters WHERE book_id='clone-id'`).
		Scan(&chapterID, &transcriptState, &chapterQuality); err != nil {
		t.Fatal(err)
	}
	if chapterID != "clone-id-ch-000002" || transcriptState != "complete" ||
		chapterQuality.Float64 != quality || !chapterQuality.Valid {
		t.Fatalf("chapter quality was not preserved: %#v", chapterQuality)
	}
	var progressCount int
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM reading_progress WHERE book_id='clone-id'`).Scan(&progressCount); err != nil {
		t.Fatal(err)
	}
	if progressCount != 0 {
		t.Fatalf("clone inherited %d personal progress row(s)", progressCount)
	}
	secondClone := AdminBookOperation{
		ID: "clone-operation-again", ActorUserID: "admin", Mode: "clone", BookID: "clone-id",
		SourceOwnerUserID: "recipient", TargetOwnerUserID: "book-owner", TargetBookID: "clone-again",
	}
	if err := d.QueueAdminBookOperation(ctx, secondClone); err != nil {
		t.Fatalf("completed clone could not be copied again: %v", err)
	}
}

func TestAdminTransferChangesOwnershipAndRevokesOldProgressWrites(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	for _, user := range []struct{ id, email string }{
		{"book-owner", "owner@example.org"},
		{"recipient", "recipient@example.org"},
		{"admin", "admin@example.org"},
	} {
		if _, err := d.UpsertUser(ctx, user.id, user.email, false, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO books(id,owner_user_id,title,source_url,status,duration_ms,audio_relpath,transcript_relpath,created_at,updated_at)
		VALUES('moving','book-owner','Move me','https://media.example/audio?token=private','ready',1000,'books/old/moving/playback.mp3','books/old/moving/transcript.json.gz','2026-01-01','2026-01-01')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO jobs(id,owner_user_id,book_id,kind,status,stage,progress,created_at,updated_at)
		VALUES('moving-job','book-owner','moving','book','completed','ready',1,'2026-01-01','2026-01-01')`); err != nil {
		t.Fatal(err)
	}
	if err := d.SaveProgress(ctx, "book-owner", "moving", 700, 1, 0, "{}"); err != nil {
		t.Fatal(err)
	}
	operation := AdminBookOperation{
		ID: "move-operation", ActorUserID: "admin", Mode: "transfer", BookID: "moving",
		SourceOwnerUserID: "book-owner", TargetOwnerUserID: "recipient", TargetBookID: "moving",
	}
	if err := d.QueueAdminBookOperation(ctx, operation); err != nil {
		t.Fatal(err)
	}
	if _, found, err := d.ClaimNextAdminBookOperation(ctx); err != nil || !found {
		t.Fatalf("claim found=%v err=%v", found, err)
	}
	if err := d.TransferReadyBookForAdmin(ctx, operation, BookArtifactPaths{
		Audio: "books/new/moving/playback.mp3", Transcript: "books/new/moving/transcript.json.gz",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.BookForUser(ctx, "recipient", "moving"); !errors.Is(err, ErrBookNotFound) {
		t.Fatalf("recipient saw book before transfer cleanup: %v", err)
	}
	if err := d.SaveProgress(ctx, "recipient", "moving", 200, 1, 0, "{}"); !errors.Is(err, ErrBookOperationInProgress) {
		t.Fatalf("recipient wrote progress during transfer: %v", err)
	}
	if err := d.CompleteAdminBookOperation(ctx, operation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.BookForUser(ctx, "book-owner", "moving"); !errors.Is(err, ErrBookNotFound) {
		t.Fatalf("former owner retained access: %v", err)
	}
	book, err := d.BookForUser(ctx, "recipient", "moving")
	if err != nil || book.AudioRelPath != "books/new/moving/playback.mp3" {
		t.Fatalf("recipient book=%#v err=%v", book, err)
	}
	if err := d.SaveProgress(ctx, "book-owner", "moving", 900, 1, 0, "{}"); !errors.Is(err, ErrBookNotFound) {
		t.Fatalf("former owner could write progress: %v", err)
	}
	var jobOwner string
	if err := d.QueryRowContext(ctx, `SELECT owner_user_id FROM jobs WHERE id='moving-job'`).Scan(&jobOwner); err != nil {
		t.Fatal(err)
	}
	if jobOwner != "recipient" {
		t.Fatalf("historical job owner = %q", jobOwner)
	}
	var sourceURL sql.NullString
	if err := d.QueryRowContext(ctx, `SELECT source_url FROM books WHERE id='moving'`).Scan(&sourceURL); err != nil {
		t.Fatal(err)
	}
	if sourceURL.Valid {
		t.Fatalf("transfer retained source URL %q", sourceURL.String)
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
