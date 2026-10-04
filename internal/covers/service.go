package covers

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jgbrwn/readalong/internal/config"
	"github.com/jgbrwn/readalong/internal/coverai"
	"github.com/jgbrwn/readalong/internal/db"
	"github.com/jgbrwn/readalong/internal/epub"
	"github.com/jgbrwn/readalong/internal/pipeline"
)

const (
	coverAISettingsKey           = "cover_ai_settings"
	coverLeaseDuration           = 5 * time.Minute
	maxOpenLibrarySearchesPerDay = 100
)

var errOpenLibraryDailyLimit = errors.New("Open Library daily search limit reached")

type Service struct {
	cfg     config.Config
	db      *db.DB
	models  coverai.ImageGenerator
	catalog *OpenLibrary
}

func New(cfg config.Config, database *db.DB, models coverai.ImageGenerator) *Service {
	cfg = cfg.Normalize()
	if models == nil {
		models = coverai.NewRegistry(cfg.OpenRouterAPIKey)
	}
	return &Service{
		cfg: cfg, db: database, models: models,
		catalog: &OpenLibrary{Contact: cfg.OpenLibraryContact},
	}
}

func (s *Service) Run(ctx context.Context) {
	_ = s.db.RequeueUnscheduledCoverLookups(ctx)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		now := time.Now().UTC()
		task, found, err := s.db.ClaimNextCoverTask(ctx,
			now.Format(time.RFC3339), now.Add(coverLeaseDuration).Format(time.RFC3339))
		if err != nil {
			log.Printf("readalong: cover reconciliation queue unavailable")
		} else if found {
			taskContext, cancel := context.WithTimeout(ctx, 4*time.Minute)
			s.process(taskContext, task)
			cancel()
			continue
		}
		if !waitCover(ctx, ticker.C) {
			return
		}
	}
}

func waitCover(ctx context.Context, ticker <-chan time.Time) bool {
	select {
	case <-ctx.Done():
		return false
	case <-ticker:
		return true
	}
}

func (s *Service) lookupOpenLibrary(ctx context.Context, task db.CoverTask, now time.Time,
	forceRefresh bool,
) (Candidate, bool, time.Time, error) {
	author := trustedCoverAuthor(task)
	key := coverCacheKey(task.Title, author)
	nowText := now.UTC().Format(time.RFC3339)
	cached, exists, fresh, err := s.db.CoverLookupCache(ctx, key, nowText)
	if err != nil {
		return Candidate{}, false, time.Time{}, err
	}
	cacheNext, _ := time.Parse(time.RFC3339, cached.NextCheckAt)
	if fresh && !forceRefresh {
		candidate, found, err := cachedCoverCandidate(cached)
		return candidate, found, cacheNext, err
	}
	allowed, err := s.db.TakeCoverLookupPermit(ctx, "openlibrary", now.UTC().Format("2006-01-02"),
		maxOpenLibrarySearchesPerDay)
	if err != nil {
		return Candidate{}, false, time.Time{}, err
	}
	if !allowed {
		next := now.UTC().Truncate(24 * time.Hour).Add(24*time.Hour + 5*time.Minute)
		return Candidate{}, false, next, errOpenLibraryDailyLimit
	}
	candidate, found, err := s.catalog.SearchCover(ctx, task.Title, author)
	if err != nil {
		if exists && cached.Found {
			candidate, found, cacheErr := cachedCoverCandidate(cached)
			return candidate, found, cacheNext, cacheErr
		}
		return Candidate{}, false, time.Time{}, err
	}
	next := now.Add(30 * 24 * time.Hour)
	noMatches := 0
	payload := ""
	if found || candidate.Year > 0 {
		encoded, err := json.Marshal(candidate)
		if err != nil {
			return Candidate{}, false, time.Time{}, err
		}
		payload = string(encoded)
	}
	if found {
		noMatches = 0
	} else {
		noMatches = cached.NoMatchCount + 1
		next = nextNegativeCheck(cached.NoMatchCount, now)
	}
	next = next.UTC()
	if err := s.db.SetCoverLookupCache(ctx, key, payload, found, noMatches,
		nowText, next.Format(time.RFC3339)); err != nil {
		return Candidate{}, false, time.Time{}, err
	}
	return candidate, found, next, nil
}

