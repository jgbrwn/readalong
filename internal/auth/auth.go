package auth

import (
	"context"
	"net/http"
	"net/mail"
	"strings"
	"unicode"

	"github.com/jgbrwn/readalong/internal/config"
)

type User struct {
	ID     string `json:"id"`
	Email  string `json:"email"`
	Role   string `json:"role"`
	Status string `json:"status,omitempty"`
}

type ctxKey int

const userKey ctxKey = 1

func UserFromContext(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(userKey).(User)
	return u, ok
}

func WithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, userKey, u)
}

func Middleware(cfg config.Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" {
			next.ServeHTTP(w, r)
			return
		}
		id := strings.TrimSpace(r.Header.Get("X-ExeDev-UserID"))
		email := strings.ToLower(strings.TrimSpace(r.Header.Get("X-ExeDev-Email")))
		if cfg.Env == "development" && !cfg.RequireExe && id == "" && email == "" {
			id, email = strings.TrimSpace(cfg.DevUserID), strings.ToLower(strings.TrimSpace(cfg.DevUserEmail))
		}
		if id == "" || email == "" || len(id) > 255 || !validEmail(email) || hasControl(id) {
			http.Error(w, http.StatusText(cfg.DenyStatus), cfg.DenyStatus)
			return
		}
		ctx := WithUser(r.Context(), User{ID: id, Email: email, Role: "user"})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func validEmail(value string) bool {
	a, err := mail.ParseAddress(value)
	return err == nil && a.Address == value && !hasControl(value)
}

func hasControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
