package httpapp

import (
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jgbrwn/readalong/internal/auth"
	"github.com/jgbrwn/readalong/internal/db"
	"github.com/jgbrwn/readalong/internal/media"
	"github.com/jgbrwn/readalong/internal/pipeline"
	"github.com/jgbrwn/readalong/internal/transcript"
)

const (
	multipartMemoryBytes = 8 << 20
	readerWindowMS       = int64(5 * 60 * 1000)
	maxReaderWindowMS    = int64(10 * 60 * 1000)
)

func (s *Server) createBook(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	maxUpload := s.cfg.MaxUploadBytes
	if maxUpload <= 0 {
		maxUpload = 2 << 30
	}
	if r.Header.Get("Content-Type") == "" || !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		http.Error(w, "multipart form required", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+(8<<20))
	if err := r.ParseMultipartForm(multipartMemoryBytes); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "upload exceeds the configured size limit", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid multipart form", http.StatusBadRequest)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if len(r.MultipartForm.File["epub_file"]) > 0 {
		http.Error(w, "EPUB alignment is not available yet; import the audio without the EPUB for now", http.StatusNotImplemented)
		return
	}
	files := r.MultipartForm.File["audio_file"]
	if len(files) > 1 {
		http.Error(w, "upload one audio file at a time", http.StatusBadRequest)
		return
	}
	sourceURL := strings.TrimSpace(r.FormValue("source_url"))
	if len(sourceURL) > 4096 {
		http.Error(w, "URL is too long", http.StatusBadRequest)
		return
	}
	if (sourceURL == "") == (len(files) == 0) {
		http.Error(w, "provide exactly one URL or audio file", http.StatusBadRequest)
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	if len(title) > 255 || hasControlChars(title) {
		http.Error(w, "invalid title", http.StatusBadRequest)
		return
	}
	if title == "" {
		title = "Untitled"
	}
	sourceKind := "upload"
	normalizedURL := ""
	if sourceURL != "" {
		var err error
		sourceKind, normalizedURL, err = media.ClassifyURL(sourceURL)
		if err != nil {
			http.Error(w, "use a public HTTP(S) audio URL or a YouTube URL", http.StatusBadRequest)
			return
		}
	}
	id, err := randomID()
	if err != nil {
		http.Error(w, "could not create book", http.StatusInternalServerError)
		return
	}
	jobID, err := randomID()
	if err != nil {
		http.Error(w, "could not create book", http.StatusInternalServerError)
		return
	}
	bookDir := pipeline.BookDirectory(s.cfg.DataDir, u.ID, id)
	audioRel := ""
	if len(files) == 1 {
		header := files[0]
		if header.Size > maxUpload {
			http.Error(w, "upload exceeds the configured size limit", http.StatusRequestEntityTooLarge)
			return
		}
		ext := strings.ToLower(filepath.Ext(header.Filename))
		if !supportedAudioExtension(ext) {
			http.Error(w, "unsupported audio file type", http.StatusBadRequest)
			return
		}
		sourceDir := filepath.Join(bookDir, "source")
		if err := os.MkdirAll(sourceDir, 0700); err != nil {
			http.Error(w, "could not prepare upload", http.StatusInternalServerError)
			return
		}
		audioPath := filepath.Join(sourceDir, "upload"+ext)
		if err := saveMultipartFile(w, header, audioPath, maxUpload); err != nil {
			_ = os.RemoveAll(bookDir)
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				http.Error(w, "upload exceeds the configured size limit", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "could not save uploaded audio", http.StatusBadRequest)
			return
		}
		audioRel, err = filepath.Rel(s.cfg.DataDir, audioPath)
		if err != nil {
			_ = os.RemoveAll(bookDir)
			http.Error(w, "could not save uploaded audio", http.StatusInternalServerError)
			return
		}
	}
	if err := s.db.CreateBookAndJob(r.Context(), db.NewBook{
		ID: id, OwnerUserID: u.ID, Title: title, SourceKind: sourceKind,
		SourceURL: normalizedURL, AudioRelPath: audioRel, JobID: jobID,
	}); err != nil {
		_ = os.RemoveAll(bookDir)
		http.Error(w, "could not create book", http.StatusInternalServerError)
		return
	}
	book, err := s.db.BookForUser(r.Context(), u.ID, id)
	if err != nil {
		http.Error(w, "book was created but could not be loaded", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	jsonOut(w, book)
}

func saveMultipartFile(w http.ResponseWriter, header *multipart.FileHeader, dest string, maxBytes int64) error {
	in, err := header.Open()
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".upload-*.partial")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	n, err := io.Copy(tmp, io.LimitReader(in, maxBytes+1))
	if err != nil {
		_ = tmp.Close()
		return err
	}
	if n > maxBytes {
		_ = tmp.Close()
		return &http.MaxBytesError{Limit: maxBytes}
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dest)
}

func supportedAudioExtension(ext string) bool {
	switch ext {
	case ".mp3", ".m4a", ".m4b", ".wav", ".flac", ".ogg", ".opus", ".webm", ".mp4":
		return true
	default:
		return false
	}
}

func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (s *Server) getBook(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	book, err := s.db.BookForUser(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	chapters, err := s.db.Chapters(r.Context(), book.ID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	jsonOut(w, map[string]any{"book": book, "chapters": chapters})
}

func (s *Server) deleteBook(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	book, err := s.db.BookForUser(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if s.pipeline != nil {
		s.pipeline.CancelBook(book.ID)
	}
	if err := s.db.DeleteBook(r.Context(), u.ID, book.ID); err != nil {
		http.NotFound(w, r)
		return
	}
	_ = os.RemoveAll(pipeline.BookDirectory(s.cfg.DataDir, u.ID, book.ID))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) reader(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	book, err := s.db.BookForUser(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	chapters, err := s.db.Chapters(r.Context(), book.ID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	payload := map[string]any{
		"book": book, "chapters": chapters, "ready": false, "sentences": []transcript.Sentence{},
	}
	if book.TranscriptRelPath == "" {
		jsonOut(w, payload)
		return
	}
	path, err := localDataPath(s.cfg.DataDir, book.TranscriptRelPath)
	if err != nil {
		http.Error(w, "reader data unavailable", http.StatusInternalServerError)
		return
	}
	var doc transcript.Document
	if err := readTranscript(path, &doc); err != nil {
		http.Error(w, "reader data unavailable", http.StatusInternalServerError)
		return
	}
	startMS, err := optionalInt64(r, "start_ms", book.PositionMS)
	if err != nil {
		http.Error(w, "invalid start_ms", http.StatusBadRequest)
		return
	}
	if startMS < 0 {
		startMS = 0
	}
	endMS, err := optionalInt64(r, "end_ms", startMS+readerWindowMS)
	if err != nil || endMS <= startMS {
		http.Error(w, "invalid reader window", http.StatusBadRequest)
		return
	}
	if endMS-startMS > maxReaderWindowMS {
		endMS = startMS + maxReaderWindowMS
	}
	if book.DurationMS > 0 && endMS > book.DurationMS {
		endMS = book.DurationMS
	}
	payload["ready"] = true
	payload["start_ms"] = startMS
	payload["end_ms"] = endMS
	payload["sentences"] = transcript.Window(doc, startMS, endMS)
	payload["total_sentences"] = len(doc.Sentences)
	w.Header().Set("Cache-Control", "private, no-store")
	jsonOut(w, payload)
}

func (s *Server) audio(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	book, err := s.db.BookForUser(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	path, err := localDataPath(s.cfg.DataDir, book.AudioRelPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	contentType := mime.TypeByExtension(filepath.Ext(path))
	if contentType == "" {
		contentType = "audio/mpeg"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, "playback"+filepath.Ext(path), info.ModTime(), file)
}

func (s *Server) updateProgress(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	book, err := s.db.BookForUser(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var req struct {
		PositionMS   int64           `json:"position_ms"`
		PlaybackRate float64         `json:"playback_rate"`
		SyncOffsetMS int64           `json:"sync_offset_ms"`
		Appearance   json.RawMessage `json:"appearance"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid progress payload", http.StatusBadRequest)
		return
	}
	if req.PositionMS < 0 || req.PlaybackRate < 0.5 || req.PlaybackRate > 3 ||
		math.IsNaN(req.PlaybackRate) || math.IsInf(req.PlaybackRate, 0) ||
		req.SyncOffsetMS < -10000 || req.SyncOffsetMS > 10000 {
		http.Error(w, "progress values out of range", http.StatusBadRequest)
		return
	}
	if book.DurationMS > 0 && req.PositionMS > book.DurationMS {
		req.PositionMS = book.DurationMS
	}
	appearance := "{}"
	if len(req.Appearance) > 0 {
		var value map[string]any
		if json.Unmarshal(req.Appearance, &value) != nil || value == nil || len(req.Appearance) > 8192 {
			http.Error(w, "appearance must be a small JSON object", http.StatusBadRequest)
			return
		}
		appearance = string(req.Appearance)
	}
	if err := s.db.SaveProgress(r.Context(), u.ID, book.ID, req.PositionMS, req.PlaybackRate,
		req.SyncOffsetMS, appearance); err != nil {
		http.Error(w, "could not save progress", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) retryBook(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	id := r.PathValue("id")
	if err := s.db.RetryJob(r.Context(), u.ID, id); err != nil {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	bookID := r.PathValue("id")
	if _, err := s.db.BookForUser(r.Context(), u.ID, bookID); err != nil {
		http.NotFound(w, r)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		book, err := s.db.BookForUser(r.Context(), u.ID, bookID)
		if err != nil {
			return
		}
		job, err := s.db.JobStatus(r.Context(), bookID)
		if err != nil {
			return
		}
		data, _ := json.Marshal(map[string]any{
			"status": book.Status, "job_status": job.Status, "stage": job.Stage, "progress": job.Progress,
			"error": job.Error, "not_before_at": job.NotBeforeAt,
		})
		_, _ = fmt.Fprintf(w, "event: progress\ndata: %s\n\n", data)
		flusher.Flush()
		if job.Status == "completed" || job.Status == "error" ||
			(job.Status == "queued" && job.NotBeforeAt != "") {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func localDataPath(root, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", fmt.Errorf("invalid media path")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	path := filepath.Join(rootAbs, filepath.Clean(rel))
	relToRoot, err := filepath.Rel(rootAbs, path)
	if err != nil || relToRoot == ".." || strings.HasPrefix(relToRoot, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid media path")
	}
	return path, nil
}

func readTranscript(path string, dst any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	return json.NewDecoder(io.LimitReader(gz, 128<<20)).Decode(dst)
}

func optionalInt64(r *http.Request, key string, fallback int64) (int64, error) {
	values := r.URL.Query()[key]
	if len(values) == 0 {
		return fallback, nil
	}
	return strconv.ParseInt(values[0], 10, 64)
}

func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