func cachedCoverCandidate(cache db.CoverLookupCache) (Candidate, bool, error) {
	if cache.ResultJSON == "" {
		return Candidate{}, cache.Found, nil
	}
	var candidate Candidate
	if err := json.Unmarshal([]byte(cache.ResultJSON), &candidate); err != nil {
		return Candidate{}, false, fmt.Errorf("cached cover candidate is invalid")
	}
	return candidate, cache.Found, nil
}

func coverCacheKey(title, author string) string {
	canonical := strings.Join(normalizedTokens(title), " ") + "\x00" +
		strings.Join(normalizedTokens(author), " ")
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

func trustedCoverAuthor(task db.CoverTask) string {
	if task.EPUBAuthor != "" {
		return task.EPUBAuthor
	}
	if task.Author == "" {
		return ""
	}
	if task.SourceKind == "librivox" {
		return task.Author
	}
	return ""
}

func (s *Service) process(ctx context.Context, task db.CoverTask) {
	if task.LocalScanNeeded {
		if task.EpubRelPath != "" && task.SelectedKind == "" && s.extractEPUBCover(ctx, task) {
			return
		}
		if task.AudioRelPath != "" && task.SelectedKind == "" && s.extractAudioCover(ctx, task) {
			return
		}
		if err := s.db.SetLocalCoverScanComplete(ctx, task); err != nil {
			return
		}
	}
	task.EPUBAuthor, task.BookDescription = readBookMetadata(s.cfg.DataDir, task)
	settings, err := s.coverAISettings(ctx)
	if err != nil {
		s.setFailure(ctx, task)
		return
	}
	if task.RegenerationRequested {
		s.processRegeneration(ctx, task, settings)
		return
	}
	if !settings.CatalogLookupEnabled {
		if task.Title != "" && task.Title != "Untitled" && task.SelectedKind == "" &&
			settings.Enabled && settings.ModelID != "" && !task.AIGenerationBlocked {
			s.generateFallback(ctx, task, settings, 0, "")
			return
		}
		_ = s.db.PauseCoverTask(ctx, task)
		return
	}
	if task.Title == "" || task.Title == "Untitled" {
		s.setFailure(ctx, task)
		return
	}
	now := time.Now().UTC()
	candidate, found, cacheNext, err := s.lookupOpenLibrary(ctx, task, now, false)
	if err != nil {
		if errors.Is(err, errOpenLibraryDailyLimit) {
			next := now.UTC().Truncate(24 * time.Hour).Add(24*time.Hour + 5*time.Minute)
			_ = s.db.DeferCoverTask(ctx, task, next.Format(time.RFC3339))
			return
		}
		s.setFailure(ctx, task)
		return
	}
	if found {
		reliableAuthor := trustedCoverAuthor(task) != ""
		if task.SelectedKind == "ai_svg" || task.SelectedKind == "ai_image" || !candidate.TitleExact ||
			(reliableAuthor && candidate.AuthorOverlap < 0.75) || !reliableAuthor {
			_ = s.db.SetCoverCandidate(ctx, task, candidate.ImageURL, candidate.SourceURL, candidate.Year)
			return
		}
		_ = s.db.SetCatalogCover(ctx, task, candidate.ImageURL, candidate.SourceURL, candidate.Year)
		return
	}

	nextCheck := nextNegativeCheck(task.NoMatchCount, now)
	if cacheNext.After(nextCheck) {
		nextCheck = cacheNext
	}
	if err := s.db.SetCoverNoMatch(ctx, task, now.Format(time.RFC3339), nextCheck.Format(time.RFC3339), candidate.Year); err != nil {
		return
	}
	if task.SelectedKind != "" {
		_ = s.db.ReleaseCoverTask(ctx, task)
		return
	}
	if !settings.Enabled || settings.ModelID == "" || task.AIGenerationBlocked {
		_ = s.db.ReleaseCoverTask(ctx, task)
		return
	}
	s.generateFallback(ctx, task, settings, candidate.Year, nextCheck.Format(time.RFC3339))
}

func (s *Service) processRegeneration(ctx context.Context, task db.CoverTask, settings coverai.Settings) {
	now := time.Now().UTC()
	var suggestion *db.CoverSuggestion
	aiYear := 0
	nextCheckAt := ""
	lookupResult := ""
	if settings.CatalogLookupEnabled && task.Title != "" && task.Title != "Untitled" {
		candidate, found, cacheNext, err := s.lookupOpenLibrary(ctx, task, now, true)
		switch {
		case errors.Is(err, errOpenLibraryDailyLimit):
			lookupResult = "rate_limited"
			nextCheckAt = now.Truncate(24 * time.Hour).Add(24*time.Hour + 5*time.Minute).Format(time.RFC3339)
		case err != nil:
			lookupResult = "failed"
			nextCheckAt = nextProviderRetry(task.FailureCount, now).Format(time.RFC3339)
		case found:
			lookupResult = "found"
			suggestion = &db.CoverSuggestion{URL: candidate.ImageURL, SourceURL: candidate.SourceURL, Year: candidate.Year}
			if candidate.Year > 0 {
				aiYear = candidate.Year
			}
		default:
			lookupResult = "missing"
			nextCheck := nextNegativeCheck(task.NoMatchCount, now)
			if cacheNext.After(nextCheck) {
				nextCheck = cacheNext
			}
			nextCheckAt = nextCheck.Format(time.RFC3339)
			if candidate.Year > 0 {
				aiYear = candidate.Year
			}
		}
	}

	aiPath := ""
	aiAttempted := false
	aiFailed := false
	if settings.Enabled && settings.ModelID != "" && task.Title != "" && task.Title != "Untitled" {
		description := ""
		if settings.UseBookDescription {
			description = task.BookDescription
		}
		if err := s.db.ReserveAIGenerationAttempt(ctx, task); err != nil {
			log.Printf("readalong: regenerated image attempt could not be reserved")
			_ = s.db.ReleaseCoverTask(ctx, task)
			return
		}
		aiAttempted = true
		image, err := s.models.GenerateCoverImage(ctx, settings.ModelID, coverai.CoverDetails{
			Title: task.Title, Author: coverPromptAuthor(task), Description: description, Year: aiYear,
		})
		if err == nil && (image.Extension != "jpg" || len(image.Bytes) == 0) {
			err = fmt.Errorf("generated image had an unsupported format")
		}
		if err == nil {
			filename := filepath.Join(pipeline.BookDirectory(s.cfg.DataDir, task.OwnerUserID, task.BookID),
				"cover", fmt.Sprintf("generated-regenerated-%d.jpg", time.Now().UTC().UnixNano()))
			if writeErr := writeAtomic(filename, image.Bytes); writeErr != nil {
				err = fmt.Errorf("generated image could not be saved")
			} else if relative, relErr := filepath.Rel(s.cfg.DataDir, filename); relErr == nil {
				aiPath = relative
			} else {
				_ = os.Remove(filename)
				err = fmt.Errorf("generated image path could not be prepared")
			}
		}
		if err != nil {
			aiFailed = true
			log.Printf("readalong: regenerated image cover failed")
		}
	}

	if err := s.db.SetCoverRegenerationResults(ctx, task, suggestion, aiPath, aiYear,
		now.Format(time.RFC3339), nextCheckAt, lookupResult, aiAttempted, aiFailed); err != nil {
		if aiPath != "" {
			_ = os.Remove(filepath.Join(s.cfg.DataDir, aiPath))
		}
		s.setFailure(ctx, task)
	}
}

func (s *Service) generateFallback(ctx context.Context, task db.CoverTask, settings coverai.Settings,
	year int, nextCheckAt string,
) {
	if err := s.db.ReserveAIGenerationAttempt(ctx, task); err != nil {
		log.Printf("readalong: generated image attempt could not be reserved")
		_ = s.db.ReleaseCoverTask(ctx, task)
		return
	}
	description := ""
	if settings.UseBookDescription {
		description = task.BookDescription
	}
	cover, err := s.models.GenerateCoverImage(ctx, settings.ModelID, coverai.CoverDetails{
		Title: task.Title, Author: coverPromptAuthor(task), Description: description, Year: year,
	})
	if err != nil {
		log.Printf("readalong: generated cover design failed")
		_ = s.db.SetAIGenerationFailure(ctx, task, nextCheckAt)
		return
	}
	bookDir := pipeline.BookDirectory(s.cfg.DataDir, task.OwnerUserID, task.BookID)
	coverDir := filepath.Join(bookDir, "cover")
	if cover.Extension != "jpg" || len(cover.Bytes) == 0 {
		log.Printf("readalong: generated image cover had an unsupported format")
		_ = s.db.SetAIGenerationFailure(ctx, task, nextCheckAt)
		return
	}
	filename := filepath.Join(coverDir, "generated.jpg")
	if err := writeAtomic(filename, cover.Bytes); err != nil {
		log.Printf("readalong: generated cover could not be saved")
		_ = s.db.SetAIGenerationFailure(ctx, task, nextCheckAt)
		return
	}
	relative, err := filepath.Rel(s.cfg.DataDir, filename)
	if err != nil {
		_ = os.Remove(filename)
		_ = s.db.SetAIGenerationFailure(ctx, task, nextCheckAt)
		return
	}
	if err := s.db.SetGeneratedCover(ctx, task, relative, nextCheckAt); err != nil {
		_ = os.Remove(filename)
		_ = s.db.ReleaseCoverTask(ctx, task)
	}
}

func readBookMetadata(dataDir string, task db.CoverTask) (string, string) {
	if task.EbookJSONRelPath == "" {
		return "", ""
	}
	path, err := safeBookCoverPath(dataDir, task, task.EbookJSONRelPath)
	if err != nil {
		return "", ""
	}
	file, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		return "", ""
	}
	defer reader.Close()
	var metadata struct {
		Author      string `json:"author"`
		Description string `json:"description"`
	}
	decoder := json.NewDecoder(io.LimitReader(reader, 64<<20))
	if decoder.Decode(&metadata) != nil {
		return "", ""
	}
	if len(metadata.Author) > 255 {
		metadata.Author = metadata.Author[:255]
	}
	if len(metadata.Description) > 2400 {
		metadata.Description = metadata.Description[:2400]
	}
	return metadata.Author, metadata.Description
}

