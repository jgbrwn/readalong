package bookops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jgbrwn/readalong/internal/config"
	"github.com/jgbrwn/readalong/internal/db"
	"github.com/jgbrwn/readalong/internal/pipeline"
)

func operationFixture(t *testing.T, mode string) (*db.DB, string, db.AdminBookOperation, string) {
	t.Helper()
	dataDir := t.TempDir()
	database, err := db.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range []struct{ id, email string }{
		{"owner", "owner@example.org"}, {"target", "target@example.org"}, {"admin", "admin@example.org"},
	} {
		if _, err := database.UpsertUser(context.Background(), user.id, user.email, false, false); err != nil {
			database.Close()
			t.Fatal(err)
		}
	}
	bookID := "book-original"
	bookDir := pipeline.BookDirectory(dataDir, "owner", bookID)
	if err := os.MkdirAll(filepath.Join(bookDir, "source"), 0700); err != nil {
		database.Close()
		t.Fatal(err)
	}
	audioPath := filepath.Join(bookDir, "playback.mp3")
	transcriptPath := filepath.Join(bookDir, "transcript.json.gz")
	coverPath := filepath.Join(bookDir, "cover", "generated.svg")
	coverCandidatePath := filepath.Join(bookDir, "cover", "generated-review.svg")
	for path, content := range map[string]string{
		audioPath: "audio-bytes", transcriptPath: "transcript-bytes", coverPath: "<svg>cover</svg>",
		coverCandidatePath: "<svg>candidate</svg>",
		filepath.Join(bookDir, "source", "original.m4b"): "source-bytes",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			database.Close()
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			database.Close()
			t.Fatal(err)
		}
	}
	rel := func(path string) string {
		value, err := filepath.Rel(dataDir, path)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := database.ExecContext(context.Background(), `INSERT INTO books
		(id,owner_user_id,title,author,source_kind,mode,status,duration_ms,audio_relpath,transcript_relpath,created_at,updated_at)
		VALUES(?,?,?,?,?,'transcript','ready',1000,?,?,?,?)`,
		bookID, "owner", "Example Book", "A. Writer", "upload",
		rel(audioPath), rel(transcriptPath), now, now); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if _, err := database.ExecContext(context.Background(), `INSERT INTO jobs
		(id,owner_user_id,book_id,kind,status,stage,progress,created_at,updated_at)
		VALUES('book-job','owner',?,'book','completed','ready',1,?,?)`, bookID, now, now); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if _, err := database.ExecContext(context.Background(), `INSERT INTO book_cover_state
		(book_id,status,selected_kind,selected_relpath,selected_provider,ai_candidate_relpath,
		 ai_candidate_year,lookup_paused,updated_at)
		VALUES(?,'review','ai_svg',?,'Readalong vector art',?,1935,0,?)`,
		bookID, rel(coverPath), rel(coverCandidatePath), now); err != nil {
		database.Close()
		t.Fatal(err)
	}
	operationID := "operation-" + mode
	targetBookID := bookID
	if mode == "clone" {
		targetBookID = "book-clone"
	}
	operation := db.AdminBookOperation{
		ID: operationID, ActorUserID: "admin", Mode: mode, BookID: bookID,
		SourceOwnerUserID: "owner", TargetOwnerUserID: "target", TargetBookID: targetBookID,
	}
	if err := database.QueueAdminBookOperation(context.Background(), operation); err != nil {
		database.Close()
		t.Fatal(err)
	}
	return database, dataDir, operation, bookDir
}

