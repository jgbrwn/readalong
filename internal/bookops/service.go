package bookops

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jgbrwn/readalong/internal/config"
	"github.com/jgbrwn/readalong/internal/db"
	"github.com/jgbrwn/readalong/internal/pipeline"
)

const (
	maxBookCopyBytes = int64(8 << 30)
	maxBookCopyFiles = 200_000
	operationMarker  = ".readalong-operation"
)

type Service struct {
	cfg config.Config
	db  *db.DB
}

func New(cfg config.Config, database *db.DB) *Service {
	return &Service{cfg: cfg.Normalize(), db: database}
}

func (s *Service) Run(ctx context.Context) {
	if err := s.db.ResetInterruptedAdminBookOperations(ctx); err != nil {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		operation, found, err := s.db.ClaimNextAdminBookOperation(ctx)
		if err != nil {
			if !wait(ctx, ticker.C) {
				return
			}
			continue
		}
		if found {
			s.process(ctx, operation)
			continue
		}
		if !wait(ctx, ticker.C) {
			return
		}
	}
}

func wait(ctx context.Context, tick <-chan time.Time) bool {
	select {
	case <-ctx.Done():
		return false
	case <-tick:
		return true
	}
}

func (s *Service) process(ctx context.Context, operation db.AdminBookOperation) {
	if s.recoverPublished(ctx, operation) {
		return
	}
	book, err := s.db.BookByID(ctx, operation.BookID)
	if err != nil || book.OwnerUserID != operation.SourceOwnerUserID || book.Status != "ready" {
		s.fail(ctx, operation.ID, "The source book changed before it could be copied.")
		return
	}
	if book.AudioRelPath == "" || book.TranscriptRelPath == "" {
		s.fail(ctx, operation.ID, "Only books with saved audio and a transcript can be copied.")
		return
	}

	sourceDir := pipeline.BookDirectory(s.cfg.DataDir, operation.SourceOwnerUserID, operation.BookID)
	destinationDir := pipeline.BookDirectory(s.cfg.DataDir, operation.TargetOwnerUserID, operation.TargetBookID)
	stageDir := filepath.Join(filepath.Dir(destinationDir), ".stage-"+operation.ID)
	if err := os.RemoveAll(stageDir); err != nil {
		s.fail(ctx, operation.ID, "Could not prepare the destination for this book.")
		return
	}
	if _, err := os.Lstat(destinationDir); err == nil {
		if !hasOperationMarker(destinationDir, operation.ID) {
			s.fail(ctx, operation.ID, "A destination folder already exists; it was left untouched.")
			return
		}
		if err := os.RemoveAll(destinationDir); err != nil {
			s.fail(ctx, operation.ID, "Could not clean up a previous interrupted copy.")
			return
		}
	} else if !os.IsNotExist(err) {
		s.fail(ctx, operation.ID, "Could not inspect the destination folder.")
		return
	}

	paths, err := remapBookPaths(s.cfg.DataDir, sourceDir, destinationDir, book)
	if err != nil {
		s.fail(ctx, operation.ID, "The source book contains an invalid artifact path.")
		return
	}
	coverState, err := s.db.CoverStateForUser(ctx, operation.SourceOwnerUserID, operation.BookID)
	if err != nil {
		s.fail(ctx, operation.ID, "The source book cover state could not be loaded.")
		return
	}
	if paths.CoverSelected, err = remapRelativePath(s.cfg.DataDir, sourceDir, destinationDir, coverState.SelectedRelPath); err != nil {
		s.fail(ctx, operation.ID, "The selected cover path is invalid.")
		return
	}
	if paths.CoverCandidate, err = remapRelativePath(s.cfg.DataDir, sourceDir, destinationDir, coverState.CandidateRelPath); err != nil {
		s.fail(ctx, operation.ID, "The candidate cover path is invalid.")
		return
	}
	if paths.CoverAICandidate, err = remapRelativePath(s.cfg.DataDir, sourceDir, destinationDir, coverState.AICandidateRelPath); err != nil {
		s.fail(ctx, operation.ID, "The generated cover candidate path is invalid.")
		return
	}
	files, total, err := fileManifest(s.cfg.DataDir, sourceDir, []string{
		book.AudioRelPath, book.EpubRelPath, book.EbookJSONRelPath, book.TranscriptRelPath,
		book.AlignmentRelPath, coverState.SelectedRelPath, coverState.CandidateRelPath, coverState.AICandidateRelPath,
	})
	if err != nil || total > maxBookCopyBytes {
		s.fail(ctx, operation.ID, "The book is missing files or exceeds the 8 GiB copy limit.")
		return
	}
	if !hasCopyDiskSpace(s.cfg.DataDir, total) {
		s.fail(ctx, operation.ID, "There is not enough free disk space to safely copy this book.")
		return
	}
	if err := s.db.UpdateAdminBookOperation(ctx, operation.ID, "copying", 0, total); err != nil {
		s.fail(ctx, operation.ID, "Could not update copy progress.")
		return
	}
	if err := os.MkdirAll(stageDir, 0700); err != nil {
		s.fail(ctx, operation.ID, "Could not create the private destination folder.")
		return
	}
	var copied int64
	for _, file := range files {
		if err := copyBookFile(ctx, sourceDir, stageDir, file); err != nil {
			_ = os.RemoveAll(stageDir)
			s.fail(ctx, operation.ID, "A book file could not be copied; the original was kept.")
			return
		}
		copied += file.size
		if err := s.db.UpdateAdminBookOperation(ctx, operation.ID, "copying", copied, total); err != nil {
			_ = os.RemoveAll(stageDir)
			s.fail(ctx, operation.ID, "Could not update copy progress.")
			return
		}
	}
	if err := validateStagedArtifacts(s.cfg.DataDir, destinationDir, stageDir, paths); err != nil {
		_ = os.RemoveAll(stageDir)
		s.fail(ctx, operation.ID, "A required book artifact did not pass validation.")
		return
	}
	if err := writeSyncedFile(filepath.Join(stageDir, operationMarker), []byte(operation.ID)); err != nil {
		_ = os.RemoveAll(stageDir)
		s.fail(ctx, operation.ID, "Could not prepare the copy for safe publication.")
		return
	}
	if err := syncTreeDirectories(stageDir); err != nil {
		_ = os.RemoveAll(stageDir)
		s.fail(ctx, operation.ID, "Could not make the staged copy durable.")
		return
	}
	if err := os.MkdirAll(filepath.Dir(destinationDir), 0700); err != nil {
		_ = os.RemoveAll(stageDir)
		s.fail(ctx, operation.ID, "Could not create the destination folder.")
		return
	}
	if err := syncDirectoryAncestors(filepath.Dir(destinationDir), s.cfg.DataDir); err != nil {
		_ = os.RemoveAll(stageDir)
		s.fail(ctx, operation.ID, "The destination parent could not be made durable.")
		return
	}
	if err := os.Rename(stageDir, destinationDir); err != nil {
		_ = os.RemoveAll(stageDir)
		s.fail(ctx, operation.ID, "The copied book could not be published.")
		return
	}
	if err := syncDirectoryAncestors(filepath.Dir(destinationDir), s.cfg.DataDir); err != nil {
		if hasOperationMarker(destinationDir, operation.ID) {
			_ = os.RemoveAll(destinationDir)
		}
		s.fail(ctx, operation.ID, "The destination directory could not be made durable.")
		return
	}

	if err := s.db.UpdateAdminBookOperation(ctx, operation.ID, "publishing", total, total); err != nil {
		_ = os.RemoveAll(destinationDir)
		s.fail(ctx, operation.ID, "Could not publish the book metadata.")
		return
	}
	if operation.Mode == "clone" {
		err = s.db.CloneReadyBookForAdmin(ctx, operation, paths)
	} else {
		err = s.db.TransferReadyBookForAdmin(ctx, operation, paths)
	}
	if err != nil {
		if s.recoverPublished(ctx, operation) {
			return
		}
		if hasOperationMarker(destinationDir, operation.ID) {
			_ = os.RemoveAll(destinationDir)
		}
		s.fail(ctx, operation.ID, "The book metadata changed; no copy was published.")
		return
	}
	if !s.recoverPublished(ctx, operation) {
		s.fail(ctx, operation.ID, "The published book could not be validated; the source was preserved.")
	}
}

