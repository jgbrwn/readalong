package db

import (
	"context"
	"errors"
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
	if err := d.ChooseCoverCandidate(ctx, "cover-owner", "cover-book", true); err != nil {
		t.Fatal(err)
	}
	state, err := d.CoverStateForUser(ctx, "cover-owner", "cover-book")
	if err != nil || state.SelectedKind != "catalog" || state.SelectedYear != 1920 ||
		state.SelectedURL != "https://covers.openlibrary.org/b/id/77-M.jpg?default=false" ||
		state.CandidateURL != "" || !state.LookupPaused {
		t.Fatalf("selected cover state=%#v err=%v", state, err)
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