func (s *Service) extractEPUBCover(ctx context.Context, task db.CoverTask) bool {
	path, err := safeBookCoverPath(s.cfg.DataDir, task, task.EpubRelPath)
	if err != nil {
		return false
	}
	imageBytes, year, err := epub.ExtractCoverFileWithYear(path)
	if err != nil {
		return false
	}
	bookDir := pipeline.BookDirectory(s.cfg.DataDir, task.OwnerUserID, task.BookID)
	filename := filepath.Join(bookDir, "cover", "epub.jpg")
	if err := writeAtomic(filename, imageBytes); err != nil {
		return false
	}
	relative, err := filepath.Rel(s.cfg.DataDir, filename)
	if err != nil {
		_ = os.Remove(filename)
		return false
	}
	if err := s.db.SetLocalCover(ctx, task, "epub", relative, year, false, ""); err != nil {
		_ = os.Remove(filename)
		return false
	}
	return true
}

func (s *Service) extractAudioCover(ctx context.Context, task db.CoverTask) bool {
	paths := []string{task.AudioRelPath}
	sourceDir := filepath.Join(pipeline.BookDirectory(s.cfg.DataDir, task.OwnerUserID, task.BookID), "source")
	entries, _ := os.ReadDir(sourceDir)
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		switch strings.ToLower(filepath.Ext(entry.Name())) {
		case ".mp3", ".m4a", ".m4b", ".flac", ".ogg", ".opus", ".wav", ".mp4":
			sourcePath := filepath.Join(sourceDir, entry.Name())
			if relative, err := filepath.Rel(s.cfg.DataDir, sourcePath); err == nil {
				paths = append(paths, relative)
			}
		}
	}
	var imageBytes []byte
	found := false
	for _, relative := range paths {
		path, err := safeBookCoverPath(s.cfg.DataDir, task, relative)
		if err != nil {
			continue
		}
		imageBytes, found = extractEmbeddedArtwork(ctx, s.cfg.FFprobeBin, s.cfg.FFmpegBin, path)
		if found {
			break
		}
	}
	if !found {
		return false
	}
	bookDir := pipeline.BookDirectory(s.cfg.DataDir, task.OwnerUserID, task.BookID)
	filename := filepath.Join(bookDir, "cover", "audio.jpg")
	if err := writeAtomic(filename, imageBytes); err != nil {
		return false
	}
	relative, err := filepath.Rel(s.cfg.DataDir, filename)
	if err != nil {
		_ = os.Remove(filename)
		return false
	}
	if err := s.db.SetLocalCover(ctx, task, "audio", relative, 0, false, ""); err != nil {
		_ = os.Remove(filename)
		return false
	}
	return true
}

