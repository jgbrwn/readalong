package httpapp

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jgbrwn/readalong/internal/auth"
	"github.com/jgbrwn/readalong/internal/db"
)

func (s *Server) adminUserBooks(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	userID := strings.TrimSpace(r.PathValue("id"))
	if userID == "" || len(userID) > 255 {
		http.Error(w, "invalid user id", http.StatusBadRequest)
		return
	}
	if _, err := s.db.UserByID(r.Context(), userID); err != nil {
		http.NotFound(w, r)
		return
	}
	books, err := s.db.BooksForUser(r.Context(), userID)
	if err != nil {
		http.Error(w, "account books are unavailable", http.StatusInternalServerError)
		return
	}
	jsonOut(w, books)
}

func (s *Server) createAdminBookOperation(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	var request struct {
		BookID       string `json:"book_id"`
		SourceUserID string `json:"source_user_id"`
		TargetUserID string `json:"target_user_id"`
		Mode         string `json:"mode"`
	}
	if err := decodeAdminJSON(w, r, 4096, &request); err != nil {
		http.Error(w, "invalid bookshelf operation request", http.StatusBadRequest)
		return
	}
	request.BookID = strings.TrimSpace(request.BookID)
	request.SourceUserID = strings.TrimSpace(request.SourceUserID)
	request.TargetUserID = strings.TrimSpace(request.TargetUserID)
	if request.BookID == "" || len(request.BookID) > 255 ||
		request.SourceUserID == "" || len(request.SourceUserID) > 255 ||
		request.TargetUserID == "" || len(request.TargetUserID) > 255 ||
		(request.Mode != "clone" && request.Mode != "transfer") {
		http.Error(w, "choose a book, source, recipient, and operation", http.StatusBadRequest)
		return
	}
	if request.SourceUserID == request.TargetUserID {
		http.Error(w, "choose two different accounts", http.StatusBadRequest)
		return
	}
	source, err := s.db.BookForUser(r.Context(), request.SourceUserID, request.BookID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	target, err := s.db.UserByID(r.Context(), request.TargetUserID)
	if err != nil || target.Status != "active" {
		http.Error(w, "choose an existing active recipient account", http.StatusBadRequest)
		return
	}
	operationID, err := randomID()
	if err != nil {
		http.Error(w, "could not queue bookshelf operation", http.StatusInternalServerError)
		return
	}
	targetBookID := source.ID
	if request.Mode == "clone" {
		targetBookID, err = randomID()
		if err != nil {
			http.Error(w, "could not queue bookshelf operation", http.StatusInternalServerError)
			return
		}
	}
	actor, _ := auth.UserFromContext(r.Context())
	operation := db.AdminBookOperation{
		ID: operationID, ActorUserID: actor.ID, Mode: request.Mode, BookID: source.ID,
		SourceOwnerUserID: request.SourceUserID, TargetOwnerUserID: request.TargetUserID,
		TargetBookID: targetBookID,
	}
	if err := s.db.QueueAdminBookOperation(r.Context(), operation); err != nil {
		if errors.Is(err, db.ErrAdminBookOperationUnavailable) {
			http.Error(w, "Only complete, idle books can be copied; check for another operation on this book.", http.StatusConflict)
			return
		}
		http.Error(w, "could not queue bookshelf operation", http.StatusInternalServerError)
		return
	}
	operation.Status, operation.Stage = "queued", "queued"
	w.WriteHeader(http.StatusAccepted)
	jsonOut(w, map[string]any{
		"operation":    operation,
		"source_title": source.Title,
		"target_email": target.Email,
	})
}

func (s *Server) getAdminBookOperation(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" || len(id) > 255 {
		http.Error(w, "invalid operation id", http.StatusBadRequest)
		return
	}
	operation, err := s.db.AdminBookOperation(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	jsonOut(w, operation)
}
