package httpapp

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jgbrwn/readalong/internal/align"
	"github.com/jgbrwn/readalong/internal/catalog"
	"github.com/jgbrwn/readalong/internal/config"
	"github.com/jgbrwn/readalong/internal/coverai"
	"github.com/jgbrwn/readalong/internal/db"
	"github.com/jgbrwn/readalong/internal/pipeline"
	"github.com/jgbrwn/readalong/internal/transcript"
)

func testServer(t *testing.T, cfg config.Config) (*db.DB, http.Handler) {
	t.Helper()
	d, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d, New(cfg, d)
}

func TestPairedCatalogSearchAndOwnerScopedImport(t *testing.T) {
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(catalog.Response{Books: []catalog.Record{{
			ID: "146", Title: "Anne of Green Gables", Language: "English",
			URLTextSource: "https://www.gutenberg.org/etext/45",
			URLZipFile:    "https://archive.org/compress/anne/formats=64KBPS%20MP3",
			URLLibriVox:   "https://librivox.org/anne/",
			TotalTimeSecs: 37811,
			Authors:       []catalog.Author{{FirstName: "Lucy Maud", LastName: "Montgomery"}},
		}}})
	}))
	defer apiServer.Close()
	client := catalog.NewClient()
	client.BaseURL, client.HTTP = apiServer.URL, apiServer.Client()

	cfg := config.Config{Env: "production", RequireExe: true, DenyStatus: http.StatusNotFound}
	d, _ := testServer(t, cfg)
	handler := NewWithCatalog(cfg, d, client)

	search := request(handler, http.MethodGet, "/api/discovery/pairs?q=Anne", "catalog-user", "reader@example.org", "")
	if search.Code != http.StatusOK || !strings.Contains(search.Body.String(), `"gutenberg_id":"45"`) {
		t.Fatalf("catalog search = %d: %s", search.Code, search.Body)
	}
	if strings.Contains(search.Body.String(), `"url_zip_file"`) {
		t.Fatalf("catalog exposed the archive URL: %s", search.Body)
	}
	imported := request(handler, http.MethodPost, "/api/discovery/pairs/146/import",
		"catalog-user", "reader@example.org", `{"rights_confirmed":true}`)
	if imported.Code != http.StatusAccepted {
		t.Fatalf("catalog import = %d: %s", imported.Code, imported.Body)
	}
	var book db.Book
	if err := json.Unmarshal(imported.Body.Bytes(), &book); err != nil {
		t.Fatal(err)
	}
	if book.Mode != "aligned" || book.SourceKind != "librivox" || book.GutenbergID != "45" ||
		book.Title != "Anne of Green Gables" || book.Author != "Lucy Maud Montgomery" {
		t.Fatalf("unexpected imported pair: %#v", book)
	}
	if book.EbookSourceURL != "https://www.gutenberg.org/ebooks/45" {
		t.Fatalf("canonical ebook source = %q", book.EbookSourceURL)
	}
	outsider := request(handler, http.MethodGet, "/api/books", "other-user", "other@example.org", "")
	if outsider.Code != http.StatusOK || strings.Contains(outsider.Body.String(), book.ID) {
		t.Fatalf("catalog book crossed owner boundary: %d %s", outsider.Code, outsider.Body)
	}
}

func TestAdminCoverImageModelSettingsAreFixedAndDefaultToGPTImage2(t *testing.T) {
	cfg := config.Config{
		Env: "production", RequireExe: true, DenyStatus: http.StatusNotFound,
		AdminUserIDs: map[string]bool{"admin-id": true},
	}
	d, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.SetAppSetting(context.Background(), coverAISettingsKey,
		`{"enabled":true,"model_id":"neuralwatt/qwen-vision","api_style":"responses","catalog_lookup_enabled":true}`); err != nil {
		t.Fatal(err)
	}
	handler := newWithCatalogAndCoverAI(cfg, d, nil, coverai.NewRegistry("test-key"))

	denied := request(handler, http.MethodGet, "/api/admin/cover-ai", "reader-id", "reader@example.org", "")
	if denied.Code != http.StatusNotFound {
		t.Fatalf("non-admin cover settings status = %d", denied.Code)
	}
	admin := request(handler, http.MethodGet, "/api/admin/cover-ai", "admin-id", "admin@example.org", "")
	if admin.Code != http.StatusOK || !strings.Contains(admin.Body.String(), `"id":"openai/gpt-image-2"`) ||
		!strings.Contains(admin.Body.String(), `"bytedance-seed/seedream-4.5"`) ||
		!strings.Contains(admin.Body.String(), `"black-forest-labs/flux.2-pro"`) ||
		!strings.Contains(admin.Body.String(), `"model_id":"openai/gpt-image-2"`) ||
		!strings.Contains(admin.Body.String(), `"openrouter_configured":true`) ||
		strings.Contains(admin.Body.String(), `"api_style"`) {
		t.Fatalf("admin image model picker = %d: %s", admin.Code, admin.Body)
	}

	saved := request(handler, http.MethodPut, "/api/admin/cover-ai", "admin-id", "admin@example.org",
		`{"enabled":true,"model_id":"bytedance-seed/seedream-4.5","catalog_lookup_enabled":true}`)
	if saved.Code != http.StatusOK || !strings.Contains(saved.Body.String(), `"enabled":true`) {
		t.Fatalf("cover settings save = %d: %s", saved.Code, saved.Body)
	}
	invalid := request(handler, http.MethodPut, "/api/admin/cover-ai", "admin-id", "admin@example.org",
		`{"enabled":true,"model_id":"unlisted/model"}`)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("unlisted model accepted: %d %s", invalid.Code, invalid.Body)
	}
}