func (s *Service) recoverPublished(ctx context.Context, operation db.AdminBookOperation) bool {
	destinationDir := pipeline.BookDirectory(s.cfg.DataDir, operation.TargetOwnerUserID, operation.TargetBookID)
	if operation.Mode == "clone" {
		book, err := s.db.BookByID(ctx, operation.TargetBookID)
		if err != nil || book.OwnerUserID != operation.TargetOwnerUserID {
			return false
		}
		if err == nil {
			_, err = s.validatePublishedArtifacts(ctx, book)
		}
		if err != nil {
			if s.db.AbortAdminBookClone(ctx, operation) == nil {
				_ = os.RemoveAll(destinationDir)
			}
			s.fail(ctx, operation.ID, "The cloned artifacts failed validation; the source was preserved.")
			return true
		}
		_ = os.Remove(filepath.Join(destinationDir, operationMarker))
		if err := s.db.CompleteAdminBookOperation(ctx, operation.ID); err != nil {
			return false
		}
		return true
	}
	book, err := s.db.BookByID(ctx, operation.BookID)
	if err != nil || book.OwnerUserID != operation.TargetOwnerUserID {
		return false
	}
	_, err = s.validatePublishedArtifacts(ctx, book)
	sourceDir := pipeline.BookDirectory(s.cfg.DataDir, operation.SourceOwnerUserID, operation.BookID)
	if err != nil {
		if s.rollbackTransfer(ctx, operation, book, sourceDir, destinationDir) {
			s.fail(ctx, operation.ID, "The transferred copy failed validation; ownership was restored to the source account.")
			return true
		}
		s.fail(ctx, operation.ID, "The transferred copy failed validation; source cleanup was stopped for admin review.")
		return true
	}
	if err := os.RemoveAll(sourceDir); err != nil {
		_ = s.db.MarkAdminBookCleanup(ctx, operation.ID)
		time.Sleep(time.Second)
		return true
	}
	if err := syncDirectoryAncestors(filepath.Dir(sourceDir), s.cfg.DataDir); err != nil {
		_ = s.db.MarkAdminBookCleanup(ctx, operation.ID)
		time.Sleep(time.Second)
		return true
	}
	_ = os.Remove(filepath.Join(destinationDir, operationMarker))
	if err := s.db.CompleteAdminBookOperation(ctx, operation.ID); err != nil {
		_ = s.db.MarkAdminBookCleanup(ctx, operation.ID)
		time.Sleep(time.Second)
	}
	return true
}

