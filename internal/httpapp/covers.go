package httpapp

import (
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
		(request.Action != "use_candidate" && request.Action != "keep_current") {
		http.Error(w, "choose the catalog cover or keep the current cover", http.StatusBadRequest)
		return
	}
	bookID := r.PathValue("id")
	chooseCandidate := request.Action == "use_candidate"
	if err := s.db.ChooseCoverCandidate(r.Context(), user.ID, bookID, chooseCandidate); err != nil {
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