func TestAdminImageCoverSettingsExplainMissingOpenRouterKey(t *testing.T) {
	cfg := config.Config{
		Env: "production", RequireExe: true, DenyStatus: http.StatusNotFound,
		AdminUserIDs: map[string]bool{"admin-id": true},
	}
	d, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	handler := newWithCatalogAndCoverAI(cfg, d, nil, coverai.NewRegistry())
	response := request(handler, http.MethodGet, "/api/admin/cover-ai", "admin-id", "admin@example.org", "")
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"openrouter_configured":false`) ||
		!strings.Contains(response.Body.String(), "OPENROUTER_API_KEY") ||
		!strings.Contains(response.Body.String(), `"enabled":false`) {
		t.Fatalf("missing OpenRouter key was not explained: %d %s", response.Code, response.Body)
	}
	save := request(handler, http.MethodPut, "/api/admin/cover-ai", "admin-id", "admin@example.org",
		`{"enabled":true,"model_id":"openai/gpt-image-2"}`)
	if save.Code != http.StatusServiceUnavailable {
		t.Fatalf("image generation enabled without API key: %d %s", save.Code, save.Body)
	}
}

func TestOwnerCanQueueCoverRegenerationAndCannotQueueDuplicates(t *testing.T) {
	cfg := config.Config{Env: "production", RequireExe: true, DenyStatus: http.StatusNotFound}
	d, handler := testServer(t, cfg)
	_ = request(handler, http.MethodGet, "/api/me", "cover-user", "reader@example.org", "")
	if err := d.CreateBookAndJob(context.Background(), db.NewBook{
		ID: "cover-regenerate", JobID: "cover-regenerate-job", OwnerUserID: "cover-user",
		Title: "A Book", Author: "An Author", SourceKind: "upload",
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteJob(context.Background(), "cover-regenerate-job"); err != nil {
		t.Fatal(err)
	}
	response := request(handler, http.MethodPost, "/api/books/cover-regenerate/cover-regenerate",
		"cover-user", "reader@example.org", "")
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"cover_regeneration_queued":true`) {
		t.Fatalf("cover regeneration queue = %d: %s", response.Code, response.Body)
	}
	duplicate := request(handler, http.MethodPost, "/api/books/cover-regenerate/cover-regenerate",
		"cover-user", "reader@example.org", "")
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("duplicate cover regeneration = %d: %s", duplicate.Code, duplicate.Body)
	}
	outsider := request(handler, http.MethodPost, "/api/books/cover-regenerate/cover-regenerate",
		"other-user", "other@example.org", "")
	if outsider.Code != http.StatusNotFound {
		t.Fatalf("cross-owner cover regeneration = %d: %s", outsider.Code, outsider.Body)
	}
}

func TestPrivateBookshelfAPIIsNeverBrowserCached(t *testing.T) {
	cfg := config.Config{Env: "production", RequireExe: true, DenyStatus: http.StatusNotFound}
	_, handler := testServer(t, cfg)
	response := request(handler, http.MethodGet, "/api/books", "cache-owner", "owner@example.org", "")
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("books API cache policy = %d %q", response.Code, response.Header().Get("Cache-Control"))
	}
}

