package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestAdminBootstrapClaimsStableUserID(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	admin, err := d.UpsertUser(ctx, "exe-user-1", "admin@example.org", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if admin.Role != "admin" || admin.Status != "active" {
		t.Fatalf("unexpected initial user: %#v", admin)
	}
	other, err := d.UpsertUser(ctx, "exe-user-2", "admin@example.org", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if other.Role != "user" {
		t.Fatalf("bootstrap email transferred admin role: %#v", other)
	}
	renamed, err := d.UpsertUser(ctx, "exe-user-1", "new-address@example.org", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Role != "admin" || renamed.Email != "new-address@example.org" {
		t.Fatalf("admin role did not stay bound to stable ID: %#v", renamed)
	}
	configured, err := d.UpsertUser(ctx, "configured-admin", "third@example.org", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if configured.Role != "admin" {
		t.Fatalf("configured ID did not get admin role: %#v", configured)
	}
}

func TestUserStatusAndBookShelfMetadata(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	if _, err := d.UpsertUser(ctx, "u1", "one@example.org", false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := d.UpsertUser(ctx, "u2", "two@example.org", false, false); err != nil {
		t.Fatal(err)
	}
	if err := d.SetUserStatus(ctx, "u2", "suspended"); err != nil {
		t.Fatal(err)
	}
	suspended, err := d.UpsertUser(ctx, "u2", "two@example.org", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if suspended.Status != "suspended" {
		t.Fatalf("status was not persisted across visits: %#v", suspended)
	}
	users, err := d.Users(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 {
		t.Fatalf("got %d users, want 2", len(users))
	}
}

func TestOpenMigratesOlderUsersTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.db")
	sqlDB, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = sqlDB.Exec(`CREATE TABLE users (
		id TEXT PRIMARY KEY, email TEXT NOT NULL, created_at TEXT NOT NULL, last_seen_at TEXT NOT NULL
	);
	INSERT INTO users VALUES('old-id','old@example.org','2020-01-01','2020-01-01')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	d, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	user, err := d.UpsertUser(context.Background(), "old-id", "old@example.org", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if user.Role != "user" || user.Status != "active" {
		t.Fatalf("legacy account defaults incorrect: %#v", user)
	}
}
