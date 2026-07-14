package migrations

import (
	"context"
	"database/sql"
	"fmt"
)

// Up applies all embedded up migrations in filename order against db.
func Up(ctx context.Context, db *sql.DB) error {
	for _, m := range upMigrations {
		if _, err := db.ExecContext(ctx, m.ddl); err != nil {
			return fmt.Errorf("apply %s: %w", m.name, err)
		}
	}
	return nil
}

// Down applies all embedded down migrations in reverse filename order against
// db, dropping the schema created by Up.
func Down(ctx context.Context, db *sql.DB) error {
	for i := len(downMigrations) - 1; i >= 0; i-- {
		m := downMigrations[i]
		if _, err := db.ExecContext(ctx, m.ddl); err != nil {
			return fmt.Errorf("apply %s: %w", m.name, err)
		}
	}
	return nil
}