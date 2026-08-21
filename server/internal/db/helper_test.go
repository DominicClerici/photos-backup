package db

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

const (
	defaultAdminURL = "postgres://photobackup:photobackup@localhost:5432/photobackup?sslmode=disable"
	// Each package gets its own database. `go test ./...` runs packages
	// concurrently, and a shared one means one package truncates assets while
	// another is mid-test.
	testDBName = "photobackup_test_db"
)

// testStore returns a migrated, empty store backed by a real Postgres. These
// tests deliberately do not mock the database: the behavior under test is
// mostly the schema's (unique constraints, defaults, ordering), and a mock
// would only assert that the mock works.
func testStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()

	admin := defaultAdminURL
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		admin = v
	}
	ensureTestDatabase(t, ctx, admin)
	url := withDatabase(t, admin, testDBName)

	store, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("connect to test database: %v\n\nIs Postgres up? Run: docker compose up -d", err)
	}
	t.Cleanup(store.Close)

	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	// Every table by name rather than `cascade`, so a future table with a
	// foreign key here has to be added deliberately instead of silently wiped.
	// albums is named too because it is the one table here that does not hang
	// off assets: membership cascades away with the photos, but the album row
	// itself would survive into the next test and be counted by it.
	if _, err := store.pool.Exec(ctx, "truncate table assets, device_assets, jobs, albums, merge_groups, tags, vault_people, vault_secret cascade"); err != nil {
		t.Fatalf("truncate assets: %v", err)
	}
	return store
}

// withDatabase points a connection string at a different database, keeping the
// host and credentials, so TEST_DATABASE_URL selects a server rather than a
// single shared database.
func withDatabase(t *testing.T, rawURL, name string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse database url: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

func ensureTestDatabase(t *testing.T, ctx context.Context, adminURL string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to Postgres: %v\n\nIs Postgres up? Run: docker compose up -d", err)
	}
	defer conn.Close(ctx)

	var exists bool
	err = conn.QueryRow(ctx, "select exists (select 1 from pg_database where datname = $1)", testDBName).Scan(&exists)
	if err != nil {
		t.Fatalf("look up test database: %v", err)
	}
	if exists {
		return
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf("create database %s", pgx.Identifier{testDBName}.Sanitize())); err != nil {
		t.Fatalf("create test database: %v", err)
	}
}