func TestAdminBookOperationEndpointsAreAdminOnlyAndQueueCompleteBooks(t *testing.T) {
	cfg := config.Config{
		Env: "production", RequireExe: true, DenyStatus: http.StatusNotFound,
		AdminUserIDs: map[string]bool{"admin-id": true},
	}
	d, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	handler := NewWithCatalog(cfg, d, nil)
	for _, user := range []struct{ id, email string }{
		{"admin-id", "admin@example.org"}, {"source-user", "source@example.org"},
		{"target-user", "target@example.org"},
	} {
		_ = request(handler, http.MethodGet, "/api/me", user.id, user.email, "")
	}
	if err := d.CreateBookAndJob(context.Background(), db.NewBook{
		ID: "admin-copy-book", JobID: "admin-copy-job", OwnerUserID: "source-user",
		Title: "Copy Ready Book", Author: "A. Writer", SourceKind: "upload",
		AudioRelPath: "books/source-user/admin-copy-book/playback.mp3",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(context.Background(), `UPDATE books SET status='ready',
		transcript_relpath='books/source-user/admin-copy-book/transcript.json.gz'
		WHERE id='admin-copy-book'`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(context.Background(), `UPDATE jobs SET status='completed',stage='ready',progress=1
		WHERE id='admin-copy-job'`); err != nil {
		t.Fatal(err)
	}

	deniedBooks := request(handler, http.MethodGet, "/api/admin/users/source-user/books",
		"source-user", "source@example.org", "")
	if deniedBooks.Code != http.StatusNotFound {
		t.Fatalf("non-admin user books status = %d", deniedBooks.Code)
	}
	books := request(handler, http.MethodGet, "/api/admin/users/source-user/books",
		"admin-id", "admin@example.org", "")
	if books.Code != http.StatusOK || !strings.Contains(books.Body.String(), `"Copy Ready Book"`) ||
		strings.Contains(books.Body.String(), `"audio_relpath"`) {
		t.Fatalf("admin source-book list = %d: %s", books.Code, books.Body)
	}
	body := `{"book_id":"admin-copy-book","source_user_id":"source-user","target_user_id":"target-user","mode":"clone"}`
	deniedCopy := request(handler, http.MethodPost, "/api/admin/book-operations",
		"source-user", "source@example.org", body)
	if deniedCopy.Code != http.StatusNotFound {
		t.Fatalf("non-admin copy status = %d", deniedCopy.Code)
	}
	queued := request(handler, http.MethodPost, "/api/admin/book-operations",
		"admin-id", "admin@example.org", body)
	if queued.Code != http.StatusAccepted || !strings.Contains(queued.Body.String(), `"mode":"clone"`) ||
		!strings.Contains(queued.Body.String(), `"status":"queued"`) {
		t.Fatalf("admin clone queue = %d: %s", queued.Code, queued.Body)
	}
	var response struct {
		Operation db.AdminBookOperation `json:"operation"`
	}
	if err := json.Unmarshal(queued.Body.Bytes(), &response); err != nil || response.Operation.ID == "" {
		t.Fatalf("queued response=%#v err=%v", response, err)
	}
	status := request(handler, http.MethodGet, "/api/admin/book-operations/"+response.Operation.ID,
		"admin-id", "admin@example.org", "")
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"status":"queued"`) {
		t.Fatalf("admin operation status = %d: %s", status.Code, status.Body)
	}
}

func TestCoverArtworkAndChoiceAreOwnerScoped(t *testing.T) {
	cfg := config.Config{Env: "production", RequireExe: true, DenyStatus: http.StatusNotFound, DataDir: t.TempDir()}
	d, handler := testServer(t, cfg)
	_ = request(handler, http.MethodGet, "/api/me", "cover-owner", "owner@example.org", "")
	_ = request(handler, http.MethodGet, "/api/me", "cover-outsider", "other@example.org", "")
	if err := d.CreateBookAndJob(context.Background(), db.NewBook{
		ID: "cover-book", OwnerUserID: "cover-owner", Title: "Cover Book", Author: "A. Author",
		SourceKind: "upload", AudioRelPath: "books/cover-owner/cover-book/playback.mp3", JobID: "cover-job",
	}); err != nil {
		t.Fatal(err)
	}
	coverDir := pipeline.BookDirectory(cfg.Normalize().DataDir, "cover-owner", "cover-book")
	coverPath := filepath.Join(coverDir, "cover", "generated.svg")
	aiCandidatePath := filepath.Join(coverDir, "cover", "generated-review.jpg")
	if err := os.MkdirAll(filepath.Dir(coverPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(coverPath, []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`), 0600); err != nil {
		t.Fatal(err)
	}
	var jpegBytes bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, image.NewRGBA(image.Rect(0, 0, 768, 1152)), &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(aiCandidatePath, jpegBytes.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cfg.Normalize().DataDir, coverPath)
	if err != nil {
		t.Fatal(err)
	}
	aiRelative, err := filepath.Rel(cfg.Normalize().DataDir, aiCandidatePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(context.Background(), `UPDATE book_cover_state SET status='review',
		selected_kind='ai_svg',selected_relpath=?,selected_provider='Readalong vector art',
		candidate_kind='catalog',candidate_url='https://covers.openlibrary.org/b/id/77-M.jpg?default=false',
		candidate_provider='https://openlibrary.org/works/OL77W',candidate_year=1920,
		ai_candidate_relpath=?,ai_candidate_year=1935
		WHERE book_id='cover-book'`, relative, aiRelative); err != nil {
		t.Fatal(err)
	}

	ownerCover := request(handler, http.MethodGet, "/api/books/cover-book/cover/selected", "cover-owner", "owner@example.org", "")
	if ownerCover.Code != http.StatusOK || ownerCover.Header().Get("Content-Type") != "image/svg+xml; charset=utf-8" {
		t.Fatalf("owner cover status=%d type=%q body=%s", ownerCover.Code, ownerCover.Header().Get("Content-Type"), ownerCover.Body)
	}
	outsiderCover := request(handler, http.MethodGet, "/api/books/cover-book/cover/selected", "cover-outsider", "other@example.org", "")
	if outsiderCover.Code != http.StatusNotFound {
		t.Fatalf("other user cover status=%d", outsiderCover.Code)
	}
	ownerAICandidate := request(handler, http.MethodGet, "/api/books/cover-book/cover/ai-candidate",
		"cover-owner", "owner@example.org", "")
	outsiderAICandidate := request(handler, http.MethodGet, "/api/books/cover-book/cover/ai-candidate",
		"cover-outsider", "other@example.org", "")
	if ownerAICandidate.Code != http.StatusOK || ownerAICandidate.Header().Get("Content-Type") != "image/jpeg" ||
		ownerAICandidate.Header().Get("Cache-Control") != "private, no-store" ||
		outsiderAICandidate.Code != http.StatusNotFound {
		t.Fatalf("AI candidate access owner=%d outsider=%d", ownerAICandidate.Code, outsiderAICandidate.Code)
	}
	books, err := d.BooksForUser(context.Background(), "cover-owner")
	if err != nil || len(books) != 1 || !books[0].CoverReviewNeeded ||
		books[0].CoverCandidateYear != 1920 || books[0].CoverAICandidateYear != 1935 ||
		!strings.HasPrefix(books[0].CoverAICandidateURL, "/api/books/cover-book/cover/ai-candidate?v=") {
		t.Fatalf("book cover summary=%#v err=%v", books, err)
	}
	versionedAICandidate := request(handler, http.MethodGet, books[0].CoverAICandidateURL,
		"cover-owner", "owner@example.org", "")
	if versionedAICandidate.Code != http.StatusOK ||
		versionedAICandidate.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("versioned AI candidate access = %d cache=%q", versionedAICandidate.Code,
			versionedAICandidate.Header().Get("Cache-Control"))
	}
	chosen := request(handler, http.MethodPost, "/api/books/cover-book/cover-choice",
		"cover-owner", "owner@example.org", `{"action":"use_candidate"}`)
	var updated db.Book
	if err := json.Unmarshal(chosen.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	state, stateErr := d.CoverStateForUser(context.Background(), "cover-owner", "cover-book")
	if chosen.Code != http.StatusOK || updated.CoverKind != "catalog" || updated.CoverReviewNeeded ||
		stateErr != nil || state.CandidateURL != "" || state.AICandidateRelPath != "" || !state.LookupPaused {
		t.Fatalf("choosing candidate = %d: %s", chosen.Code, chosen.Body)
	}
	if _, err := d.ExecContext(context.Background(), `UPDATE book_cover_state SET ai_candidate_relpath=?,ai_candidate_year=1940
		WHERE book_id='cover-book'`, aiRelative); err != nil {
		t.Fatal(err)
	}
	chooseAI := request(handler, http.MethodPost, "/api/books/cover-book/cover-choice",
		"cover-owner", "owner@example.org", `{"action":"use_ai_candidate"}`)
	if chooseAI.Code != http.StatusOK {
		t.Fatalf("AI candidate choice = %d: %s", chooseAI.Code, chooseAI.Body)
	}
	state, stateErr = d.CoverStateForUser(context.Background(), "cover-owner", "cover-book")
	if stateErr != nil || state.SelectedKind != "ai_image" || state.SelectedRelPath != aiRelative ||
		state.AICandidateRelPath != "" || !state.LookupPaused {
		t.Fatalf("AI cover choice state=%#v err=%v", state, stateErr)
	}
	outsiderChoice := request(handler, http.MethodPost, "/api/books/cover-book/cover-choice",
		"cover-outsider", "other@example.org", `{"action":"keep_current"}`)
	if outsiderChoice.Code != http.StatusNotFound {
		t.Fatalf("other user cover choice status=%d", outsiderChoice.Code)
	}
}

func TestPairedSearchReturnsEmptyWithPartialCatalogWarning(t *testing.T) {
	catalogServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/advancedsearch.php":
			_ = json.NewEncoder(w).Encode(map[string]any{"response": map[string]any{"docs": []any{}}})
		case "/api/feed/audiobooks/":
			http.Error(w, "rate limited", http.StatusTooManyRequests)
		default:
			http.NotFound(w, r)
		}
	}))
	defer catalogServer.Close()

	client := catalog.NewClient()
	client.EnableArchiveSearch()
	client.ArchiveBaseURL = catalogServer.URL
	client.ArchiveRequestGap = 0
	client.BaseURL = catalogServer.URL + "/api/feed/audiobooks/"
	client.HTTP = catalogServer.Client()

	cfg := config.Config{Env: "production", RequireExe: true, DenyStatus: http.StatusNotFound}
	d, _ := testServer(t, cfg)
	handler := NewWithCatalog(cfg, d, client)
	search := request(handler, http.MethodGet, "/api/discovery/pairs?q=Confederacy+dunces",
		"catalog-user", "reader@example.org", "")
	if search.Code != http.StatusOK || strings.TrimSpace(search.Body.String()) != "[]" {
		t.Fatalf("partial no-match search = %d: %s", search.Code, search.Body)
	}
	warning := search.Header().Get("X-Readalong-Search-Warning")
	if !strings.Contains(warning, "No matching pairs were found in Internet Archive") ||
		!strings.Contains(warning, "LibriVox fallback is temporarily unavailable") {
		t.Fatalf("unexpected search warning %q", warning)
	}
}

