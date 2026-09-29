package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jgbrwn/readalong/internal/config"
)

func TestMiddlewareAcceptsAuthenticatedProxyIdentitiesWithoutEmailAllowlist(t *testing.T) {
	cfg := config.Config{Env: "production", RequireExe: true, DenyStatus: http.StatusNotFound}
	handler := Middleware(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := UserFromContext(r.Context())
		if !ok || u.ID != "stable-user-42" || u.Email != "reader@example.org" {
			t.Fatalf("unexpected context user: %#v, ok=%v", u, ok)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.Header.Set("X-ExeDev-UserID", "stable-user-42")
	req.Header.Set("X-ExeDev-Email", "Reader@example.org")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", res.Code)
	}
}

func TestMiddlewareRejectsMissingOrMalformedIdentity(t *testing.T) {
	cfg := config.Config{Env: "production", RequireExe: false, DenyStatus: http.StatusNotFound}
	handler := Middleware(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, tc := range []struct {
		name, id, email string
	}{
		{name: "missing"},
		{name: "partial", id: "user"},
		{name: "bad email", id: "user", email: "not-an-email"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
			if tc.id != "" {
				req.Header.Set("X-ExeDev-UserID", tc.id)
			}
			if tc.email != "" {
				req.Header.Set("X-ExeDev-Email", tc.email)
			}
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", res.Code)
			}
		})
	}
}

func TestDevelopmentIdentityFallbackIsDevelopmentOnly(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := UserFromContext(r.Context())
		w.Write([]byte(u.ID + "|" + u.Email))
	})
	dev := httptest.NewRecorder()
	Middleware(config.Config{
		Env: "development", RequireExe: false, DevUserID: "local", DevUserEmail: "local@example.org",
	}, h).ServeHTTP(dev, httptest.NewRequest(http.MethodGet, "/api/me", nil))
	if dev.Code != http.StatusOK || dev.Body.String() != "local|local@example.org" {
		t.Fatalf("development fallback = %d %q", dev.Code, dev.Body.String())
	}
	prod := httptest.NewRecorder()
	Middleware(config.Config{
		Env: "production", RequireExe: false, DevUserID: "local", DevUserEmail: "local@example.org",
		DenyStatus: http.StatusNotFound,
	}, h).ServeHTTP(prod, httptest.NewRequest(http.MethodGet, "/api/me", nil))
	if prod.Code != http.StatusNotFound {
		t.Fatalf("production fallback status = %d, want 404", prod.Code)
	}
}

func TestHealthDoesNotRequireIdentity(t *testing.T) {
	res := httptest.NewRecorder()
	Middleware(config.Config{Env: "production", DenyStatus: http.StatusNotFound},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })).
		ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", res.Code)
	}
}
