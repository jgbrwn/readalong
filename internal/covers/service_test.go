package covers

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jgbrwn/readalong/internal/config"
	"github.com/jgbrwn/readalong/internal/coverai"
	"github.com/jgbrwn/readalong/internal/db"
	"github.com/jgbrwn/readalong/internal/pipeline"
)

type fakeCoverImageGenerator struct {
	configured bool
	image      coverai.GeneratedImage
	err        error
	calls      int
	modelID    string
	details    coverai.CoverDetails
}

func (f *fakeCoverImageGenerator) Configured() bool { return f.configured }

func (f *fakeCoverImageGenerator) GenerateCoverImage(_ context.Context, modelID string,
	details coverai.CoverDetails,
) (coverai.GeneratedImage, error) {
	f.calls++
	f.modelID = modelID
	f.details = details
	return f.image, f.err
}

func TestLookupCacheDeduplicatesAcrossEquivalentBookMetadata(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]any{"docs": []any{}})
	}))
	defer server.Close()
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	service := New(config.Config{DataDir: t.TempDir()}, database, nil)
	service.catalog = &OpenLibrary{BaseURL: server.URL, HTTP: server.Client()}
	now := time.Now().UTC()
	first := db.CoverTask{Title: "No Cover!", Author: "A. Writer"}
	_, found, next, err := service.lookupOpenLibrary(context.Background(), first, now, false)
	if err != nil || found {
		t.Fatalf("first lookup found=%v err=%v", found, err)
	}
	if want := now.Add(24 * time.Hour); next.Sub(want) > time.Second || want.Sub(next) > time.Second {
		t.Fatalf("first negative retry = %s, want %s", next, want)
	}
	second := db.CoverTask{Title: "no cover", Author: "A Writer"}
	_, found, nextCached, err := service.lookupOpenLibrary(context.Background(), second, now.Add(time.Hour), false)
	if err != nil || found || calls != 1 {
		t.Fatalf("equivalent lookup found=%v calls=%d err=%v", found, calls, err)
	}
	if nextCached.Sub(next) > time.Second || next.Sub(nextCached) > time.Second {
		t.Fatalf("cached retry time = %s, want %s", nextCached, next)
	}
}

func TestManualRegenerationBypassesFreshNegativeCacheOnce(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]any{"docs": []any{}})
	}))
	defer server.Close()
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	service := New(config.Config{DataDir: t.TempDir()}, database, nil)
	service.catalog = &OpenLibrary{BaseURL: server.URL, HTTP: server.Client()}
	now := time.Now().UTC()
	task := db.CoverTask{Title: "Same Book", Author: "A Writer"}
	if _, _, _, err := service.lookupOpenLibrary(context.Background(), task, now, false); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := service.lookupOpenLibrary(context.Background(), task, now.Add(time.Hour), true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("manual regeneration reused the fresh negative cache: provider calls=%d", calls)
	}
}