func TestInternetArchivePairRequiresChoosingAndConfirmingTextCandidate(t *testing.T) {
	var pgCatalog bytes.Buffer
	compressor := gzip.NewWriter(&pgCatalog)
	_, _ = compressor.Write([]byte("Text#,Type,Issued,Title,Language,Authors,Subjects,LoCC,Bookshelves\n" +
		"45,Text,2008-06-27,Anne of Green Gables,en,\"Montgomery, L. M. (Lucy Maud), 1874-1942\",Fiction,PZ,Classics\n" +
		"19576,Text,2006-10-20,Anne of Green Gables,en,\"Montgomery, L. M. (Lucy Maud), 1874-1942\",Fiction,PZ,Classics\n"))
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/advancedsearch.php":
			_ = json.NewEncoder(w).Encode(map[string]any{"response": map[string]any{"docs": []any{map[string]any{
				"identifier": "anne_of_green_gables_librivox", "title": "Anne of Green Gables",
				"creator": "Lucy Maud Montgomery", "language": "eng", "runtime": "10:30.11",
				"source":      "Librivox recording of a public-domain text",
				"description": `LibriVox recording. Read by <a href="https://example.org/reader">Karen Savage</a>.`,
				"licenseurl":  "http://creativecommons.org/licenses/publicdomain/",
			}}}})
		case "/pg_catalog.csv.gz":
			_, _ = w.Write(pgCatalog.Bytes())
		case "/metadata/anne_of_green_gables_librivox":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]any{
					"identifier": "anne_of_green_gables_librivox", "title": "Anne of Green Gables",
					"creator": "Lucy Maud Montgomery", "language": "eng", "runtime": "10:30.11",
					"mediatype": "audio", "collection": []string{"librivoxaudio"},
					"source":      "Librivox recording of a public-domain text",
					"description": `LibriVox recording. Read by <a href="https://example.org/reader">Karen Savage</a>.`,
				},
				"files": []any{map[string]any{"name": "anne_01_64kb.mp3", "private": false}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := catalog.NewClient()
	client.ArchiveEnabled = true
	client.ArchiveBaseURL = server.URL
	client.ArchiveRequestGap = 0
	client.GutenbergCatalogURL = server.URL + "/pg_catalog.csv.gz"
	client.HTTP = server.Client()
	cfg := config.Config{Env: "production", RequireExe: true, DenyStatus: http.StatusNotFound}
	d, _ := testServer(t, cfg)
	handler := NewWithCatalog(cfg, d, client)

	search := request(handler, http.MethodGet, "/api/discovery/pairs?q=Anne+Green+Gables",
		"catalog-user", "reader@example.org", "")
	if search.Code != http.StatusOK || !strings.Contains(search.Body.String(), `"provider":"internet_archive"`) ||
		!strings.Contains(search.Body.String(), `"match_kind":"title_author"`) ||
		!strings.Contains(search.Body.String(), `"gutenberg_id":"19576"`) {
		t.Fatalf("Internet Archive search = %d: %s", search.Code, search.Body)
	}
	if strings.Contains(search.Body.String(), `"url_zip_file"`) || strings.Contains(search.Body.String(), `"source_url"`) {
		t.Fatalf("search exposed an import URL: %s", search.Body)
	}

	path := "/api/discovery/pairs/ia-anne_of_green_gables_librivox/import"
	unselected := request(handler, http.MethodPost, path, "catalog-user", "reader@example.org",
		`{"rights_confirmed":true,"match_confirmed":true}`)
	if unselected.Code != http.StatusBadRequest {
		t.Fatalf("missing text selection status = %d: %s", unselected.Code, unselected.Body)
	}
	unconfirmed := request(handler, http.MethodPost, path, "catalog-user", "reader@example.org",
		`{"rights_confirmed":true,"gutenberg_id":"45"}`)
	if unconfirmed.Code != http.StatusBadRequest {
		t.Fatalf("unconfirmed title-author match status = %d: %s", unconfirmed.Code, unconfirmed.Body)
	}
	imported := request(handler, http.MethodPost, path, "catalog-user", "reader@example.org",
		`{"rights_confirmed":true,"gutenberg_id":"45","match_confirmed":true}`)
	if imported.Code != http.StatusAccepted {
		t.Fatalf("selected text import = %d: %s", imported.Code, imported.Body)
	}
	var book db.Book
	if err := json.Unmarshal(imported.Body.Bytes(), &book); err != nil {
		t.Fatal(err)
	}
	if book.Mode != "aligned" || book.SourceKind != "librivox" || book.GutenbergID != "45" ||
		book.Title != "Anne of Green Gables" || book.EbookSourceURL != "https://www.gutenberg.org/ebooks/45" {
		t.Fatalf("unexpected imported IA pair: %#v", book)
	}
}