func (s *Service) validatePublishedArtifacts(ctx context.Context, book db.Book) (db.BookArtifactPaths, error) {
	bookDir := pipeline.BookDirectory(s.cfg.DataDir, book.OwnerUserID, book.ID)
	paths, err := remapBookPaths(s.cfg.DataDir, bookDir, bookDir, book)
	if err != nil {
		return paths, err
	}
	cover, err := s.db.CoverStateForUser(ctx, book.OwnerUserID, book.ID)
	if err != nil {
		return paths, err
	}
	paths.CoverSelected, err = remapRelativePath(s.cfg.DataDir, bookDir, bookDir, cover.SelectedRelPath)
	if err != nil {
		return paths, err
	}
	paths.CoverCandidate, err = remapRelativePath(s.cfg.DataDir, bookDir, bookDir, cover.CandidateRelPath)
	if err != nil {
		return paths, err
	}
	paths.CoverAICandidate, err = remapRelativePath(s.cfg.DataDir, bookDir, bookDir, cover.AICandidateRelPath)
	if err != nil {
		return paths, err
	}
	return paths, validateStagedArtifacts(s.cfg.DataDir, bookDir, bookDir, paths)
}

func (s *Service) rollbackTransfer(ctx context.Context, operation db.AdminBookOperation, book db.Book,
	sourceDir, destinationDir string,
) bool {
	if book.ID == "" || book.OwnerUserID != operation.TargetOwnerUserID {
		return false
	}
	sourcePaths, err := remapBookPaths(s.cfg.DataDir, destinationDir, sourceDir, book)
	if err != nil {
		return false
	}
	cover, err := s.db.CoverStateForUser(ctx, operation.TargetOwnerUserID, operation.BookID)
	if err != nil {
		return false
	}
	sourcePaths.CoverSelected, err = remapRelativePath(s.cfg.DataDir, destinationDir, sourceDir, cover.SelectedRelPath)
	if err != nil {
		return false
	}
	sourcePaths.CoverCandidate, err = remapRelativePath(s.cfg.DataDir, destinationDir, sourceDir, cover.CandidateRelPath)
	if err != nil {
		return false
	}
	sourcePaths.CoverAICandidate, err = remapRelativePath(s.cfg.DataDir, destinationDir, sourceDir, cover.AICandidateRelPath)
	if err != nil || validateStagedArtifacts(s.cfg.DataDir, sourceDir, sourceDir, sourcePaths) != nil {
		return false
	}
	if err := s.db.RollbackAdminBookTransfer(ctx, operation, sourcePaths); err != nil {
		return false
	}
	if hasOperationMarker(destinationDir, operation.ID) {
		_ = os.RemoveAll(destinationDir)
	}
	return true
}