func TestManualRegenerationCreatesCatalogAndFreshAICoverCandidates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dataDir := t.TempDir()
	database, err := db.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.UpsertUser(ctx, "cover-owner", "owner@example.org", false, false); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateBookAndJob(ctx, db.NewBook{
		ID: "manual-cover", JobID: "manual-cover-job", OwnerUserID: "cover-owner",
		Title: "Example Book", Author: "A. Writer", SourceKind: "librivox",
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteJob(ctx, "manual-cover-job"); err != nil {
		t.Fatal(err)
	}
	currentCover := filepath.Join(pipeline.BookDirectory(dataDir, "cover-owner", "manual-cover"),
		"cover", "current.svg")
	if err := writeAtomic(currentCover, []byte(`<svg xmlns="http://www.w3.org/2000/svg"><text>current</text></svg>`)); err != nil {
		t.Fatal(err)
	}
	currentCoverRel, err := filepath.Rel(dataDir, currentCover)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE book_cover_state SET status='selected',
		selected_kind='audio',selected_relpath=?,lookup_paused=1 WHERE book_id='manual-cover'`, currentCoverRel); err != nil {
		t.Fatal(err)
	}
	if err := database.QueueCoverRegeneration(ctx, "cover-owner", "manual-cover"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task, found, err := database.ClaimNextCoverTask(ctx, now.Format(time.RFC3339),
		now.Add(5*time.Minute).Format(time.RFC3339))
	if err != nil || !found || !task.RegenerationRequested {
		t.Fatalf("regeneration claim=%#v found=%v err=%v", task, found, err)
	}

	catalogServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"docs": []any{map[string]any{
			"key": "/works/OL123W", "title": "Example Book", "author_name": []string{"A. Writer"},
			"first_publish_year": 1930, "cover_i": 77,
		}}})
	}))
	defer catalogServer.Close()
	var imageData bytes.Buffer
	if err := jpeg.Encode(&imageData, image.NewRGBA(image.Rect(0, 0, 768, 1152)), &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	settings, err := json.Marshal(coverai.Settings{
		Enabled: true, CatalogLookupEnabled: true, ModelID: coverai.DefaultImageModelID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetAppSetting(ctx, coverAISettingsKey, string(settings)); err != nil {
		t.Fatal(err)
	}
	generator := &fakeCoverImageGenerator{
		configured: true,
		image:      coverai.GeneratedImage{Bytes: imageData.Bytes(), Extension: "jpg"},
	}
	service := New(config.Config{DataDir: dataDir}, database, generator)
	service.catalog = &OpenLibrary{BaseURL: catalogServer.URL + "/search.json", HTTP: catalogServer.Client()}
	service.process(ctx, task)

	state, err := database.CoverStateForUser(ctx, "cover-owner", "manual-cover")
	if err != nil || state.Status != "review" || state.RegenerationRequested ||
		state.SelectedKind != "audio" || state.SelectedRelPath != currentCoverRel ||
		state.CandidateURL != "https://covers.openlibrary.org/b/id/77-M.jpg?default=false" ||
		state.AICandidateRelPath == "" || state.AICandidateYear != 1930 {
		t.Fatalf("manual regeneration results=%#v err=%v", state, err)
	}
	aiPath := filepath.Join(dataDir, state.AICandidateRelPath)
	aiCover, err := os.ReadFile(aiPath)
	if err != nil || !bytes.HasPrefix(aiCover, []byte{0xff, 0xd8, 0xff}) ||
		!strings.HasSuffix(state.AICandidateRelPath, ".jpg") || generator.calls != 1 ||
		generator.modelID != coverai.DefaultImageModelID || generator.details.Author != "A. Writer" ||
		generator.details.Year != 1930 {
		t.Fatalf("fresh AI cover missing or invalid: bytes=%d err=%v", len(aiCover), err)
	}
}

func TestCoverRetryScheduleBacksOffNegativeAndProviderFailures(t *testing.T) {
	now := time.Now()
	if got := nextNegativeCheck(0, now).Sub(now); got != 24*time.Hour {
		t.Fatalf("first no-match delay = %s", got)
	}
	if got := nextNegativeCheck(1, now).Sub(now); got != 7*24*time.Hour {
		t.Fatalf("second no-match delay = %s", got)
	}
	if got := nextNegativeCheck(2, now).Sub(now); got != 30*24*time.Hour {
		t.Fatalf("later no-match delay = %s", got)
	}
	if got := nextProviderRetry(0, now).Sub(now); got != time.Hour {
		t.Fatalf("first provider retry delay = %s", got)
	}
	if got := nextProviderRetry(1, now).Sub(now); got != 6*time.Hour {
		t.Fatalf("second provider retry delay = %s", got)
	}
}

func TestReadBookMetadataUsesOnlyStoredEPUBDescriptionFields(t *testing.T) {
	dataDir := t.TempDir()
	bookDir := pipeline.BookDirectory(dataDir, "owner", "metadata-book")
	filename := filepath.Join(bookDir, "ebook.v1.json.gz")
	if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	compressed := gzip.NewWriter(file)
	if err := json.NewEncoder(compressed).Encode(map[string]string{
		"author": "Verified Writer", "description": "A short publisher description, not book chapters.",
	}); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(dataDir, filename)
	if err != nil {
		t.Fatal(err)
	}
	author, description := readBookMetadata(dataDir, db.CoverTask{
		BookID: "metadata-book", OwnerUserID: "owner", EbookJSONRelPath: relative,
	})
	if author != "Verified Writer" || description != "A short publisher description, not book chapters." {
		t.Fatalf("metadata author=%q description=%q", author, description)
	}
}

func TestLocalEPUBCoverStillExtractsWhenCatalogLookupIsDisabled(t *testing.T) {
	dataDir := t.TempDir()
	database, err := db.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	if _, err := database.UpsertUser(ctx, "cover-owner", "cover@example.org", false, false); err != nil {
		t.Fatal(err)
	}
	bookID := "epub-cover"
	bookDir := pipeline.BookDirectory(dataDir, "cover-owner", bookID)
	epubPath := filepath.Join(bookDir, "source", "book.epub")
	if err := os.MkdirAll(filepath.Dir(epubPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeEPUBWithCover(epubPath); err != nil {
		t.Fatal(err)
	}
	epubRel, err := filepath.Rel(dataDir, epubPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CreateBookAndJob(ctx, db.NewBook{
		ID: bookID, JobID: bookID + "-job", OwnerUserID: "cover-owner",
		Title: "A Cover Book", Author: "A Writer", SourceKind: "upload",
		EpubRelPath: epubRel,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE books SET status='ready',audio_relpath='books/cover-owner/epub-cover/playback.mp3'
		WHERE id=?`, bookID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE jobs SET status='completed',stage='ready',progress=1 WHERE book_id=?`, bookID); err != nil {
		t.Fatal(err)
	}
	settings, err := json.Marshal(coverai.Settings{CatalogLookupEnabled: false, UseBookDescription: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetAppSetting(ctx, coverAISettingsKey, string(settings)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task, found, err := database.ClaimNextCoverTask(ctx, now.Format(time.RFC3339), now.Add(5*time.Minute).Format(time.RFC3339))
	if err != nil || !found {
		t.Fatalf("cover task found=%v err=%v", found, err)
	}
	service := New(config.Config{DataDir: dataDir}, database, nil)
	service.process(ctx, task)
	state, err := database.CoverStateForUser(ctx, "cover-owner", bookID)
	if err != nil || state.SelectedKind != "epub" || !state.LookupPaused {
		t.Fatalf("local EPUB cover state=%#v err=%v", state, err)
	}
	if _, err := os.Stat(filepath.Join(bookDir, "cover", "epub.jpg")); err != nil {
		t.Fatalf("normalized EPUB cover was not saved: %v", err)
	}
}

func writeEPUBWithCover(filename string) error {
	var imageBytes bytes.Buffer
	imageData := image.NewRGBA(image.Rect(0, 0, 120, 180))
	for y := 0; y < 180; y++ {
		for x := 0; x < 120; x++ {
			imageData.Set(x, y, color.RGBA{R: 35, G: uint8(x), B: 90, A: 255})
		}
	}
	if err := png.Encode(&imageBytes, imageData); err != nil {
		return err
	}
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	archive := zip.NewWriter(file)
	entries := map[string][]byte{
		"META-INF/container.xml": []byte(`<container><rootfiles><rootfile full-path="OEBPS/package.opf"/></rootfiles></container>`),
		"OEBPS/package.opf":      []byte(`<package><metadata><title>A Cover Book</title><creator>A Writer</creator><description>A short story.</description><meta name="cover" content="cover"/></metadata><manifest><item id="cover" href="images/cover.png" media-type="image/png"/></manifest><spine><itemref idref="cover"/></spine></package>`),
		"OEBPS/images/cover.png": imageBytes.Bytes(),
	}
	for name, data := range entries {
		entry, err := archive.Create(name)
		if err != nil {
			archive.Close()
			file.Close()
			return err
		}
		if _, err := entry.Write(data); err != nil {
			archive.Close()
			file.Close()
			return err
		}
	}
	if err := archive.Close(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func TestCoverProviderDailyBudgetDefersWithoutSendingMoreSearches(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]any{"docs": []any{}})
	}))
	defer server.Close()
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	service := New(config.Config{DataDir: t.TempDir()}, database, nil)
	service.catalog = &OpenLibrary{BaseURL: server.URL, HTTP: server.Client()}
	now := time.Now().UTC()
	day := now.Format("2006-01-02")
	for range maxOpenLibrarySearchesPerDay {
		if allowed, err := database.TakeCoverLookupPermit(context.Background(), "openlibrary", day,
			maxOpenLibrarySearchesPerDay); err != nil || !allowed {
			t.Fatalf("daily permit allowed=%v err=%v", allowed, err)
		}
	}
	_, _, next, err := service.lookupOpenLibrary(context.Background(),
		db.CoverTask{Title: "A title", Author: "A writer"}, now, false)
	if err != errOpenLibraryDailyLimit || calls != 0 {
		t.Fatalf("daily limit err=%v calls=%d", err, calls)
	}
	want := now.Truncate(24 * time.Hour).Add(24*time.Hour + 5*time.Minute)
	if next.Sub(want) > time.Second || want.Sub(next) > time.Second {
		t.Fatalf("daily limit retry=%s, want=%s", next, want)
	}
}