func TestRetranscribeEndpointQueuesFreshJobWithoutReplacingCurrentTranscript(t *testing.T) {
	dataDir := t.TempDir()
	cfg := config.Config{
		Env: "production", RequireExe: true, DenyStatus: http.StatusNotFound,
		DataDir: dataDir, GroqAPIKey: "test-key",
	}
	d, err := db.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	handler := New(cfg, d, pipeline.New(cfg, d))
	_ = request(handler, http.MethodGet, "/api/me", "retranscribe-owner", "owner@example.org", "")
	if err := d.CreateBookAndJob(context.Background(), db.NewBook{
		ID: "retranscribe-book", JobID: "initial-job", OwnerUserID: "retranscribe-owner",
		Title: "Test book", SourceKind: "upload", AudioRelPath: "books/test/playback.mp3",
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetTranscriptPath(context.Background(), "retranscribe-book", "books/test/old/transcript.v1.json.gz"); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteJob(context.Background(), "initial-job"); err != nil {
		t.Fatal(err)
	}

	response := request(handler, http.MethodPost, "/api/books/retranscribe-book/retranscribe",
		"retranscribe-owner", "owner@example.org", "")
	if response.Code != http.StatusAccepted {
		t.Fatalf("re-transcribe status = %d: %s", response.Code, response.Body)
	}
	var book db.Book
	if err := json.Unmarshal(response.Body.Bytes(), &book); err != nil {
		t.Fatal(err)
	}
	if book.Status != "ready" || book.JobStatus != "queued" {
		t.Fatalf("unexpected queued book response: %#v", book)
	}
	stored, err := d.BookByID(context.Background(), "retranscribe-book")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "ready" || stored.TranscriptRelPath != "books/test/old/transcript.v1.json.gz" {
		t.Fatalf("queueing replaced or hid the current transcript: %#v", stored)
	}

	outsider := request(handler, http.MethodPost, "/api/books/retranscribe-book/retranscribe",
		"other-user", "other@example.org", "")
	if outsider.Code != http.StatusNotFound {
		t.Fatalf("other owner could retranscribe book: status=%d body=%s", outsider.Code, outsider.Body)
	}
	busy := request(handler, http.MethodPost, "/api/books/retranscribe-book/retranscribe",
		"retranscribe-owner", "owner@example.org", "")
	if busy.Code != http.StatusConflict {
		t.Fatalf("duplicate re-transcription status = %d: %s", busy.Code, busy.Body)
	}
}

func TestReaderUsesTranscriptFallbackAndAllowsCanonicalEPUBView(t *testing.T) {
	dataDir := t.TempDir()
	cfg := config.Config{Env: "production", RequireExe: true, DenyStatus: http.StatusNotFound, DataDir: dataDir}
	d, err := db.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	handler := New(cfg, d)
	_ = request(handler, http.MethodGet, "/api/me", "reader-owner", "reader@example.org", "")
	if err := d.CreateBookAndJob(httptest.NewRequest(http.MethodGet, "/", nil).Context(), db.NewBook{
		ID: "reader-book", JobID: "reader-job", OwnerUserID: "reader-owner", Title: "Pair",
		SourceKind: "upload", AudioRelPath: "books/audio.mp3", EpubRelPath: "books/book.epub",
	}); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(dataDir, "transcript.v1.json.gz")
	alignmentPath := filepath.Join(dataDir, "alignment.v1.json.gz")
	transcriptDoc := transcript.Document{Version: 1, DurationMS: 5000, Sentences: []transcript.Sentence{{
		ID: "asr", ParagraphID: "asr-p", StartMS: 1000, EndMS: 1500,
		Words: []transcript.Word{{Text: "Audio", StartMS: 1000, EndMS: 1250, Confidence: 1}, {Text: "words.", StartMS: 1260, EndMS: 1500, Confidence: 1}},
	}}}
	alignmentDoc := align.EbookAlignment{
		Version: 1, Quality: 0.65, MatchedWords: 2, TotalWords: 4,
		Chapters: []align.EbookChapter{{ID: "ch1", Title: "Chapter One", Ordinal: 0, StartMS: 1000, EndMS: 1500}},
		Sentences: transcript.Document{Version: 1, DurationMS: 5000, Sentences: []transcript.Sentence{{
			ID: "ebook", ParagraphID: "p1", StartMS: 1000, EndMS: 1500,
			Words: []transcript.Word{{Text: "Canonical", StartMS: 1000, EndMS: 1250, Confidence: 1}, {Text: "text.", StartMS: 0, EndMS: 0}},
		}}},
	}
	writeCompressedJSON(t, transcriptPath, transcriptDoc)
	writeCompressedJSON(t, alignmentPath, alignmentDoc)
	if err := d.SetTranscriptPath(context.Background(), "reader-book", "transcript.v1.json.gz"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetAlignment(context.Background(), "reader-book", "alignment.v1.json.gz", alignmentDoc.Quality); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		query, want string
	}{
		{"", "transcript"},
		{"?content=transcript", "transcript"},
		{"?content=ebook", "ebook"},
	} {
		res := request(handler, http.MethodGet, "/api/books/reader-book/reader"+tc.query, "reader-owner", "reader@example.org", "")
		if res.Code != http.StatusOK {
			t.Fatalf("reader query %q status=%d: %s", tc.query, res.Code, res.Body)
		}
		var payload struct {
			ContentMode string                `json:"content_mode"`
			Sentences   []transcript.Sentence `json:"sentences"`
		}
		if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.ContentMode != tc.want {
			t.Fatalf("reader query %q returned mode %q", tc.query, payload.ContentMode)
		}
		wantText := "Audio"
		if tc.want == "ebook" {
			wantText = "Canonical"
		}
		if len(payload.Sentences) != 1 || payload.Sentences[0].Words[0].Text != wantText {
			t.Fatalf("reader query %q returned %#v", tc.query, payload.Sentences)
		}
	}
	invalid := request(handler, http.MethodGet, "/api/books/reader-book/reader?content=other",
		"reader-owner", "reader@example.org", "")
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid reader mode status = %d", invalid.Code)
	}
}

func writeCompressedJSON(t *testing.T, filename string, value any) {
	t.Helper()
	file, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(file)
	if err := json.NewEncoder(gz).Encode(value); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func request(handler http.Handler, method, path, id, email, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions {
		req.Header.Set("Origin", "https://"+req.Host)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	if id != "" {
		req.Header.Set("X-ExeDev-UserID", id)
	}
	if email != "" {
		req.Header.Set("X-ExeDev-Email", email)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	return res
}

func TestFirstVisitProvisioningAdminAndOwnerIsolation(t *testing.T) {
	cfg := config.Config{
		Env: "production", RequireExe: true, DenyStatus: http.StatusNotFound,
		AdminBootstrapEmails: map[string]bool{"admin@example.org": true},
	}
	d, handler := testServer(t, cfg)
	admin := request(handler, http.MethodGet, "/api/me", "exe-id-1", "admin@example.org", "")
	if admin.Code != http.StatusOK {
		t.Fatalf("admin /api/me status = %d: %s", admin.Code, admin.Body)
	}
	var me struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := json.Unmarshal(admin.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.ID != "exe-id-1" || me.Role != "admin" {
		t.Fatalf("unexpected admin profile: %#v", me)
	}
	regular := request(handler, http.MethodGet, "/api/books", "exe-id-2", "reader@example.org", "")
	if regular.Code != http.StatusOK {
		t.Fatalf("regular first visit status = %d: %s", regular.Code, regular.Body)
	}
	if _, err := d.Exec(`INSERT INTO books(id,owner_user_id,title,created_at,updated_at)
		VALUES('private-book','exe-id-1','Private','2026-01-01','2026-01-01')`); err != nil {
		t.Fatal(err)
	}
	outsider := request(handler, http.MethodGet, "/api/books/private-book/reader", "exe-id-2", "reader@example.org", "")
	if outsider.Code != http.StatusNotFound {
		t.Fatalf("cross-user reader status = %d, want 404", outsider.Code)
	}
	owner := request(handler, http.MethodGet, "/api/books/private-book/reader", "exe-id-1", "admin@example.org", "")
	if owner.Code != http.StatusOK {
		t.Fatalf("admin cannot access own book: %d %s", owner.Code, owner.Body)
	}
	deniedAdminList := request(handler, http.MethodGet, "/api/admin/users", "exe-id-2", "reader@example.org", "")
	if deniedAdminList.Code != http.StatusNotFound {
		t.Fatalf("regular user admin API status = %d, want 404", deniedAdminList.Code)
	}
	adminList := request(handler, http.MethodGet, "/api/admin/users", "exe-id-1", "admin@example.org", "")
	if adminList.Code != http.StatusOK {
		t.Fatalf("admin API status = %d: %s", adminList.Code, adminList.Body)
	}
	if !strings.Contains(adminList.Body.String(), `"book_count":1`) {
		t.Fatalf("admin list missing owned book count: %s", adminList.Body)
	}
}

func TestAdminCanSuspendButNotSelfSuspend(t *testing.T) {
	cfg := config.Config{
		Env: "production", RequireExe: true, DenyStatus: http.StatusNotFound,
		AdminUserIDs: map[string]bool{"admin-id": true},
	}
	_, handler := testServer(t, cfg)
	_ = request(handler, http.MethodGet, "/api/me", "admin-id", "admin@example.org", "")
	_ = request(handler, http.MethodGet, "/api/me", "user-id", "user@example.org", "")

	self := request(handler, http.MethodPut, "/api/admin/users/admin-id", "admin-id", "admin@example.org", `{"status":"suspended"}`)
	if self.Code != http.StatusBadRequest {
		t.Fatalf("self-suspend status = %d, want 400", self.Code)
	}
	suspend := request(handler, http.MethodPut, "/api/admin/users/user-id", "admin-id", "admin@example.org", `{"status":"suspended"}`)
	if suspend.Code != http.StatusOK {
		t.Fatalf("suspend status = %d: %s", suspend.Code, suspend.Body)
	}
	visit := request(handler, http.MethodGet, "/api/me", "user-id", "user@example.org", "")
	if visit.Code != http.StatusNotFound {
		t.Fatalf("suspended user status = %d, want 404", visit.Code)
	}
	restore := request(handler, http.MethodPut, "/api/admin/users/user-id", "admin-id", "admin@example.org", `{"status":"active"}`)
	if restore.Code != http.StatusOK {
		t.Fatalf("reactivate status = %d: %s", restore.Code, restore.Body)
	}
}

func TestMutationRequiresSameOrigin(t *testing.T) {
	server := &Server{cfg: config.Config{Env: "production"}}
	handler := server.sameOrigin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, tc := range []struct {
		name, origin, fetchSite string
		want                    int
	}{
		{name: "same origin", origin: "https://app.example.org", fetchSite: "same-origin", want: http.StatusNoContent},
		{name: "cross origin", origin: "https://evil.example", fetchSite: "cross-site", want: http.StatusForbidden},
		{name: "missing origin", want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://app.example.org/api/books", nil)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != tc.want {
				t.Fatalf("status = %d, want %d", res.Code, tc.want)
			}
		})
	}
}

func TestMultipartYouTubeImportIsProvisionedAndOwnerScoped(t *testing.T) {
	cfg := config.Config{Env: "production", RequireExe: true, DenyStatus: http.StatusNotFound}
	d, handler := testServer(t, cfg)
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	_ = form.WriteField("source_url", "https://youtu.be/example-id")
	_ = form.WriteField("title", "A private book")
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/books", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Origin", "https://"+req.Host)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("X-ExeDev-UserID", "importer")
	req.Header.Set("X-ExeDev-Email", "importer@example.org")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("import status = %d: %s", res.Code, res.Body)
	}
	var book db.Book
	if err := json.Unmarshal(res.Body.Bytes(), &book); err != nil {
		t.Fatal(err)
	}
	if book.ID == "" || book.Title != "A private book" || book.SourceKind != "youtube" || book.Status != "queued" {
		t.Fatalf("unexpected import response: %#v", book)
	}
	if strings.Contains(res.Body.String(), "audio_relpath") || strings.Contains(res.Body.String(), "source_url") {
		t.Fatalf("API exposed private storage/source details: %s", res.Body)
	}
	outsider := request(handler, http.MethodGet, "/api/books", "other-user", "other@example.org", "")
	if outsider.Code != http.StatusOK || strings.Contains(outsider.Body.String(), book.ID) {
		t.Fatalf("separate bookshelf not isolated: %d %s", outsider.Code, outsider.Body)
	}
	if _, err := d.BookForUser(httptest.NewRequest(http.MethodGet, "/", nil).Context(), "other-user", book.ID); err == nil {
		t.Fatal("other user unexpectedly owns imported book")
	}
}

func TestMultipartAudioEPUBImportCreatesAlignedBook(t *testing.T) {
	absoluteDataDir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dataDir, err := filepath.Rel(cwd, absoluteDataDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Env: "production", RequireExe: true, DenyStatus: http.StatusNotFound, DataDir: dataDir}
	d, handler := testServer(t, cfg)
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	audio, err := form.CreateFormFile("audio_file", "story.mp3")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = audio.Write([]byte("test-audio"))
	ebook, err := form.CreateFormFile("epub_file", "story.epub")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = ebook.Write(httpTestEPUB(t))
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/books", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Origin", "https://"+req.Host)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("X-ExeDev-UserID", "ebook-owner")
	req.Header.Set("X-ExeDev-Email", "reader@example.org")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusAccepted {
		t.Fatalf("audio+EPUB import = %d: %s", res.Code, res.Body)
	}
	var book db.Book
	if err := json.Unmarshal(res.Body.Bytes(), &book); err != nil {
		t.Fatal(err)
	}
	if book.Mode != "aligned" || book.Title != "Test Book" || book.Author != "Test Author" {
		t.Fatalf("unexpected paired upload: %#v", book)
	}
	if strings.Contains(res.Body.String(), "epub_relpath") || strings.Contains(res.Body.String(), "audio_relpath") {
		t.Fatalf("response exposed private storage paths: %s", res.Body)
	}
	stored, err := d.BookForUser(httptest.NewRequest(http.MethodGet, "/", nil).Context(), "ebook-owner", book.ID)
	if err != nil || stored.EpubRelPath == "" {
		t.Fatalf("stored EPUB path missing: book=%#v err=%v", stored, err)
	}
	for _, rel := range []string{stored.AudioRelPath, stored.EpubRelPath} {
		if _, err := os.Stat(filepath.Join(dataDir, rel)); err != nil {
			t.Fatalf("uploaded media missing under relative data dir: %s: %v", rel, err)
		}
	}
}

