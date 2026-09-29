package httpapp

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jgbrwn/readalong/internal/config"
	"github.com/jgbrwn/readalong/internal/db"
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
