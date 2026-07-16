package migrations

import (
	"context"
	"database/sql"
	"fmt"
)

// Up applies all pending embedded up migrations in filename order against db.
// It is idempotent: an internal schema_migrations table tracks applied
// versions, so re-runs after make reset-db are a no-op.
func Up(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	for _, m := range upMigrations {
		var ok int
		err := db.QueryRowContext(ctx,
			`SELECT 1 FROM schema_migrations WHERE version = $1`, m.version).Scan(&ok)
		switch err {
		case nil:
			continue
		case sql.ErrNoRows:
		default:
			return fmt.Errorf("check migration %d: %w", m.version, err)
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin tx for migration %d: %w", m.version, err)
		}
		if _, err := tx.ExecContext(ctx, m.ddl); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1)`, m.version); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %d: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", m.version, err)
		}
	}
	return nil
}

// Down reverts the latest applied migration (the highest version recorded in
// schema_migrations). It is idempotent: if no migrations are recorded as
// applied it is a no-op. Repeated calls step back one migration at a time.
func Down(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	var version int
	err := db.QueryRowContext(ctx,
		`SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&version)
	switch err {
	case sql.ErrNoRows:
		return nil
	case nil:
	default:
		return fmt.Errorf("find latest migration: %w", err)
	}

	var down string
	for _, m := range downMigrations {
		if m.version == version {
			down = m.ddl
			break
		}
	}
	if down == "" {
		return fmt.Errorf("no down script for migration %d", version)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx for migration %d down: %w", version, err)
	}
	if _, err := tx.ExecContext(ctx, down); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("revert migration %d down: %w", version, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM schema_migrations WHERE version = $1`, version); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete migration %d record: %w", version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %d down: %w", version, err)
	}
	return nil
}
