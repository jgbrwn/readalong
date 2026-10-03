package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrCoverTaskLeaseLost = errors.New("cover task lease expired")

type CoverTask struct {
	BookID           string
	OwnerUserID      string
	Title            string
	Author           string
	SourceKind       string
	AudioRelPath     string
	EpubRelPath      string
	EbookJSONRelPath string
	LocalScanNeeded  bool
	EPUBAuthor       string
	BookDescription  string
	SelectedKind     string
	SelectedRelPath  string
	SelectedURL      string
	NoMatchCount     int
	FailureCount     int
	LeaseUntil       string
}

type BookCoverState struct {
	Status            string
	SelectedKind      string
	SelectedRelPath   string
	SelectedURL       string
	SelectedProvider  string
	SelectedYear      int
	CandidateKind     string
	CandidateRelPath  string
	CandidateURL      string
	CandidateProvider string
	CandidateYear     int
	LastCheckedAt     string
	NextCheckAt       string
	NoMatchCount      int
	FailureCount      int
	LookupPaused      bool
}

type CoverLookupCache struct {
	ResultJSON   string
	Found        bool
	NoMatchCount int
	CheckedAt    string
	NextCheckAt  string
}

func (d *DB) CoverLookupCache(ctx context.Context, key, now string) (CoverLookupCache, bool, bool, error) {
	var cache CoverLookupCache
	var found int
	err := d.QueryRowContext(ctx, `SELECT result_json,found,no_match_count,checked_at,next_check_at
		FROM cover_lookup_cache WHERE cache_key=?`, key).
		Scan(&cache.ResultJSON, &found, &cache.NoMatchCount, &cache.CheckedAt, &cache.NextCheckAt)
	if err == sql.ErrNoRows {
		return CoverLookupCache{}, false, false, nil
	}
	if err != nil {
		return CoverLookupCache{}, false, false, err
	}
	cache.Found = found != 0
	return cache, true, cache.NextCheckAt > now, nil
}

func (d *DB) BookCoverIsRunning(ctx context.Context, bookID, now string) (bool, error) {
	var running bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM book_cover_state
		WHERE book_id=? AND lease_until>? )`, bookID, now).Scan(&running)
	return running, err
}

func (d *DB) BookIsDeleting(ctx context.Context, ownerID, bookID string) (bool, error) {
	var deleting bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM book_cover_state c
		JOIN books b ON b.id=c.book_id WHERE c.book_id=? AND b.owner_user_id=? AND c.deleting=1)`,
		bookID, ownerID).Scan(&deleting)
	return deleting, err
}

