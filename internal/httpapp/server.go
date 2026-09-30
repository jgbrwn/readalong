package httpapp

import (
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/jgbrwn/readalong/internal/auth"
	"github.com/jgbrwn/readalong/internal/catalog"
	"github.com/jgbrwn/readalong/internal/config"
	"github.com/jgbrwn/readalong/internal/db"
	"github.com/jgbrwn/readalong/internal/pipeline"
	"github.com/jgbrwn/readalong/webui"
)

type Server struct {
	cfg      config.Config
	db       *db.DB
	pipeline *pipeline.Service
	catalog  *catalog.Client
	mux      *http.ServeMux
}

func New(cfg config.Config, d *db.DB, workers ...*pipeline.Service) http.Handler {
	return NewWithCatalog(cfg, d, catalog.NewClient(), workers...)
}

func NewWithCatalog(cfg config.Config, d *db.DB, catalogClient *catalog.Client, workers ...*pipeline.Service) http.Handler {
	if catalogClient == nil {
		catalogClient = catalog.NewClient()
	}
	s := &Server{cfg: cfg, db: d, mux: http.NewServeMux()}
	s.catalog = catalogClient
	if len(workers) > 0 {
		s.pipeline = workers[0]
	}
	s.routes()
	return auth.Middleware(cfg, s.sameOrigin(s.provisionUsers(s.mux)))
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, map[string]any{"ok": true}) })
	s.mux.HandleFunc("GET /api/me", s.me)
	s.mux.HandleFunc("GET /api/books", s.books)
	s.mux.HandleFunc("POST /api/books", s.createBook)
	s.mux.HandleFunc("GET /api/discovery/pairs", s.searchPairs)
	s.mux.HandleFunc("POST /api/discovery/pairs/{id}/import", s.importPair)
	s.mux.HandleFunc("GET /api/books/{id}", s.getBook)
	s.mux.HandleFunc("DELETE /api/books/{id}", s.deleteBook)
	s.mux.HandleFunc("GET /api/books/{id}/reader", s.reader)
	s.mux.HandleFunc("GET /api/books/{id}/audio", s.audio)
	s.mux.HandleFunc("GET /api/books/{id}/events", s.events)
	s.mux.HandleFunc("PUT /api/books/{id}/progress", s.updateProgress)
	s.mux.HandleFunc("POST /api/books/{id}/retry", s.retryBook)
	s.mux.HandleFunc("POST /api/books/{id}/retranscribe", s.retranscribeBook)
	s.mux.HandleFunc("GET /api/admin/users", s.adminUsers)
	s.mux.HandleFunc("PUT /api/admin/users/{id}", s.updateAdminUser)
	sub, _ := fs.Sub(webui.Static, "static")
	fileServer := http.FileServer(http.FS(sub))
	s.mux.HandleFunc("GET /manifest.webmanifest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/manifest+json")
		fileServer.ServeHTTP(w, r)
	})
	s.mux.HandleFunc("GET /sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Service-Worker-Allowed", "/")
		fileServer.ServeHTTP(w, r)
	})
	s.mux.Handle("GET /", fileServer)
	s.mux.HandleFunc("GET /reader/{id}", func(w http.ResponseWriter, r *http.Request) {
		content, err := webui.Static.ReadFile("static/reader.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(content)
	})
}

func (s *Server) provisionUsers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" || !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		u, ok := auth.UserFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		user, err := s.db.UpsertUser(r.Context(), u.ID, u.Email,
			s.cfg.AdminUserIDs[u.ID], s.cfg.AdminBootstrapEmails[u.Email])
		if err != nil {
			http.Error(w, "account provisioning failed", http.StatusInternalServerError)
			return
		}
		if user.Status != "active" {
			http.Error(w, http.StatusText(s.cfg.DenyStatus), s.cfg.DenyStatus)
			return
		}
		u.Role, u.Status = user.Role, user.Status
		next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), u)))
	})
}

func (s *Server) sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") ||
			r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		origin, err := url.Parse(r.Header.Get("Origin"))
		if err != nil || origin.Host == "" || origin.User != nil ||
			(origin.Scheme != "https" && !(s.cfg.Env == "development" && origin.Scheme == "http")) {
			http.Error(w, "same-origin request required", http.StatusForbidden)
			return
		}
		expectedHost := r.Host
		if s.cfg.BaseURL != "" {
			base, parseErr := url.Parse(s.cfg.BaseURL)
			if parseErr != nil || base.Host == "" {
				http.Error(w, "same-origin request required", http.StatusForbidden)
				return
			}
			expectedHost = base.Host
		}
		if normalizeOriginHost(origin.Host, origin.Scheme) != normalizeOriginHost(expectedHost, origin.Scheme) {
			http.Error(w, "same-origin request required", http.StatusForbidden)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
			http.Error(w, "same-origin request required", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func normalizeOriginHost(host, scheme string) string {
	name, port, err := net.SplitHostPort(host)
	if err != nil {
		name = strings.Trim(host, "[]")
		port = ""
	}
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return strings.ToLower(net.JoinHostPort(name, port))
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	jsonOut(w, u)
}
func (s *Server) books(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(query)) > 100 || hasControlChars(query) {
		http.Error(w, "search query is too long or invalid", http.StatusBadRequest)
		return
	}
	b, err := s.db.SearchBooksForUser(r.Context(), u.ID, query)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	jsonOut(w, b)
}
func (s *Server) adminUsers(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	users, err := s.db.Users(r.Context())
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	jsonOut(w, users)
}

func (s *Server) updateAdminUser(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" || len(id) > 255 {
		http.Error(w, "invalid user id", http.StatusBadRequest)
		return
	}
	var req struct {
		Status string `json:"status"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.Status != "active" && req.Status != "suspended" {
		http.Error(w, "status must be active or suspended", http.StatusBadRequest)
		return
	}
	actor, _ := auth.UserFromContext(r.Context())
	if actor.ID == id && req.Status == "suspended" {
		http.Error(w, "cannot suspend your own account", http.StatusBadRequest)
		return
	}
	if err := s.db.SetUserStatus(r.Context(), id, req.Status); err != nil {
		http.NotFound(w, r)
		return
	}
	jsonOut(w, map[string]string{"status": "updated"})
}

func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	u, ok := auth.UserFromContext(r.Context())
	if !ok || u.Role != "admin" {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return false
	}
	return true
}

func jsonOut(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
