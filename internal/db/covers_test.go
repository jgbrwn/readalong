package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCoverStateBackfillsExistingBooksAndQueuesNewBooks(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	if _, err := d.UpsertUser(ctx, "cover-owner", "cover@example.org", false, false); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := d.ExecContext(ctx, `INSERT INTO books(id,owner_user_id,title,author,status,created_at,updated_at)
		VALUES('legacy-cover','cover-owner','Legacy Cover','Writer','ready',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err := d.migrate(); err != nil {
		t.Fatal(err)
	}
	state, err := d.CoverStateForUser(ctx, "cover-owner", "legacy-cover")
	if err != nil || state.Status != "pending" || state.NextCheckAt == "" {
		t.Fatalf("existing bookshelf item cover state=%#v err=%v", state, err)
	}
	if err := d.CreateBookAndJob(ctx, NewBook{
		ID: "new-cover", OwnerUserID: "cover-owner", Title: "New Cover", Author: "Writer",
		SourceKind: "upload", JobID: "new-cover-job",
	}); err != nil {
		t.Fatal(err)
	}
	state, err = d.CoverStateForUser(ctx, "cover-owner", "new-cover")
	if err != nil || state.Status != "pending" || state.NextCheckAt == "" {
		t.Fatalf("new bookshelf item cover state=%#v err=%v", state, err)
	}
}

func TestCoverWorkerCanStartDuringInitialIngestionOnceArtifactsAreStable(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	d := openTestDB(t)
	if _, err := d.UpsertUser(ctx, "cover-owner", "cover@example.org", false, false); err != nil {
		t.Fatal(err)
	}

	if err := d.CreateBookAndJob(ctx, NewBook{
		ID: "pair-cover", JobID: "pair-cover-job", OwnerUserID: "cover-owner",
		Title: "Pair Cover", Author: "A Writer", SourceKind: "librivox", GutenbergID: "1234",
	}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := d.ClaimNextJob(ctx); err != nil || !found {
		t.Fatalf("pair import job was not claimed: found=%v err=%v", found, err)
	}
	assertNoCoverTask := func(bookID string) {
		t.Helper()
		if _, found, err := d.ClaimNextCoverTask(ctx, now.Format(time.RFC3339),
			now.Add(time.Minute).Format(time.RFC3339)); err != nil || found {
			t.Fatalf("cover task for %s claimed before its ingestion artifacts were ready: found=%v err=%v", bookID, found, err)
		}
	}
	assertNoCoverTask("pair-cover")
	if err := d.SetEpubPath(ctx, "pair-cover", "books/cover-owner/pair-cover/source/book.epub"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetEbookJSONPath(ctx, "pair-cover", "books/cover-owner/pair-cover/ebook.v1.json.gz"); err != nil {
		t.Fatal(err)
	}
	assertNoCoverTask("pair-cover")
	if err := d.SetBookMedia(ctx, "pair-cover", "Pair Cover", "A Writer",
		"books/cover-owner/pair-cover/playback.mp3", 60_000); err != nil {
		t.Fatal(err)
	}
	assertNoCoverTask("pair-cover")
	if err := d.SetJobProgress(ctx, "pair-cover-job", "transcribing", "transcribing", 0.16); err != nil {
		t.Fatal(err)
	}
	task, found, err := d.ClaimNextCoverTask(ctx, now.Format(time.RFC3339),
		now.Add(time.Minute).Format(time.RFC3339))
	if err != nil || !found || task.BookID != "pair-cover" {
		t.Fatalf("pair cover task did not start during transcription: task=%#v found=%v err=%v", task, found, err)
	}

	if err := d.CreateBookAndJob(ctx, NewBook{
		ID: "upload-cover", JobID: "upload-cover-job", OwnerUserID: "cover-owner",
		Title: "Upload Cover", Author: "A Writer", SourceKind: "upload",
		AudioRelPath: "books/cover-owner/upload-cover/source/upload.mp3",
		EpubRelPath:  "books/cover-owner/upload-cover/source/book.epub",
	}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := d.ClaimNextJob(ctx); err != nil || !found {
		t.Fatalf("upload job was not claimed: found=%v err=%v", found, err)
	}
	if err := d.SetBookMedia(ctx, "upload-cover", "Upload Cover", "A Writer",
		"books/cover-owner/upload-cover/playback.mp3", 60_000); err != nil {
		t.Fatal(err)
	}
	if err := d.SetJobProgress(ctx, "upload-cover-job", "transcribing", "transcribing", 0.16); err != nil {
		t.Fatal(err)
	}
	assertNoCoverTask("upload-cover")
	if err := d.SetEbookJSONPath(ctx, "upload-cover", "books/cover-owner/upload-cover/ebook.v1.json.gz"); err != nil {
		t.Fatal(err)
	}
	task, found, err = d.ClaimNextCoverTask(ctx, now.Format(time.RFC3339),
		now.Add(time.Minute).Format(time.RFC3339))
	if err != nil || !found || task.BookID != "upload-cover" {
		t.Fatalf("aligned upload cover task did not start after EPUB artifacts were published: task=%#v found=%v err=%v", task, found, err)
	}
}

func TestCoverWorkerCanClaimAudioOnlyUploadDuringTranscription(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	if _, err := d.UpsertUser(ctx, "cover-owner", "cover@example.org", false, false); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateBookAndJob(ctx, NewBook{
		ID: "audio-only-cover", JobID: "audio-only-cover-job", OwnerUserID: "cover-owner",
		Title: "Audio Only", Author: "A Writer", SourceKind: "upload",
		AudioRelPath: "books/cover-owner/audio-only-cover/source/upload.mp3",
	}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := d.ClaimNextJob(ctx); err != nil || !found {
		t.Fatalf("audio upload job was not claimed: found=%v err=%v", found, err)
	}
	now := time.Now().UTC()
	if _, found, err := d.ClaimNextCoverTask(ctx, now.Format(time.RFC3339),
		now.Add(time.Minute).Format(time.RFC3339)); err != nil || found {
		t.Fatalf("cover task ran before audio metadata was published: found=%v err=%v", found, err)
	}
	if err := d.SetBookMedia(ctx, "audio-only-cover", "Audio Only", "A Writer",
		"books/cover-owner/audio-only-cover/playback.mp3", 60_000); err != nil {
		t.Fatal(err)
	}
	if _, found, err := d.ClaimNextCoverTask(ctx, now.Format(time.RFC3339),
		now.Add(time.Minute).Format(time.RFC3339)); err != nil || found {
		t.Fatalf("cover task ran before transcription-stage metadata was final: found=%v err=%v", found, err)
	}
	if err := d.SetJobProgress(ctx, "audio-only-cover-job", "transcribing", "transcribing", 0.16); err != nil {
		t.Fatal(err)
	}
	task, found, err := d.ClaimNextCoverTask(ctx, now.Format(time.RFC3339),
		now.Add(time.Minute).Format(time.RFC3339))
	if err != nil || !found || task.BookID != "audio-only-cover" {
		t.Fatalf("audio-only cover task did not run during transcription: task=%#v found=%v err=%v",
			task, found, err)
	}
}

func TestAIImageAttemptReservationSurvivesWorkerRestart(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	if _, err := d.UpsertUser(ctx, "cover-owner", "cover@example.org", false, false); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateBookAndJob(ctx, NewBook{
		ID: "reserved-cover", JobID: "reserved-cover-job", OwnerUserID: "cover-owner",
		Title: "Reserved Cover", SourceKind: "upload",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `UPDATE books SET status='ready',duration_ms=60000,
		audio_relpath='books/cover-owner/reserved-cover/playback.mp3',
		transcript_relpath='books/cover-owner/reserved-cover/transcript.v1.json.gz' WHERE id='reserved-cover'`); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteJob(ctx, "reserved-cover-job"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task, found, err := d.ClaimNextCoverTask(ctx, now.Format(time.RFC3339),
		now.Add(time.Minute).Format(time.RFC3339))
	if err != nil || !found || task.RegenerationRequested {
		t.Fatalf("automatic cover claim=%#v found=%v err=%v", task, found, err)
	}
	if err := d.ReserveAIGenerationAttempt(ctx, task); err != nil {
		t.Fatal(err)
	}

	// Simulate a process crash after the durable reservation but before
	// dispatching the provider request; the expired lease must not reauthorize it.
	if _, err := d.ExecContext(ctx, `UPDATE book_cover_state SET lease_until=NULL WHERE book_id='reserved-cover'`); err != nil {
		t.Fatal(err)
	}
	replay, found, err := d.ClaimNextCoverTask(ctx, now.Add(time.Second).Format(time.RFC3339),
		now.Add(2*time.Minute).Format(time.RFC3339))
	if err != nil || !found || !replay.AIGenerationBlocked || replay.RegenerationRequested {
		t.Fatalf("restart replay claim=%#v found=%v err=%v", replay, found, err)
	}
	if err := d.ReserveAIGenerationAttempt(ctx, replay); !errors.Is(err, ErrCoverTaskLeaseLost) {
		t.Fatalf("automatic replay reauthorized a blocked attempt: %v", err)
	}
	_ = d.ReleaseCoverTask(ctx, replay)

	if err := d.QueueCoverRegeneration(ctx, "cover-owner", "reserved-cover"); err != nil {
		t.Fatal(err)
	}
	manual, found, err := d.ClaimNextCoverTask(ctx, now.Add(2*time.Second).Format(time.RFC3339),
		now.Add(3*time.Minute).Format(time.RFC3339))
	if err != nil || !found || !manual.RegenerationRequested || !manual.AIGenerationBlocked {
		t.Fatalf("manual retry authorization=%#v found=%v err=%v", manual, found, err)
	}
	if err := d.ReserveAIGenerationAttempt(ctx, manual); err != nil {
		t.Fatalf("explicit regeneration could not reserve a fresh attempt: %v", err)
	}
}

func TestRetranscriptionCannotOverlapAnActiveCoverWorker(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	if _, err := d.UpsertUser(ctx, "cover-owner", "cover@example.org", false, false); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateBookAndJob(ctx, NewBook{
		ID: "cover-lock", JobID: "cover-lock-job", OwnerUserID: "cover-owner",
		Title: "Cover Lock", SourceKind: "upload",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `UPDATE books SET status='ready',duration_ms=60000,
		audio_relpath='books/cover-owner/cover-lock/playback.mp3',
		transcript_relpath='books/cover-owner/cover-lock/transcript.v1.json.gz' WHERE id='cover-lock'`); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteJob(ctx, "cover-lock-job"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task, found, err := d.ClaimNextCoverTask(ctx, now.Format(time.RFC3339),
		now.Add(time.Minute).Format(time.RFC3339))
	if err != nil || !found {
		t.Fatalf("cover task claim=%#v found=%v err=%v", task, found, err)
	}
	if err := d.QueueRetranscription(ctx, "cover-owner", "cover-lock", "cover-lock-retranscribe"); !errors.Is(err, ErrBookCoverInProgress) {
		t.Fatalf("retranscription queued during cover generation: %v", err)
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO jobs
		(id,owner_user_id,book_id,kind,status,stage,created_at,updated_at)
		VALUES('cover-lock-race','cover-owner','cover-lock','retranscribe','queued','retranscribing',?,?)`,
		now.Format(time.RFC3339), now.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if _, found, err := d.ClaimNextJob(ctx); err != nil || found {
		t.Fatalf("worker claimed retranscription during cover lease: found=%v err=%v", found, err)
	}
	if err := d.ReleaseCoverTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job, found, err := d.ClaimNextJob(ctx)
	if err != nil || !found || job.ID != "cover-lock-race" {
		t.Fatalf("retranscription did not resume after cover lease: job=%#v found=%v err=%v", job, found, err)
	}
}

func TestCoverWorkerClaimLeaseAndCandidateChoice(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	if _, err := d.UpsertUser(ctx, "cover-owner", "cover@example.org", false, false); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := d.ExecContext(ctx, `INSERT INTO books(id,owner_user_id,title,author,status,created_at,updated_at)
		VALUES('cover-book','cover-owner','Cover Book','A Writer','ready',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO book_cover_state(book_id,status,next_check_at,updated_at)
		VALUES('cover-book','pending',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	task, found, err := d.ClaimNextCoverTask(ctx, now, time.Now().UTC().Add(time.Minute).Format(time.RFC3339))
	if err != nil || !found || task.BookID != "cover-book" || task.Title != "Cover Book" {
		t.Fatalf("claim=%#v found=%v err=%v", task, found, err)
	}
	if _, found, err := d.ClaimNextCoverTask(ctx, now, time.Now().UTC().Add(time.Minute).Format(time.RFC3339)); err != nil || found {
		t.Fatalf("leased cover task was claimed twice: found=%v err=%v", found, err)
	}
	if err := d.SetCoverCandidate(ctx, task,
		"https://covers.openlibrary.org/b/id/77-M.jpg?default=false",
		"https://openlibrary.org/works/OL77W", 1920); err != nil {
		t.Fatal(err)
	}
	if err := d.ChooseCoverCandidate(ctx, "cover-owner", "cover-book", "use_candidate"); err != nil {
		t.Fatal(err)
	}
	state, err := d.CoverStateForUser(ctx, "cover-owner", "cover-book")
	if err != nil || state.SelectedKind != "catalog" || state.SelectedYear != 1920 ||
		state.SelectedURL != "https://covers.openlibrary.org/b/id/77-M.jpg?default=false" ||
		state.CandidateURL != "" || !state.LookupPaused {
		t.Fatalf("selected cover state=%#v err=%v", state, err)
	}
}

func TestManualCoverRegenerationPreservesSelectionAndOffersBothAlternates(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	if _, err := d.UpsertUser(ctx, "cover-owner", "cover@example.org", false, false); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateBookAndJob(ctx, NewBook{
		ID: "regenerate-book", JobID: "regenerate-job", OwnerUserID: "cover-owner",
		Title: "Regenerate Me", SourceKind: "upload",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `UPDATE books SET status='ready',audio_relpath='books/cover-owner/regenerate-book/playback.mp3',
		transcript_relpath='books/cover-owner/regenerate-book/transcript.json.gz' WHERE id='regenerate-book'`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `UPDATE jobs SET status='completed',stage='ready',progress=1 WHERE id='regenerate-job'`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `UPDATE book_cover_state SET
		status='selected',selected_kind='ai_svg',selected_relpath='books/cover-owner/regenerate-book/cover/generated.svg',
		selected_provider='Readalong vector art',selected_year=1930,lookup_paused=1 WHERE book_id='regenerate-book'`); err != nil {
		t.Fatal(err)
	}
	if err := d.QueueCoverRegeneration(ctx, "cover-owner", "regenerate-book"); err != nil {
		t.Fatal(err)
	}
	book, err := d.BookForUser(ctx, "cover-owner", "regenerate-book")
	if err != nil || !book.CoverRegenerationQueued || book.CoverKind != "ai_svg" ||
		!strings.HasPrefix(book.CoverURL, "/api/books/regenerate-book/cover/selected?v=") {
		t.Fatalf("queued regeneration did not preserve the selected cover: book=%#v err=%v", book, err)
	}
	now := time.Now().UTC()
	task, found, err := d.ClaimNextCoverTask(ctx, now.Format(time.RFC3339),
		now.Add(time.Minute).Format(time.RFC3339))
	if err != nil || !found || !task.RegenerationRequested || task.SelectedKind != "ai_svg" ||
		task.SelectedYear != 1930 {
		t.Fatalf("regeneration task=%#v found=%v err=%v", task, found, err)
	}
	if err := d.SetCoverRegenerationResults(ctx, task, &CoverSuggestion{
		URL:       "https://covers.openlibrary.org/b/id/99-M.jpg",
		SourceURL: "https://openlibrary.org/works/OL99W", Year: 1935,
	}, "books/cover-owner/regenerate-book/cover/generated-new.jpg", 1935,
		now.Format(time.RFC3339), "", "found", false, false); err != nil {
		t.Fatal(err)
	}
	book, err = d.BookForUser(ctx, "cover-owner", "regenerate-book")
	if err != nil || !book.CoverReviewNeeded ||
		book.CoverCandidateURL != "https://covers.openlibrary.org/b/id/99-M.jpg" ||
		!strings.HasPrefix(book.CoverAICandidateURL, "/api/books/regenerate-book/cover/ai-candidate?v=") ||
		book.CoverAICandidateYear != 1935 || !strings.HasPrefix(book.CoverURL, "/api/books/regenerate-book/cover/selected?v=") {
		t.Fatalf("manual regeneration did not expose both choices: book=%#v err=%v", book, err)
	}
	if err := d.ChooseCoverCandidate(ctx, "cover-owner", "regenerate-book", "use_ai_candidate"); err != nil {
		t.Fatal(err)
	}
	book, err = d.BookForUser(ctx, "cover-owner", "regenerate-book")
	if err != nil || book.CoverKind != "ai_image" ||
		!strings.HasPrefix(book.CoverURL, "/api/books/regenerate-book/cover/selected?v=") ||
		book.CoverReviewNeeded || book.CoverAICandidateURL != "" {
		t.Fatalf("new AI cover was not selected cleanly: book=%#v err=%v", book, err)
	}

	if err := d.QueueCoverRegeneration(ctx, "cover-owner", "regenerate-book"); err != nil {
		t.Fatal(err)
	}
	task, found, err = d.ClaimNextCoverTask(ctx, now.Format(time.RFC3339),
		now.Add(2*time.Minute).Format(time.RFC3339))
	if err != nil || !found || !task.RegenerationRequested {
		t.Fatalf("second regeneration task=%#v found=%v err=%v", task, found, err)
	}
	if err := d.SetCoverRegenerationResults(ctx, task, &CoverSuggestion{
		URL:       "https://covers.openlibrary.org/b/id/100-M.jpg",
		SourceURL: "https://openlibrary.org/works/OL100W", Year: 1936,
	}, "books/cover-owner/regenerate-book/cover/generated-newer.svg", 1936,
		now.Format(time.RFC3339), "", "found", false, false); err != nil {
		t.Fatal(err)
	}
	if err := d.ChooseCoverCandidate(ctx, "cover-owner", "regenerate-book", "use_candidate"); err != nil {
		t.Fatal(err)
	}
	book, err = d.BookForUser(ctx, "cover-owner", "regenerate-book")
	if err != nil || book.CoverKind != "catalog" ||
		book.CoverURL != "https://covers.openlibrary.org/b/id/100-M.jpg" ||
		book.CoverReviewNeeded || book.CoverAICandidateURL != "" {
		t.Fatalf("catalog cover was not selected cleanly: book=%#v err=%v", book, err)
	}
}

func TestCoverLookupCacheIsSharedByMetadataKeyAndExpires(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	now := time.Now().UTC()
	entry := CoverLookupCache{
		ResultJSON: `{"ImageURL":"https://covers.openlibrary.org/b/id/77-M.jpg?default=false"}`,
		Found:      true, CheckedAt: now.Format(time.RFC3339), NextCheckAt: now.Add(time.Hour).Format(time.RFC3339),
	}
	if err := d.SetCoverLookupCache(ctx, "book-key", entry.ResultJSON, entry.Found, 0, entry.CheckedAt, entry.NextCheckAt); err != nil {
		t.Fatal(err)
	}
	cached, found, fresh, err := d.CoverLookupCache(ctx, "book-key", now.Add(30*time.Minute).Format(time.RFC3339))
	if err != nil || !found || !fresh || !cached.Found {
		t.Fatalf("cache found=%v fresh=%v entry=%#v err=%v", found, fresh, cached, err)
	}
	_, _, fresh, err = d.CoverLookupCache(ctx, "book-key", now.Add(2*time.Hour).Format(time.RFC3339))
	if err != nil || fresh {
		t.Fatalf("expired cache marked fresh=%v err=%v", fresh, err)
	}
}

func TestBookDeletionReservationProtectsInFlightCoverWork(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	if _, err := d.UpsertUser(ctx, "cover-owner", "cover@example.org", false, false); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateBookAndJob(ctx, NewBook{
		ID: "delete-cover", JobID: "delete-cover-job", OwnerUserID: "cover-owner",
		Title: "Delete Cover", SourceKind: "upload",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `UPDATE books SET status='ready',audio_relpath='books/owner/delete-cover/audio.mp3',
		transcript_relpath='books/owner/delete-cover/transcript.json.gz' WHERE id='delete-cover'`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `UPDATE jobs SET status='completed',stage='ready',progress=1 WHERE id='delete-cover-job'`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	lease := now.Add(time.Minute).Format(time.RFC3339)
	if _, err := d.ExecContext(ctx, `UPDATE book_cover_state SET lease_until=? WHERE book_id='delete-cover'`, lease); err != nil {
		t.Fatal(err)
	}
	if err := d.BeginBookDeletion(ctx, "cover-owner", "delete-cover"); !errors.Is(err, ErrBookCoverInProgress) {
		t.Fatalf("deletion reservation with active cover lease = %v", err)
	}
	if active, err := d.BookIsDeleting(ctx, "cover-owner", "delete-cover"); err != nil || active {
		t.Fatalf("failed deletion left reservation active=%v err=%v", active, err)
	}
	if _, err := d.ExecContext(ctx, `UPDATE book_cover_state SET lease_until=NULL WHERE book_id='delete-cover'`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `UPDATE jobs SET status='running' WHERE id='delete-cover-job'`); err != nil {
		t.Fatal(err)
	}
	if err := d.BeginBookDeletion(ctx, "cover-owner", "delete-cover"); !errors.Is(err, ErrBookJobInProgress) {
		t.Fatalf("deletion reservation with active job = %v", err)
	}
	if _, err := d.ExecContext(ctx, `UPDATE jobs SET status='queued' WHERE id='delete-cover-job'`); err != nil {
		t.Fatal(err)
	}
	if err := d.BeginBookDeletion(ctx, "cover-owner", "delete-cover"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := d.ClaimNextCoverTask(ctx, now.Format(time.RFC3339), now.Add(time.Minute).Format(time.RFC3339)); err != nil || found {
		t.Fatalf("deleting book was claimed for cover work: found=%v err=%v", found, err)
	}
	if _, found, err := d.ClaimNextJob(ctx); err != nil || found {
		t.Fatalf("deleting book's queued job was claimed: found=%v err=%v", found, err)
	}
	if err := d.DeleteBook(ctx, "cover-owner", "delete-cover"); err != nil {
		t.Fatal(err)
	}
}

func TestRestartClearsAnInterruptedDeletionReservation(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	if _, err := d.UpsertUser(ctx, "cover-owner", "cover@example.org", false, false); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateBookAndJob(ctx, NewBook{
		ID: "interrupted-delete", JobID: "interrupted-delete-job", OwnerUserID: "cover-owner",
		Title: "Interrupted Delete", SourceKind: "upload",
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.BeginBookDeletion(ctx, "cover-owner", "interrupted-delete"); err != nil {
		t.Fatal(err)
	}
	if deleting, err := d.BookIsDeleting(ctx, "cover-owner", "interrupted-delete"); err != nil || !deleting {
		t.Fatalf("reservation active=%v err=%v", deleting, err)
	}
	if err := d.ResetInterruptedBookDeletions(ctx); err != nil {
		t.Fatal(err)
	}
	if deleting, err := d.BookIsDeleting(ctx, "cover-owner", "interrupted-delete"); err != nil || deleting {
		t.Fatalf("interrupted reservation remained active=%v err=%v", deleting, err)
	}
}