func (s *Service) setFailure(ctx context.Context, task db.CoverTask) {
	now := time.Now().UTC()
	next := nextProviderRetry(task.FailureCount, now)
	_ = s.db.SetCoverLookupFailure(ctx, task, now.Format(time.RFC3339), next.Format(time.RFC3339))
}

func (s *Service) coverAISettings(ctx context.Context) (coverai.Settings, error) {
	setting, found, err := s.db.AppSetting(ctx, coverAISettingsKey)
	if err != nil {
		return coverai.Settings{}, err
	}
	if !found {
		return coverai.DefaultSettings(s.models.Configured()), nil
	}
	var settings coverai.Settings
	if err := json.Unmarshal([]byte(setting.Value), &settings); err != nil {
		return coverai.Settings{}, fmt.Errorf("cover settings are invalid")
	}
	return coverai.NormalizeSettings(settings, s.models.Configured()), nil
}

func coverPromptAuthor(task db.CoverTask) string {
	if author := strings.TrimSpace(task.EPUBAuthor); author != "" {
		return author
	}
	return strings.TrimSpace(task.Author)
}

func nextNegativeCheck(previous int, now time.Time) time.Time {
	switch previous + 1 {
	case 1:
		return now.Add(24 * time.Hour)
	case 2:
		return now.Add(7 * 24 * time.Hour)
	default:
		return now.Add(30 * 24 * time.Hour)
	}
}

