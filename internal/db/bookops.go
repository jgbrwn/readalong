package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrAdminBookOperationUnavailable = errors.New("book cannot be copied in its current state")

type AdminBookOperation struct {
	ID                string `json:"id"`
	ActorUserID       string `json:"actor_user_id"`
	Mode              string `json:"mode"`
	BookID            string `json:"book_id"`
	SourceOwnerUserID string `json:"source_owner_user_id"`
	TargetOwnerUserID string `json:"target_owner_user_id"`
	TargetBookID      string `json:"target_book_id"`
	Status            string `json:"status"`
	Stage             string `json:"stage"`
	CopiedBytes       int64  `json:"copied_bytes"`
	TotalBytes        int64  `json:"total_bytes"`
	Error             string `json:"error,omitempty"`
	CreatedAt         string `json:"created_at"`
	UpdatedAt         string `json:"updated_at"`
}

type BookArtifactPaths struct {
	Audio, EPUB, EbookJSON, Transcript, Alignment string
	CoverSelected, CoverCandidate                 string
}

func (d *DB) CloneReadyBookForAdmin(ctx context.Context, operation AdminBookOperation, paths BookArtifactPaths) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_book_operations op
		JOIN users u ON u.id=op.target_owner_user_id
		WHERE op.id=? AND op.mode='clone' AND op.status='running' AND u.status='active'`, operation.ID).Scan(&active); err != nil || active != 1 {
		return ErrAdminBookOperationUnavailable
	}
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := tx.ExecContext(ctx, `INSERT INTO books
		(id,owner_user_id,title,author,source_kind,source_url,mode,status,duration_ms,audio_relpath,epub_relpath,
		 ebook_json_relpath,transcript_relpath,alignment_relpath,alignment_quality,gutenberg_id,ebook_source_url,created_at,updated_at)
		SELECT ?,?,title,author,source_kind,NULL,mode,'ready',duration_ms,?,?,?,?,?,alignment_quality,gutenberg_id,ebook_source_url,?,?
		FROM books WHERE id=? AND owner_user_id=? AND status='ready'
			AND EXISTS(SELECT 1 FROM jobs WHERE book_id=? AND owner_user_id=? AND kind='book' AND status='completed')
			AND NOT EXISTS(SELECT 1 FROM jobs WHERE book_id=? AND status IN ('queued','running'))
			AND NOT EXISTS(SELECT 1 FROM book_cover_state WHERE book_id=? AND deleting=1)`,
		operation.TargetBookID, operation.TargetOwnerUserID,
		nullableString(paths.Audio), nullableString(paths.EPUB), nullableString(paths.EbookJSON),
		nullableString(paths.Transcript), nullableString(paths.Alignment),
		now, now,
		operation.BookID, operation.SourceOwnerUserID, operation.BookID, operation.SourceOwnerUserID,
		operation.BookID, operation.BookID)
	if err != nil {
		return err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if inserted != 1 {
		return ErrAdminBookOperationUnavailable
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs
		(id,owner_user_id,book_id,kind,status,stage,progress,created_at,updated_at)
		VALUES(?,?,?,'book','completed','ready',1,?,?)`,
		operation.TargetBookID+"-clone", operation.TargetOwnerUserID, operation.TargetBookID, now, now); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT ordinal,title,start_ms,end_ms,transcript_state,alignment_quality
		FROM chapters WHERE book_id=? ORDER BY ordinal`, operation.BookID)
	if err != nil {
		return err
	}
	type chapterCopy struct {
		ordinal int
		title   string
		start   int64
		end     int64
		state   string
		quality sql.NullFloat64
	}
	var chapters []chapterCopy
	for rows.Next() {
		var chapter chapterCopy
		if err := rows.Scan(&chapter.ordinal, &chapter.title, &chapter.start, &chapter.end, &chapter.state, &chapter.quality); err != nil {
			rows.Close()
			return err
		}
		chapters = append(chapters, chapter)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, chapter := range chapters {
		id := fmt.Sprintf("%s-ch-%06d", operation.TargetBookID, chapter.ordinal)
		if _, err := tx.ExecContext(ctx, `INSERT INTO chapters(id,book_id,ordinal,title,start_ms,end_ms,transcript_state,alignment_quality)
			VALUES(?,?,?,?,?,?,?,?)`, id, operation.TargetBookID, chapter.ordinal, chapter.title,
			chapter.start, chapter.end, chapter.state, nullableFloat(chapter.quality)); err != nil {
			return err
		}
	}
	var cover BookCoverState
	var lookupPaused int
	if err := tx.QueryRowContext(ctx, `SELECT status,selected_kind,COALESCE(selected_relpath,''),
		COALESCE(selected_url,''),COALESCE(selected_provider,''),selected_year,candidate_kind,
		COALESCE(candidate_relpath,''),COALESCE(candidate_url,''),COALESCE(candidate_provider,''),
		candidate_year,COALESCE(last_checked_at,''),COALESCE(next_check_at,''),no_match_count,
		failure_count,lookup_paused FROM book_cover_state WHERE book_id=?`, operation.BookID).
		Scan(&cover.Status, &cover.SelectedKind, &cover.SelectedRelPath, &cover.SelectedURL,
			&cover.SelectedProvider, &cover.SelectedYear, &cover.CandidateKind, &cover.CandidateRelPath,
			&cover.CandidateURL, &cover.CandidateProvider, &cover.CandidateYear,
			&cover.LastCheckedAt, &cover.NextCheckAt, &cover.NoMatchCount, &cover.FailureCount, &lookupPaused); err != nil {
		if err != sql.ErrNoRows {
			return err
		}
	} else {
		status := cover.Status
		if status == "checking" {
			status = "pending"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO book_cover_state
			(book_id,status,selected_kind,selected_relpath,selected_url,selected_provider,selected_year,
			 candidate_kind,candidate_relpath,candidate_url,candidate_provider,candidate_year,last_checked_at,
			 next_check_at,no_match_count,failure_count,lookup_paused,lease_until,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULL,?)`,
			operation.TargetBookID, status, cover.SelectedKind, nullableString(paths.CoverSelected),
			nullableString(cover.SelectedURL), nullableString(cover.SelectedProvider), cover.SelectedYear,
			cover.CandidateKind, nullableString(paths.CoverCandidate), nullableString(cover.CandidateURL),
			nullableString(cover.CandidateProvider), cover.CandidateYear, nullableString(cover.LastCheckedAt),
			nullableString(cover.NextCheckAt), cover.NoMatchCount, cover.FailureCount, lookupPaused, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) TransferReadyBookForAdmin(ctx context.Context, operation AdminBookOperation, paths BookArtifactPaths) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_book_operations op
		JOIN users u ON u.id=op.target_owner_user_id
		WHERE op.id=? AND op.mode='transfer' AND op.status='running' AND u.status='active'`, operation.ID).Scan(&active); err != nil || active != 1 {
		return ErrAdminBookOperationUnavailable
	}
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := tx.ExecContext(ctx, `UPDATE books SET owner_user_id=?,source_url=NULL,
		audio_relpath=?,epub_relpath=?,ebook_json_relpath=?,transcript_relpath=?,alignment_relpath=?,updated_at=?
		WHERE id=? AND owner_user_id=? AND status='ready'
		AND EXISTS(SELECT 1 FROM jobs WHERE book_id=? AND owner_user_id=? AND kind='book' AND status='completed')
		AND NOT EXISTS(SELECT 1 FROM jobs WHERE book_id=? AND status IN ('queued','running'))
		AND NOT EXISTS(SELECT 1 FROM book_cover_state WHERE book_id=? AND deleting=1)
		AND EXISTS(SELECT 1 FROM users WHERE id=? AND status='active')`,
		operation.TargetOwnerUserID, nullableString(paths.Audio), nullableString(paths.EPUB),
		nullableString(paths.EbookJSON), nullableString(paths.Transcript), nullableString(paths.Alignment), now,
		operation.BookID, operation.SourceOwnerUserID, operation.BookID, operation.SourceOwnerUserID,
		operation.BookID, operation.BookID, operation.TargetOwnerUserID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrAdminBookOperationUnavailable
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET owner_user_id=? WHERE book_id=?`,
		operation.TargetOwnerUserID, operation.BookID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE book_cover_state SET selected_relpath=?,candidate_relpath=?,
		lease_until=NULL,updated_at=? WHERE book_id=?`, nullableString(paths.CoverSelected),
		nullableString(paths.CoverCandidate), now, operation.BookID); err != nil {
		return err
	}
	return tx.Commit()
}

func nullableFloat(value sql.NullFloat64) any {
	if value.Valid {
		return value.Float64
	}
	return nil
}

func (d *DB) QueueAdminBookOperation(ctx context.Context, operation AdminBookOperation) error {
	if operation.ID == "" || operation.ActorUserID == "" || operation.BookID == "" ||
		operation.SourceOwnerUserID == "" || operation.TargetOwnerUserID == "" ||
		operation.TargetBookID == "" || (operation.Mode != "clone" && operation.Mode != "transfer") ||
		operation.SourceOwnerUserID == operation.TargetOwnerUserID {
		return ErrAdminBookOperationUnavailable
	}
	if operation.Mode == "transfer" && operation.TargetBookID != operation.BookID {
		return ErrAdminBookOperationUnavailable
	}
	if operation.Mode == "clone" && operation.TargetBookID == operation.BookID {
		return ErrAdminBookOperationUnavailable
	}
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := d.ExecContext(ctx, `INSERT INTO admin_book_operations
		(id,actor_user_id,mode,book_id,source_owner_user_id,target_owner_user_id,target_book_id,status,stage,created_at,updated_at)
		SELECT ?,?,?,?,?,?,?,'queued','queued',?,?
		WHERE EXISTS(SELECT 1 FROM users WHERE id=? AND status='active')
		AND EXISTS(SELECT 1 FROM books b WHERE b.id=? AND b.owner_user_id=? AND b.status='ready'
			AND COALESCE(b.audio_relpath,'')<>'' AND COALESCE(b.transcript_relpath,'')<>'')
		AND EXISTS(SELECT 1 FROM jobs j WHERE j.book_id=? AND j.owner_user_id=? AND j.kind='book' AND j.status='completed')
		AND NOT EXISTS(SELECT 1 FROM jobs j WHERE j.book_id=? AND j.status IN ('queued','running'))
		AND NOT EXISTS(SELECT 1 FROM book_cover_state c WHERE c.book_id=? AND c.deleting=1)
		AND NOT EXISTS(SELECT 1 FROM book_cover_state c WHERE c.book_id=? AND c.lease_until>?)
		AND NOT EXISTS(SELECT 1 FROM admin_book_operations o WHERE o.book_id=? AND o.status IN ('queued','running'))`,
		operation.ID, operation.ActorUserID, operation.Mode, operation.BookID, operation.SourceOwnerUserID,
		operation.TargetOwnerUserID, operation.TargetBookID, now, now, operation.TargetOwnerUserID,
		operation.BookID, operation.SourceOwnerUserID, operation.BookID, operation.SourceOwnerUserID,
		operation.BookID, operation.BookID, operation.BookID, now, operation.BookID)
	if err != nil {
		return err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if inserted != 1 {
		return ErrAdminBookOperationUnavailable
	}
	return nil
}

func (d *DB) ClaimNextAdminBookOperation(ctx context.Context) (AdminBookOperation, bool, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return AdminBookOperation{}, false, err
	}
	defer tx.Rollback()
	var operation AdminBookOperation
	err = tx.QueryRowContext(ctx, `SELECT id,actor_user_id,mode,book_id,source_owner_user_id,target_owner_user_id,
		target_book_id,status,stage,copied_bytes,total_bytes,COALESCE(error,''),created_at,updated_at
		FROM admin_book_operations WHERE status='queued' ORDER BY created_at,rowid LIMIT 1`).
		Scan(&operation.ID, &operation.ActorUserID, &operation.Mode, &operation.BookID,
			&operation.SourceOwnerUserID, &operation.TargetOwnerUserID, &operation.TargetBookID,
			&operation.Status, &operation.Stage, &operation.CopiedBytes, &operation.TotalBytes,
			&operation.Error, &operation.CreatedAt, &operation.UpdatedAt)
	if err == sql.ErrNoRows {
		return AdminBookOperation{}, false, nil
	}
	if err != nil {
		return AdminBookOperation{}, false, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	stage := "validating"
	if operation.Stage == "cleanup" {
		stage = "cleanup"
	}
	result, err := tx.ExecContext(ctx, `UPDATE admin_book_operations SET status='running',stage=?,error=NULL,updated_at=?
		WHERE id=? AND status='queued'`, stage, now, operation.ID)
	if err != nil {
		return AdminBookOperation{}, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return AdminBookOperation{}, false, err
	}
	if rows == 0 {
		return AdminBookOperation{}, false, nil
	}
	if err := tx.Commit(); err != nil {
		return AdminBookOperation{}, false, err
	}
	operation.Status, operation.Stage = "running", stage
	return operation, true, nil
}

func (d *DB) UpdateAdminBookOperation(ctx context.Context, id, stage string, copiedBytes, totalBytes int64) error {
	if copiedBytes < 0 || totalBytes < 0 || copiedBytes > totalBytes && totalBytes != 0 {
		return fmt.Errorf("invalid book operation progress")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.ExecContext(ctx, `UPDATE admin_book_operations SET stage=?,copied_bytes=?,total_bytes=?,updated_at=?
		WHERE id=? AND status='running'`, stage, copiedBytes, totalBytes, now, id)
	return err
}

func (d *DB) CompleteAdminBookOperation(ctx context.Context, id string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var mode, bookID string
	if err := tx.QueryRowContext(ctx, `SELECT mode,book_id FROM admin_book_operations
		WHERE id=? AND status='running'`, id).Scan(&mode, &bookID); err != nil {
		return err
	}
	if mode == "transfer" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM reading_progress WHERE book_id=?`, bookID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM chunks WHERE book_id=?`, bookID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE admin_book_operations SET status='completed',stage='ready',error=NULL,
		copied_bytes=total_bytes,updated_at=? WHERE id=? AND status='running'`, now, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) MarkAdminBookCleanup(ctx context.Context, id string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.ExecContext(ctx, `UPDATE admin_book_operations SET status='queued',stage='cleanup',updated_at=?
		WHERE id=? AND mode='transfer' AND status='running'`, now, id)
	return err
}