func runOperation(t *testing.T, database *db.DB, dataDir string, operation db.AdminBookOperation) db.AdminBookOperation {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		New(config.Config{DataDir: dataDir}, database).Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, err := database.AdminBookOperation(context.Background(), operation.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == "completed" || current.Status == "error" {
			return current
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("book operation did not finish")
	return db.AdminBookOperation{}
}

func TestCloneCopiesArtifactsIndependentlyAndKeepsSource(t *testing.T) {
	database, dataDir, operation, sourceDir := operationFixture(t, "clone")
	defer database.Close()
	result := runOperation(t, database, dataDir, operation)
	if result.Status != "completed" {
		t.Fatalf("clone failed: %#v", result)
	}
	cloned, err := database.BookForUser(context.Background(), "target", operation.TargetBookID)
	if err != nil {
		t.Fatal(err)
	}
	source, err := database.BookForUser(context.Background(), "owner", operation.BookID)
	if err != nil || source.ID != operation.BookID {
		t.Fatalf("source book disappeared: %#v err=%v", source, err)
	}
	targetDir := pipeline.BookDirectory(dataDir, "target", operation.TargetBookID)
	targetCover := filepath.Join(targetDir, "cover", "generated.svg")
	targetCoverCandidate := filepath.Join(targetDir, "cover", "generated-review.svg")
	targetAudio := filepath.Join(targetDir, "playback.mp3")
	for _, path := range []string{targetCover, targetCoverCandidate, targetAudio, filepath.Join(targetDir, "source", "original.m4b")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("clone artifact %q missing: %v", path, err)
		}
	}
	if cloned.CoverKind != "ai_svg" || cloned.CoverURL != "/api/books/book-clone/cover/selected" ||
		cloned.CoverAICandidateURL != "/api/books/book-clone/cover/ai-candidate" ||
		!cloned.CoverReviewNeeded {
		t.Fatalf("cover state was not cloned: %#v", cloned)
	}
	if err := os.WriteFile(targetAudio, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	sourceAudio := filepath.Join(sourceDir, "playback.mp3")
	content, err := os.ReadFile(sourceAudio)
	if err != nil || string(content) != "audio-bytes" {
		t.Fatalf("clone shares or changed source audio: %q err=%v", content, err)
	}
	if job, err := database.JobStatus(context.Background(), operation.TargetBookID); err != nil ||
		job.Kind != "book" || job.Status != "completed" {
		t.Fatalf("clone completion marker=%#v err=%v", job, err)
	}
}

func TestTransferMovesOwnerPathsAndRemovesFormerOwnerDirectory(t *testing.T) {
	database, dataDir, operation, sourceDir := operationFixture(t, "transfer")
	defer database.Close()
	result := runOperation(t, database, dataDir, operation)
	if result.Status != "completed" {
		t.Fatalf("transfer failed: %#v", result)
	}
	if _, err := database.BookForUser(context.Background(), "owner", operation.BookID); !errors.Is(err, db.ErrBookNotFound) {
		t.Fatalf("former owner retained book access: %v", err)
	}
	transferred, err := database.BookForUser(context.Background(), "target", operation.BookID)
	if err != nil {
		t.Fatal(err)
	}
	targetBookDir, err := filepath.Rel(dataDir, pipeline.BookDirectory(dataDir, "target", operation.BookID))
	if err != nil {
		t.Fatal(err)
	}
	if transferred.AudioRelPath != filepath.Join(targetBookDir, "playback.mp3") {
		t.Fatalf("transferred path was not rewritten: %q", transferred.AudioRelPath)
	}
	if transferred.CoverKind != "ai_svg" || transferred.CoverURL != "/api/books/"+operation.BookID+"/cover/selected" ||
		transferred.CoverAICandidateURL != "/api/books/"+operation.BookID+"/cover/ai-candidate" ||
		!transferred.CoverReviewNeeded {
		t.Fatalf("cover state was not transferred: %#v", transferred)
	}
	if _, err := os.Stat(sourceDir); !os.IsNotExist(err) {
		t.Fatalf("former owner files were not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pipeline.BookDirectory(dataDir, "target", operation.BookID), "cover", "generated.svg")); err != nil {
		t.Fatalf("transferred cover missing: %v", err)
	}
}

func TestAdminCopyRejectsSymlinksWithoutDamagingSource(t *testing.T) {
	database, dataDir, operation, sourceDir := operationFixture(t, "clone")
	defer database.Close()
	if err := os.Symlink("/etc/passwd", filepath.Join(sourceDir, "source", "unexpected-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	result := runOperation(t, database, dataDir, operation)
	if result.Status != "error" {
		t.Fatalf("symlink copy status=%q, want error", result.Status)
	}
	if _, err := os.Stat(filepath.Join(sourceDir, "playback.mp3")); err != nil {
		t.Fatalf("source was damaged: %v", err)
	}
	if _, err := database.BookForUser(context.Background(), "target", operation.TargetBookID); err == nil {
		t.Fatal("unsafe clone was published")
	}
}

func TestInterruptedTransferRecoversOrRollsOwnershipBack(t *testing.T) {
	for _, invalidateDestination := range []bool{false, true} {
		name := "valid destination"
		if invalidateDestination {
			name = "invalid destination"
		}
		t.Run(name, func(t *testing.T) {
			database, dataDir, operation, sourceDir := operationFixture(t, "transfer")
			defer database.Close()
			if _, found, err := database.ClaimNextAdminBookOperation(context.Background()); err != nil || !found {
				t.Fatalf("claim found=%v err=%v", found, err)
			}
			book, err := database.BookByID(context.Background(), operation.BookID)
			if err != nil {
				t.Fatal(err)
			}
			destinationDir := pipeline.BookDirectory(dataDir, operation.TargetOwnerUserID, operation.TargetBookID)
			paths, err := remapBookPaths(dataDir, sourceDir, destinationDir, book)
			if err != nil {
				t.Fatal(err)
			}
			cover, err := database.CoverStateForUser(context.Background(), operation.SourceOwnerUserID, operation.BookID)
			if err != nil {
				t.Fatal(err)
			}
			paths.CoverSelected, err = remapRelativePath(dataDir, sourceDir, destinationDir, cover.SelectedRelPath)
			if err != nil {
				t.Fatal(err)
			}
			paths.CoverAICandidate, err = remapRelativePath(dataDir, sourceDir, destinationDir, cover.AICandidateRelPath)
			if err != nil {
				t.Fatal(err)
			}
			files, _, err := fileManifest(dataDir, sourceDir, []string{
				book.AudioRelPath, book.TranscriptRelPath, cover.SelectedRelPath, cover.AICandidateRelPath,
			})
			if err != nil {
				t.Fatal(err)
			}
			stageDir := filepath.Join(filepath.Dir(destinationDir), ".stage-"+operation.ID)
			if err := os.MkdirAll(stageDir, 0700); err != nil {
				t.Fatal(err)
			}
			for _, file := range files {
				if err := copyBookFile(context.Background(), sourceDir, stageDir, file); err != nil {
					t.Fatal(err)
				}
			}
			if err := writeSyncedFile(filepath.Join(stageDir, operationMarker), []byte(operation.ID)); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(destinationDir), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(stageDir, destinationDir); err != nil {
				t.Fatal(err)
			}
			if invalidateDestination {
				if err := os.Remove(filepath.Join(destinationDir, "transcript.json.gz")); err != nil {
					t.Fatal(err)
				}
			}
			if err := database.TransferReadyBookForAdmin(context.Background(), operation, paths); err != nil {
				t.Fatal(err)
			}

			runOperation(t, database, dataDir, operation)
			finished, err := database.AdminBookOperation(context.Background(), operation.ID)
			if err != nil {
				t.Fatal(err)
			}
			if invalidateDestination {
				if finished.Status != "error" {
					t.Fatalf("invalid destination operation status=%q", finished.Status)
				}
				if _, err := database.BookForUser(context.Background(), operation.SourceOwnerUserID, operation.BookID); err != nil {
					t.Fatalf("source owner was not restored: %v", err)
				}
				if _, err := database.BookForUser(context.Background(), operation.TargetOwnerUserID, operation.BookID); err == nil {
					t.Fatal("recipient kept an invalid transferred book")
				}
				if _, err := os.Stat(sourceDir); err != nil {
					t.Fatalf("original files were removed: %v", err)
				}
			} else {
				if finished.Status != "completed" {
					t.Fatalf("valid destination operation status=%q error=%q", finished.Status, finished.Error)
				}
				if _, err := os.Stat(sourceDir); !os.IsNotExist(err) {
					t.Fatalf("valid transfer left source files: %v", err)
				}
			}
		})
	}
}