func (s *Service) fail(ctx context.Context, operationID, message string) {
	_ = s.db.FailAdminBookOperation(ctx, operationID, message)
}

type manifestFile struct {
	relative string
	size     int64
}

func fileManifest(dataRoot, bookRoot string, required []string) ([]manifestFile, int64, error) {
	root, err := filepath.Abs(bookRoot)
	if err != nil {
		return nil, 0, err
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, 0, fmt.Errorf("source directory is unavailable")
	}
	var files []manifestFile
	seen := make(map[string]bool)
	var total int64
	addFile := func(path string, info os.FileInfo) error {
		if err := validateNoSymlinkPath(root, path); err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unexpected non-file in source book")
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("source file path is invalid")
		}
		if seen[rel] {
			return nil
		}
		if len(files) >= maxBookCopyFiles || info.Size() < 0 || info.Size() > maxBookCopyBytes ||
			total > maxBookCopyBytes-info.Size() {
			return fmt.Errorf("source book exceeds copy limits")
		}
		seen[rel] = true
		total += info.Size()
		files = append(files, manifestFile{relative: rel, size: info.Size()})
		return nil
	}
	dataAbs, err := filepath.Abs(dataRoot)
	if err != nil {
		return nil, 0, err
	}
	for _, relative := range required {
		if relative == "" {
			continue
		}
		if filepath.IsAbs(relative) {
			return nil, 0, fmt.Errorf("absolute artifact path")
		}
		path := filepath.Join(dataAbs, filepath.Clean(relative))
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, 0, fmt.Errorf("required artifact is unavailable")
		}
		if err := addFile(path, info); err != nil {
			return nil, 0, err
		}
	}
	sourceDir := filepath.Join(root, "source")
	if _, err := os.Lstat(sourceDir); err == nil {
		err = filepath.WalkDir(sourceDir, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("symbolic link in source book")
			}
			if entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("unexpected non-file in source book")
			}
			name := entry.Name()
			if name == operationMarker || strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".tmp") ||
				strings.HasSuffix(name, ".info.json") {
				return nil
			}
			return addFile(path, info)
		})
		if err != nil {
			return nil, 0, err
		}
	} else if !os.IsNotExist(err) {
		return nil, 0, err
	}
	if len(files) == 0 {
		return nil, 0, fmt.Errorf("source files are unavailable")
	}
	return files, total, nil
}

func validateNoSymlinkPath(root, filename string) error {
	relative, err := filepath.Rel(root, filename)
	if err != nil || relative == ".." || filepath.IsAbs(relative) ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("file path is outside the book directory")
	}
	current := root
	parts := strings.Split(relative, string(filepath.Separator))
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symbolic link or missing path in book directory")
		}
		if index < len(parts)-1 && !info.IsDir() {
			return fmt.Errorf("non-directory in book path")
		}
	}
	return nil
}

func hasOperationMarker(destinationDir, operationID string) bool {
	marker, err := os.ReadFile(filepath.Join(destinationDir, operationMarker))
	return err == nil && strings.TrimSpace(string(marker)) == operationID
}

func copyBookFile(ctx context.Context, sourceRoot, targetRoot string, file manifestFile) error {
	source := filepath.Join(sourceRoot, file.relative)
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("source is not a regular file")
	}
	target := filepath.Join(targetRoot, file.relative)
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	buffer := make([]byte, 256<<10)
	var copied int64
	for {
		if err := ctx.Err(); err != nil {
			_ = output.Close()
			_ = os.Remove(target)
			return err
		}
		n, readErr := input.Read(buffer)
		if n > 0 {
			copied += int64(n)
			if copied > file.size || copied > maxBookCopyBytes {
				_ = output.Close()
				_ = os.Remove(target)
				return fmt.Errorf("source file changed while copying")
			}
			if _, err := output.Write(buffer[:n]); err != nil {
				_ = output.Close()
				_ = os.Remove(target)
				return err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			_ = output.Close()
			_ = os.Remove(target)
			return readErr
		}
	}
	if copied != file.size {
		_ = output.Close()
		_ = os.Remove(target)
		return fmt.Errorf("source file size changed while copying")
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		_ = os.Remove(target)
		return err
	}
	return output.Close()
}