func (d *DB) RollbackAdminBookTransfer(ctx context.Context, operation AdminBookOperation, paths BookArtifactPaths) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_book_operations
		WHERE id=? AND mode='transfer' AND status='running'`, operation.ID).Scan(&active); err != nil || active != 1 {
		return ErrAdminBookOperationUnavailable
	}
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := tx.ExecContext(ctx, `UPDATE books SET owner_user_id=?,
		audio_relpath=?,epub_relpath=?,ebook_json_relpath=?,transcript_relpath=?,alignment_relpath=?,updated_at=?
		WHERE id=? AND owner_user_id=?`,
		operation.SourceOwnerUserID, nullableString(paths.Audio), nullableString(paths.EPUB),
		nullableString(paths.EbookJSON), nullableString(paths.Transcript), nullableString(paths.Alignment), now,
		operation.BookID, operation.TargetOwnerUserID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return ErrAdminBookOperationUnavailable
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET owner_user_id=? WHERE book_id=?`,
		operation.SourceOwnerUserID, operation.BookID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE book_cover_state SET selected_relpath=?,candidate_relpath=?,
		updated_at=? WHERE book_id=?`, nullableString(paths.CoverSelected),
		nullableString(paths.CoverCandidate), now, operation.BookID); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) AbortAdminBookClone(ctx context.Context, operation AdminBookOperation) error {
	res, err := d.ExecContext(ctx, `DELETE FROM books WHERE id=? AND owner_user_id=?
		AND EXISTS(SELECT 1 FROM admin_book_operations WHERE id=? AND mode='clone' AND status='running')`,
		operation.TargetBookID, operation.TargetOwnerUserID, operation.ID)
	if err != nil {
		return err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrAdminBookOperationUnavailable
	}
	return nil
}

func (d *DB) FailAdminBookOperation(ctx context.Context, id, message string) error {
	if len(message) > 500 {
		message = message[:500]
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.ExecContext(ctx, `UPDATE admin_book_operations SET status='error',stage='error',error=?,updated_at=?
		WHERE id=? AND status='running'`, message, now, id)
	return err
}

func (d *DB) ResetInterruptedAdminBookOperations(ctx context.Context) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.ExecContext(ctx, `UPDATE admin_book_operations SET status='queued',stage='queued',updated_at=?
		WHERE status='running'`, now)
	return err
}

func (d *DB) AdminBookOperation(ctx context.Context, id string) (AdminBookOperation, error) {
	var operation AdminBookOperation
	err := d.QueryRowContext(ctx, `SELECT id,actor_user_id,mode,book_id,source_owner_user_id,target_owner_user_id,
		target_book_id,status,stage,copied_bytes,total_bytes,COALESCE(error,''),created_at,updated_at
		FROM admin_book_operations WHERE id=?`, id).
		Scan(&operation.ID, &operation.ActorUserID, &operation.Mode, &operation.BookID,
			&operation.SourceOwnerUserID, &operation.TargetOwnerUserID, &operation.TargetBookID,
			&operation.Status, &operation.Stage, &operation.CopiedBytes, &operation.TotalBytes,
			&operation.Error, &operation.CreatedAt, &operation.UpdatedAt)
	return operation, err
}

func (d *DB) BookHasAdminOperation(ctx context.Context, bookID string) (bool, error) {
	var exists bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM admin_book_operations
		WHERE book_id=? AND status IN ('queued','running'))`, bookID).Scan(&exists)
	return exists, err
}
