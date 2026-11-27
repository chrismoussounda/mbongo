// Package migrate brings each component's tables up to date. Steps are only ever appended,
// so step n of a component means the same thing in every version of Mbongo.
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func Run(ctx context.Context, db *sql.DB, component string, steps []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_versions (
		component TEXT PRIMARY KEY, version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var done int
	err = tx.QueryRowContext(ctx, "SELECT version FROM schema_versions WHERE component = ?", component).Scan(&done)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if done > len(steps) {
		return fmt.Errorf("migrate: %s is at version %d, newer than this program (%d)", component, done, len(steps))
	}
	for i := done; i < len(steps); i++ {
		if _, err := tx.ExecContext(ctx, steps[i]); err != nil {
			return fmt.Errorf("migrate: %s step %d: %w", component, i+1, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_versions (component, version) VALUES (?, ?)
		ON CONFLICT (component) DO UPDATE SET version = excluded.version`, component, len(steps)); err != nil {
		return err
	}
	return tx.Commit()
}