func (d *DB) BeginBookDeletion(ctx context.Context, ownerID, bookID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists, operationActive, coverActive, deleting, jobActive int
	err = tx.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM books WHERE id=? AND owner_user_id=?),
		EXISTS(SELECT 1 FROM admin_book_operations WHERE book_id=? AND status IN ('queued','running')),
		EXISTS(SELECT 1 FROM book_cover_state WHERE book_id=? AND lease_until>?),
		COALESCE((SELECT deleting FROM book_cover_state WHERE book_id=?),0),
		EXISTS(SELECT 1 FROM jobs WHERE book_id=? AND status='running')`,
		bookID, ownerID, bookID, bookID, now, bookID, bookID).Scan(&exists, &operationActive, &coverActive, &deleting, &jobActive)
	if err != nil {
		return err
	}
	switch {
	case exists == 0:
		return ErrBookNotFound
	case operationActive != 0:
		return ErrBookOperationInProgress
	case coverActive != 0:
		return ErrBookCoverInProgress
	case deleting != 0:
		return ErrBookDeleteInProgress
	case jobActive != 0:
		return ErrBookJobInProgress
	}
	result, err := tx.ExecContext(ctx, `UPDATE book_cover_state SET deleting=1,updated_at=?
		WHERE book_id=? AND deleting=0 AND (lease_until IS NULL OR lease_until<=?)
		AND NOT EXISTS(SELECT 1 FROM admin_book_operations WHERE book_id=? AND status IN ('queued','running'))`,
		now, bookID, now, bookID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrBookCoverInProgress
	}
	return tx.Commit()
}

func (d *DB) CancelBookDeletion(ctx context.Context, ownerID, bookID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.ExecContext(ctx, `UPDATE book_cover_state SET deleting=0,updated_at=?
		WHERE book_id=? AND deleting=1 AND EXISTS(SELECT 1 FROM books WHERE id=? AND owner_user_id=?)`,
		now, bookID, bookID, ownerID)
	return err
}

// ResetInterruptedBookDeletions is called once before the HTTP server starts;
// any persisted reservation then belongs to a process that no longer exists.
func (d *DB) ResetInterruptedBookDeletions(ctx context.Context) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.ExecContext(ctx, `UPDATE book_cover_state SET deleting=0,updated_at=? WHERE deleting=1`, now)
	return err
}

func (d *DB) SetCoverLookupCache(ctx context.Context, key, resultJSON string, found bool,
	noMatchCount int, checkedAt, nextCheckAt string,
) error {
	if key == "" || len(key) > 128 || len(resultJSON) > 32<<10 || noMatchCount < 0 || nextCheckAt <= checkedAt {
		return fmt.Errorf("invalid cover lookup cache entry")
	}
	foundValue := 0
	if found {
		foundValue = 1
	}
	_, err := d.ExecContext(ctx, `INSERT INTO cover_lookup_cache(cache_key,result_json,found,no_match_count,checked_at,next_check_at)
		VALUES(?,?,?,?,?,?) ON CONFLICT(cache_key) DO UPDATE SET result_json=excluded.result_json,
		found=excluded.found,no_match_count=excluded.no_match_count,checked_at=excluded.checked_at,
		next_check_at=excluded.next_check_at`,
		key, resultJSON, foundValue, noMatchCount, checkedAt, nextCheckAt)
	return err
}

func (d *DB) TakeCoverLookupPermit(ctx context.Context, provider, usageDay string, limit int) (bool, error) {
	if provider == "" || len(provider) > 50 || usageDay == "" || limit < 1 {
		return false, fmt.Errorf("invalid cover provider budget")
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO cover_provider_usage(provider,usage_day,request_count)
		VALUES(?,?,0)`, provider, usageDay); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE cover_provider_usage SET request_count=request_count+1
		WHERE provider=? AND usage_day=? AND request_count<?`, provider, usageDay, limit)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return rows == 1, nil
}

func (d *DB) QueueLocalCoverScan(ctx context.Context, bookID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.ExecContext(ctx, `UPDATE book_cover_state SET local_scan_needed=1,next_check_at=?,status='pending',updated_at=?
		WHERE book_id=? AND selected_kind='' AND lookup_paused=0
		AND (candidate_url IS NULL OR candidate_url='') AND (candidate_relpath IS NULL OR candidate_relpath='')`,
		now, now, bookID)
	return err
}

func (d *DB) SetLocalCoverScanComplete(ctx context.Context, task CoverTask) error {
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := d.ExecContext(ctx, `UPDATE book_cover_state SET local_scan_needed=0,updated_at=?
		WHERE book_id=? AND lease_until=? AND EXISTS(SELECT 1 FROM books WHERE id=? AND owner_user_id=?)`,
		now, task.BookID, task.LeaseUntil, task.BookID, task.OwnerUserID)
	return requireCoverLease(result, err)
}

func (d *DB) DeferCoverTask(ctx context.Context, task CoverTask, nextCheckAt string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := d.ExecContext(ctx, `UPDATE book_cover_state SET status=CASE WHEN selected_kind='' THEN 'pending' ELSE 'selected' END,
		local_scan_needed=0,next_check_at=?,lease_until=NULL,updated_at=? WHERE book_id=? AND lease_until=?
		AND EXISTS(SELECT 1 FROM books WHERE id=? AND owner_user_id=?)`,
		nextCheckAt, now, task.BookID, task.LeaseUntil, task.BookID, task.OwnerUserID)
	return requireCoverLease(result, err)
}

func (d *DB) ClaimNextCoverTask(ctx context.Context, now, leaseUntil string) (CoverTask, bool, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return CoverTask{}, false, err
	}
	defer tx.Rollback()
	var task CoverTask
	err = tx.QueryRowContext(ctx, `SELECT b.id,b.owner_user_id,b.title,b.author,b.source_kind,
		COALESCE(b.audio_relpath,''),COALESCE(b.epub_relpath,''),COALESCE(b.ebook_json_relpath,''),
		c.local_scan_needed,
		COALESCE(c.selected_kind,''),COALESCE(c.selected_relpath,''),COALESCE(c.selected_url,''),
		c.no_match_count,c.failure_count
		FROM book_cover_state c JOIN books b ON b.id=c.book_id
		WHERE b.status IN ('ready','error') AND c.deleting=0 AND c.lookup_paused=0
			AND (c.local_scan_needed=1 OR (c.next_check_at IS NOT NULL AND c.next_check_at<=?))
			AND (c.lease_until IS NULL OR c.lease_until<=?)
			AND COALESCE(c.candidate_url,'')='' AND COALESCE(c.candidate_relpath,'')=''
			AND NOT EXISTS(SELECT 1 FROM jobs j WHERE j.book_id=b.id AND j.status IN ('queued','running'))
			AND (b.mode<>'aligned' OR COALESCE(b.epub_relpath,'')<>'' OR NOT EXISTS(
				SELECT 1 FROM jobs j WHERE j.book_id=b.id AND j.status IN ('queued','running')))
			AND NOT EXISTS(SELECT 1 FROM admin_book_operations op WHERE op.book_id=b.id
				AND op.status IN ('queued','running'))
		ORDER BY c.next_check_at,b.created_at LIMIT 1`, now, now).
		Scan(&task.BookID, &task.OwnerUserID, &task.Title, &task.Author, &task.SourceKind,
			&task.AudioRelPath, &task.EpubRelPath, &task.EbookJSONRelPath,
			&task.LocalScanNeeded,
			&task.SelectedKind, &task.SelectedRelPath, &task.SelectedURL,
			&task.NoMatchCount, &task.FailureCount)
	if err == sql.ErrNoRows {
		return CoverTask{}, false, nil
	}
	if err != nil {
		return CoverTask{}, false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE book_cover_state SET status='checking',lease_until=?,updated_at=?
		WHERE book_id=? AND lookup_paused=0 AND (lease_until IS NULL OR lease_until<=?)`,
		leaseUntil, now, task.BookID, now)
	if err != nil {
		return CoverTask{}, false, err
	}
	claimed, err := result.RowsAffected()
	if err != nil || claimed != 1 {
		return CoverTask{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return CoverTask{}, false, err
	}
	task.LeaseUntil = leaseUntil
	return task, true, nil
}