func nextProviderRetry(previous int, now time.Time) time.Time {
	switch previous + 1 {
	case 1:
		return now.Add(time.Hour)
	case 2:
		return now.Add(6 * time.Hour)
	case 3:
		return now.Add(24 * time.Hour)
	default:
		return now.Add(72 * time.Hour)
	}
}

func safeCoverPath(root, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) {
		return "", fmt.Errorf("invalid cover source path")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	full := filepath.Join(rootAbs, filepath.Clean(relative))
	rel, err := filepath.Rel(rootAbs, full)
	if err != nil || rel == ".." || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
		return "", fmt.Errorf("invalid cover source path")
	}
	info, err := os.Lstat(full)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("cover source is not a regular file")
	}
	return full, nil
}

func safeBookCoverPath(root string, task db.CoverTask, relative string) (string, error) {
	full, err := safeCoverPath(root, relative)
	if err != nil {
		return "", err
	}
	bookDir, err := filepath.Abs(pipeline.BookDirectory(root, task.OwnerUserID, task.BookID))
	if err != nil {
		return "", err
	}
	within, err := filepath.Rel(bookDir, full)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("cover input is outside the book directory")
	}
	return full, nil
}

func writeAtomic(filename string, data []byte) error {
	if len(data) == 0 || len(data) > 20<<20 {
		return fmt.Errorf("cover image size is invalid")
	}
	dir := filepath.Dir(filename)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".cover-*.tmp")
	if err != nil {
		return err
	}
	tmp := file.Name()
	published := false
	defer func() {
		_ = file.Close()
		if !published {
			_ = os.Remove(tmp)
		}
	}()
	if err := file.Chmod(0600); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filename); err != nil {
		return err
	}
	if directory, err := os.Open(dir); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	published = true
	return nil
}