func TestRetryEndpointReturnsEmptyAcceptedResponse(t *testing.T) {
	cfg := config.Config{Env: "production", RequireExe: true, DenyStatus: http.StatusNotFound}
	d, handler := testServer(t, cfg)
	ctx := context.Background()
	_ = request(handler, http.MethodGet, "/api/me", "retry-owner", "retry@example.org", "")
	if err := d.CreateBookAndJob(ctx, db.NewBook{
		ID: "retry-book", JobID: "retry-job", OwnerUserID: "retry-owner",
		Title: "Retry test", SourceKind: "upload",
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.FailJob(ctx, "retry-job", "acquiring", "error", "test failure"); err != nil {
		t.Fatal(err)
	}

	response := request(handler, http.MethodPost, "/api/books/retry-book/retry",
		"retry-owner", "retry@example.org", "")
	if response.Code != http.StatusAccepted || response.Body.Len() != 0 {
		t.Fatalf("retry response = %d %q, want empty 202", response.Code, response.Body.String())
	}
	job, err := d.JobStatus(ctx, "retry-book")
	if err != nil || job.Status != "queued" {
		t.Fatalf("retry job state = %#v, err=%v", job, err)
	}
}

func httpTestEPUB(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	zw := zip.NewWriter(&buffer)
	entries := map[string]string{
		"META-INF/container.xml": `<container><rootfiles><rootfile full-path="OPS/book.opf"/></rootfiles></container>`,
		"OPS/book.opf":           `<package><metadata><title>Test Book</title><creator>Test Author</creator><language>en</language></metadata><manifest><item id="c1" href="chapter.xhtml" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="c1"/></spine></package>`,
		"OPS/chapter.xhtml":      `<html xmlns="http://www.w3.org/1999/xhtml"><body><p>Audio and ebook test.</p></body></html>`,
	}
	for name, value := range entries {
		entry, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestAudioRouteServesRangeOnlyToOwner(t *testing.T) {
	cfg := config.Config{Env: "production", RequireExe: true, DenyStatus: http.StatusNotFound}
	dataDir := t.TempDir()
	cfg.DataDir = dataDir
	d, err := db.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	handler := New(cfg, d)
	_ = request(handler, http.MethodGet, "/api/me", "audio-owner", "owner@example.org", "")
	mediaDir := filepath.Join(dataDir, "media")
	if err := os.MkdirAll(mediaDir, 0700); err != nil {
		t.Fatal(err)
	}
	audioPath := filepath.Join(mediaDir, "playback.mp3")
	if err := os.WriteFile(audioPath, []byte("0123456789"), 0600); err != nil {
		t.Fatal(err)
	}
	bookID, jobID := "range-book", "range-job"
	if err := d.CreateBookAndJob(httptest.NewRequest(http.MethodGet, "/", nil).Context(), db.NewBook{
		ID: bookID, JobID: jobID, OwnerUserID: "audio-owner", Title: "Range test",
		SourceKind: "upload", AudioRelPath: "media/playback.mp3",
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/books/"+bookID+"/audio", nil)
	req.Header.Set("X-ExeDev-UserID", "audio-owner")
	req.Header.Set("X-ExeDev-Email", "owner@example.org")
	req.Header.Set("Range", "bytes=2-5")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusPartialContent || res.Body.String() != "2345" ||
		res.Header().Get("Content-Range") != "bytes 2-5/10" || res.Header().Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("range response = %d %q %q", res.Code, res.Body.String(), res.Header().Get("Content-Range"))
	}
	outsider := request(handler, http.MethodGet, "/api/books/"+bookID+"/audio", "other-user", "other@example.org", "")
	if outsider.Code != http.StatusNotFound {
		t.Fatalf("other user audio status = %d, want 404", outsider.Code)
	}
}