func (d *DB) SetCoverNoMatch(ctx context.Context, task CoverTask, checkedAt, nextCheckAt string, year int) error {
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := d.ExecContext(ctx, `UPDATE book_cover_state SET status=CASE WHEN selected_kind='' THEN 'missing' ELSE 'selected' END,
		local_scan_needed=0,last_checked_at=?,next_check_at=?,no_match_count=no_match_count+1,failure_count=0,
		selected_year=CASE WHEN selected_year=0 AND ? > 0 THEN ? ELSE selected_year END,
		updated_at=? WHERE book_id=? AND lease_until=?
		AND EXISTS(SELECT 1 FROM books WHERE id=? AND owner_user_id=?)`,
		checkedAt, nextCheckAt, year, year, now, task.BookID, task.LeaseUntil, task.BookID, task.OwnerUserID)
	return requireCoverLease(result, err)
}

func (d *DB) SetCoverLookupFailure(ctx context.Context, task CoverTask, checkedAt, nextCheckAt string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := d.ExecContext(ctx, `UPDATE book_cover_state SET status=CASE WHEN selected_kind='' THEN 'error' ELSE 'selected' END,
		local_scan_needed=0,last_checked_at=?,next_check_at=?,failure_count=failure_count+1,lease_until=NULL,updated_at=?
		WHERE book_id=? AND lease_until=? AND EXISTS(SELECT 1 FROM books WHERE id=? AND owner_user_id=?)`,
		checkedAt, nextCheckAt, now, task.BookID, task.LeaseUntil, task.BookID, task.OwnerUserID)
	return requireCoverLease(result, err)
}

func (d *DB) SetLocalCover(ctx context.Context, task CoverTask, kind, relPath string, year int,
	keepChecking bool, nextCheckAt string,
) error {
	now := time.Now().UTC().Format(time.RFC3339)
	paused := 1
	if keepChecking {
		paused = 0
	}
	result, err := d.ExecContext(ctx, `UPDATE book_cover_state SET status='selected',selected_kind=?,selected_relpath=?,
		local_scan_needed=0,selected_url=NULL,selected_provider='',selected_year=?,candidate_kind='',candidate_relpath=NULL,candidate_url=NULL,
		candidate_provider=NULL,candidate_year=0,lookup_paused=?,next_check_at=?,lease_until=NULL,updated_at=?
		WHERE book_id=? AND selected_kind='' AND lease_until=?
		AND EXISTS(SELECT 1 FROM books WHERE id=? AND owner_user_id=?)`,
		kind, relPath, year, paused, nullableString(nextCheckAt), now, task.BookID, task.LeaseUntil,
		task.BookID, task.OwnerUserID)
	return requireCoverLease(result, err)
}

