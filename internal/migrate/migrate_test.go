package migrate_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/chrismoussounda/mbongo/internal/migrate"
)

func open(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func tables(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		"SELECT name FROM sqlite_master WHERE type = 'table' AND name <> 'schema_versions' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		names = append(names, n)
	}
	return strings.Join(names, ",")
}

func TestStepsRunOnceInOrder(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	steps := []string{"CREATE TABLE a (x)", "CREATE TABLE b (y)"}
	if err := migrate.Run(ctx, db, "test", steps); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Run(ctx, db, "test", steps); err != nil {
		t.Fatalf("second run: %v", err)
	}
	steps = append(steps, "ALTER TABLE a ADD COLUMN z")
	if err := migrate.Run(ctx, db, "test", steps); err != nil {
		t.Fatalf("new step: %v", err)
	}
	if got := tables(t, db); got != "a,b" {
		t.Fatalf("tables %s", got)
	}
}

func TestComponentsAreSeparate(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	if err := migrate.Run(ctx, db, "one", []string{"CREATE TABLE a (x)"}); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Run(ctx, db, "two", []string{"CREATE TABLE b (x)"}); err != nil {
		t.Fatal(err)
	}
	if got := tables(t, db); got != "a,b" {
		t.Fatalf("tables %s", got)
	}
}

func TestAFailingStepChangesNothing(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	if err := migrate.Run(ctx, db, "test", []string{"CREATE TABLE a (x)", "NOT SQL"}); err == nil {
		t.Fatal("no error for a bad step")
	}
	if got := tables(t, db); got != "" {
		t.Fatalf("tables left behind: %s", got)
	}
}

func TestANewerDatabaseIsRefused(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	if err := migrate.Run(ctx, db, "test", []string{"CREATE TABLE a (x)", "CREATE TABLE b (x)"}); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Run(ctx, db, "test", []string{"CREATE TABLE a (x)"}); err == nil {
		t.Fatal("an older program ran on a newer database")
	}
}