func remapBookPaths(dataDir, sourceDir, destinationDir string, book db.Book) (db.BookArtifactPaths, error) {
	var paths db.BookArtifactPaths
	var err error
	if paths.Audio, err = remapRelativePath(dataDir, sourceDir, destinationDir, book.AudioRelPath); err != nil {
		return paths, err
	}
	if paths.EPUB, err = remapRelativePath(dataDir, sourceDir, destinationDir, book.EpubRelPath); err != nil {
		return paths, err
	}
	if paths.EbookJSON, err = remapRelativePath(dataDir, sourceDir, destinationDir, book.EbookJSONRelPath); err != nil {
		return paths, err
	}
	if paths.Transcript, err = remapRelativePath(dataDir, sourceDir, destinationDir, book.TranscriptRelPath); err != nil {
		return paths, err
	}
	if paths.Alignment, err = remapRelativePath(dataDir, sourceDir, destinationDir, book.AlignmentRelPath); err != nil {
		return paths, err
	}
	return paths, nil
}

func remapRelativePath(dataDir, sourceDir, destinationDir, relative string) (string, error) {
	if relative == "" {
		return "", nil
	}
	var mapped string
	sourceAbs, err := filepath.Abs(sourceDir)
	if err != nil {
		return "", err
	}
	dataAbs, err := filepath.Abs(dataDir)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(relative) {
		return "", fmt.Errorf("absolute artifact path")
	}
	sourcePath := filepath.Join(dataAbs, filepath.Clean(relative))
	fromSource, err := filepath.Rel(sourceAbs, sourcePath)
	if err != nil || fromSource == ".." || strings.HasPrefix(fromSource, ".."+string(filepath.Separator)) ||
		filepath.IsAbs(fromSource) {
		return "", fmt.Errorf("artifact is outside the book directory")
	}
	newPath := filepath.Join(destinationDir, fromSource)
	mapped, err = filepath.Rel(dataAbs, newPath)
	if err != nil {
		return "", err
	}
	return mapped, nil
}

func validateStagedArtifacts(dataDir, destinationDir, stageDir string, paths db.BookArtifactPaths) error {
	check := func(rel string, required bool) error {
		if rel == "" {
			if required {
				return fmt.Errorf("required book file is missing")
			}
			return nil
		}
		if filepath.IsAbs(rel) {
			return fmt.Errorf("book path is outside its owner directory")
		}
		dataAbs, err := filepath.Abs(dataDir)
		if err != nil {
			return err
		}
		finalPath := filepath.Join(dataAbs, filepath.Clean(rel))
		fromBook, err := filepath.Rel(destinationDir, finalPath)
		if err != nil || fromBook == ".." || strings.HasPrefix(fromBook, ".."+string(filepath.Separator)) {
			return fmt.Errorf("book path is outside its owner directory")
		}
		stagedPath := filepath.Join(stageDir, fromBook)
		info, err := os.Lstat(stagedPath)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 {
			return fmt.Errorf("required artifact did not copy")
		}
		return nil
	}
	if err := check(paths.Audio, true); err != nil {
		return err
	}
	if err := check(paths.Transcript, true); err != nil {
		return err
	}
	for _, optional := range []string{paths.EPUB, paths.EbookJSON, paths.Alignment, paths.CoverSelected,
		paths.CoverCandidate, paths.CoverAICandidate} {
		if err := check(optional, false); err != nil {
			return err
		}
	}
	return nil
}

func writeSyncedFile(path string, contents []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func syncTreeDirectories(root string) error {
	var directories []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symbolic link in copied book")
		}
		if entry.IsDir() {
			directories = append(directories, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	depth := func(path string) int {
		return strings.Count(filepath.Clean(path), string(filepath.Separator))
	}
	sort.Slice(directories, func(i, j int) bool {
		left, right := depth(directories[i]), depth(directories[j])
		if left != right {
			return left > right
		}
		return len(directories[i]) > len(directories[j])
	})
	for _, directory := range directories {
		if err := syncDirectory(directory); err != nil {
			return err
		}
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

func syncDirectoryAncestors(path, dataRoot string) error {
	root, err := filepath.Abs(dataRoot)
	if err != nil {
		return err
	}
	current, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(root, current)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("directory is outside the data root")
	}
	for {
		if err := syncDirectory(current); err != nil {
			return err
		}
		if current == root {
			return nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return fmt.Errorf("data root was not reached")
		}
		current = parent
	}
}

func hasCopyDiskSpace(path string, bytesNeeded int64) bool {
	if bytesNeeded < 0 {
		return false
	}
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil || stats.Bsize <= 0 {
		return false
	}
	free := uint64(stats.Bavail) * uint64(stats.Bsize)
	reserve := uint64(256 << 20)
	if uint64(bytesNeeded)/20 > reserve {
		reserve = uint64(bytesNeeded) / 20
	}
	return free >= uint64(bytesNeeded) && free-uint64(bytesNeeded) >= reserve
}