func (d *DB) SetCatalogCover(ctx context.Context, task CoverTask, coverURL, sourceURL string, year int) error {
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := d.ExecContext(ctx, `UPDATE book_cover_state SET status='selected',selected_kind='catalog',
		selected_relpath=NULL,selected_url=?,selected_provider=?,selected_year=?,candidate_kind='',
		candidate_relpath=NULL,candidate_url=NULL,candidate_provider=NULL,candidate_year=0,
		local_scan_needed=0,lookup_paused=1,next_check_at=NULL,lease_until=NULL,last_checked_at=?,failure_count=0,updated_at=?
		WHERE book_id=? AND selected_kind='' AND lease_until=?
		AND EXISTS(SELECT 1 FROM books WHERE id=? AND owner_user_id=?)`,
		coverURL, sourceURL, year, now, now, task.BookID, task.LeaseUntil, task.BookID, task.OwnerUserID)
	return requireCoverLease(result, err)
}

func (d *DB) SetCoverCandidate(ctx context.Context, task CoverTask, coverURL, sourceURL string, year int) error {
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := d.ExecContext(ctx, `UPDATE book_cover_state SET status='review',candidate_kind='catalog',
		local_scan_needed=0,candidate_relpath=NULL,candidate_url=?,candidate_provider=?,candidate_year=?,next_check_at=NULL,
		lease_until=NULL,last_checked_at=?,failure_count=0,updated_at=? WHERE book_id=? AND lease_until=?
		AND EXISTS(SELECT 1 FROM books WHERE id=? AND owner_user_id=?)`,
		coverURL, sourceURL, year, now, now, task.BookID, task.LeaseUntil, task.BookID, task.OwnerUserID)
	return requireCoverLease(result, err)
}

func (d *DB) SetGeneratedCover(ctx context.Context, task CoverTask, relPath string, nextCheckAt string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := d.ExecContext(ctx, `UPDATE book_cover_state SET status='selected',selected_kind='ai_svg',
		local_scan_needed=0,selected_relpath=?,selected_url=NULL,selected_provider='Readalong vector art',next_check_at=?,
		lookup_paused=0,lease_until=NULL,updated_at=? WHERE book_id=? AND selected_kind='' AND lease_until=?
		AND EXISTS(SELECT 1 FROM books WHERE id=? AND owner_user_id=?)`,
		relPath, nullableString(nextCheckAt), now, task.BookID, task.LeaseUntil, task.BookID, task.OwnerUserID)
	return requireCoverLease(result, err)
}

func (d *DB) PauseCoverTask(ctx context.Context, task CoverTask) error {
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := d.ExecContext(ctx, `UPDATE book_cover_state SET status=CASE WHEN selected_kind='' THEN 'missing' ELSE 'selected' END,
		local_scan_needed=0,next_check_at=NULL,lease_until=NULL,updated_at=? WHERE book_id=? AND lease_until=?
		AND EXISTS(SELECT 1 FROM books WHERE id=? AND owner_user_id=?)`,
		now, task.BookID, task.LeaseUntil, task.BookID, task.OwnerUserID)
	return requireCoverLease(result, err)
}

func (d *DB) RetryCoverTaskAt(ctx context.Context, task CoverTask, nextCheckAt string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := d.ExecContext(ctx, `UPDATE book_cover_state SET status=CASE WHEN selected_kind='' THEN 'pending' ELSE 'selected' END,
		local_scan_needed=0,next_check_at=?,lease_until=NULL,updated_at=? WHERE book_id=? AND lease_until=?
		AND EXISTS(SELECT 1 FROM books WHERE id=? AND owner_user_id=?)`,
		nextCheckAt, now, task.BookID, task.LeaseUntil, task.BookID, task.OwnerUserID)
	return requireCoverLease(result, err)
}

