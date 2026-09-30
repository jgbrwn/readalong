package httpapp

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
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
	dataDir := t.TempDir()
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
