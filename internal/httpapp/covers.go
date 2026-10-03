package httpapp

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/jgbrwn/readalong/internal/auth"
	"github.com/jgbrwn/readalong/internal/db"
	"github.com/jgbrwn/readalong/internal/pipeline"
)

func (s *Server) coverFile(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	bookID := r.PathValue("id")
	if _, err := s.db.BookForUser(r.Context(), user.ID, bookID); err != nil {
		http.NotFound(w, r)
		return
	}
	state, err := s.db.CoverStateForUser(r.Context(), user.ID, bookID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var relative string
	switch r.PathValue("variant") {
	case "selected":
		relative = state.SelectedRelPath
	case "candidate":
		relative = state.CandidateRelPath
	case "ai-candidate":
		relative = state.AICandidateRelPath
	default:
		http.NotFound(w, r)
		return
	}
	path, err := localDataPath(s.cfg.DataDir, relative)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	bookDir, err := filepath.Abs(pipeline.BookDirectory(s.cfg.DataDir, user.ID, bookID))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rel, err := filepath.Rel(bookDir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		http.NotFound(w, r)
		return
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Size() <= 0 || info.Size() > 20<<20 {
		http.NotFound(w, r)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	switch strings.ToLower(filepath.Ext(path)) {
	case ".svg":
		w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	case ".jpg", ".jpeg":
		w.Header().Set("Content-Type", "image/jpeg")
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=300")
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), file)
}

func (s *Server) chooseCover(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	var request struct {
		Action string `json:"action"`
	}
	if err := decodeAdminJSON(w, r, 4096, &request); err != nil ||
		(request.Action != "use_candidate" && request.Action != "use_ai_candidate" &&
			request.Action != "keep_current") {
		http.Error(w, "choose a suggested cover or keep the current cover", http.StatusBadRequest)
		return
	}
	bookID := r.PathValue("id")
	if err := s.db.ChooseCoverCandidate(r.Context(), user.ID, bookID, request.Action); err != nil {
		if err == db.ErrBookNotFound {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "cover choice could not be saved", http.StatusInternalServerError)
		return
	}
	book, err := s.db.BookForUser(r.Context(), user.ID, bookID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	jsonOut(w, book)
}

func (s *Server) regenerateCover(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFromContext(r.Context())
	bookID := r.PathValue("id")
	if _, err := s.db.BookForUser(r.Context(), user.ID, bookID); err != nil {
		http.NotFound(w, r)
		return
	}
	settings, err := s.readCoverAISettings(r.Context())
	if err != nil {
		http.Error(w, "cover settings could not be loaded", http.StatusInternalServerError)
		return
	}
	if !settings.CatalogLookupEnabled && !settings.Enabled {
		http.Error(w, "cover lookup and generated covers are both disabled by the administrator", http.StatusConflict)
		return
	}
	if err := s.db.QueueCoverRegeneration(r.Context(), user.ID, bookID); err != nil {
		switch {
		case errors.Is(err, db.ErrBookNotFound):
			http.NotFound(w, r)
		case errors.Is(err, db.ErrBookJobInProgress), errors.Is(err, db.ErrBookOperationInProgress),
			errors.Is(err, db.ErrBookDeleteInProgress), errors.Is(err, db.ErrBookCoverInProgress):
			http.Error(w, "this book is already being updated; try again shortly", http.StatusConflict)
		default:
			http.Error(w, "cover regeneration could not be queued", http.StatusInternalServerError)
		}
		return
	}
	book, err := s.db.BookForUser(r.Context(), user.ID, bookID)
	if err != nil {
		http.Error(w, "cover regeneration was queued but the book status could not be loaded", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	jsonOut(w, book)
}