func (d *DB) ReleaseCoverTask(ctx context.Context, task CoverTask) error {
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := d.ExecContext(ctx, `UPDATE book_cover_state SET lease_until=NULL,updated_at=?
		WHERE book_id=? AND lease_until=? AND EXISTS(SELECT 1 FROM books WHERE id=? AND owner_user_id=?)`,
		now, task.BookID, task.LeaseUntil, task.BookID, task.OwnerUserID)
	return requireCoverLease(result, err)
}

func requireCoverLease(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrCoverTaskLeaseLost
	}
	return nil
}

func (d *DB) RequeueUnscheduledCoverLookups(ctx context.Context) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.ExecContext(ctx, `UPDATE book_cover_state SET next_check_at=?,status=CASE
		WHEN selected_kind='' THEN 'pending' ELSE 'selected' END,updated_at=?
		WHERE lookup_paused=0 AND next_check_at IS NULL
			AND (candidate_url IS NULL OR candidate_url='')
			AND (candidate_relpath IS NULL OR candidate_relpath='')`, now, now)
	return err
}

func (d *DB) CoverStateForUser(ctx context.Context, ownerID, bookID string) (BookCoverState, error) {
	var state BookCoverState
	var paused int
	err := d.QueryRowContext(ctx, `SELECT c.status,COALESCE(c.selected_kind,''),COALESCE(c.selected_relpath,''),
		COALESCE(c.selected_url,''),COALESCE(c.selected_provider,''),c.selected_year,
		COALESCE(c.candidate_kind,''),COALESCE(c.candidate_relpath,''),COALESCE(c.candidate_url,''),
		COALESCE(c.candidate_provider,''),c.candidate_year,COALESCE(c.last_checked_at,''),
		COALESCE(c.next_check_at,''),c.no_match_count,c.failure_count,c.lookup_paused
		FROM book_cover_state c JOIN books b ON b.id=c.book_id
		WHERE b.owner_user_id=? AND b.id=?`, ownerID, bookID).
		Scan(&state.Status, &state.SelectedKind, &state.SelectedRelPath, &state.SelectedURL,
			&state.SelectedProvider, &state.SelectedYear, &state.CandidateKind, &state.CandidateRelPath,
			&state.CandidateURL, &state.CandidateProvider, &state.CandidateYear,
			&state.LastCheckedAt, &state.NextCheckAt, &state.NoMatchCount, &state.FailureCount, &paused)
	if err != nil {
		return BookCoverState{}, ErrBookNotFound
	}
	state.LookupPaused = paused != 0
	return state, nil
}

func (d *DB) ChooseCoverCandidate(ctx context.Context, ownerID, bookID string, chooseCandidate bool) error {
	now := time.Now().UTC().Format(time.RFC3339)
	var query string
	if chooseCandidate {
		query = `UPDATE book_cover_state SET
			selected_kind=candidate_kind,selected_relpath=candidate_relpath,selected_url=candidate_url,
			selected_provider=candidate_provider,selected_year=candidate_year,
			candidate_kind='',candidate_relpath=NULL,candidate_url=NULL,candidate_provider=NULL,candidate_year=0,
			status='selected',lookup_paused=1,next_check_at=NULL,lease_until=NULL,updated_at=?
			WHERE book_id=? AND (candidate_url IS NOT NULL OR candidate_relpath IS NOT NULL)
				AND EXISTS(SELECT 1 FROM books WHERE id=? AND owner_user_id=?)
				AND deleting=0
				AND NOT EXISTS(SELECT 1 FROM admin_book_operations WHERE book_id=? AND status IN ('queued','running'))`
	} else {
		query = `UPDATE book_cover_state SET candidate_kind='',candidate_relpath=NULL,candidate_url=NULL,
			candidate_provider=NULL,candidate_year=0,status=CASE WHEN selected_kind='' THEN 'missing' ELSE 'selected' END,
			lookup_paused=1,next_check_at=NULL,lease_until=NULL,updated_at=?
			WHERE book_id=? AND (candidate_url IS NOT NULL OR candidate_relpath IS NOT NULL)
				AND EXISTS(SELECT 1 FROM books WHERE id=? AND owner_user_id=?)
				AND deleting=0
				AND NOT EXISTS(SELECT 1 FROM admin_book_operations WHERE book_id=? AND status IN ('queued','running'))`
	}
	result, err := d.ExecContext(ctx, query, now, bookID, bookID, ownerID, bookID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return ErrBookNotFound
	}
	return nil
}
