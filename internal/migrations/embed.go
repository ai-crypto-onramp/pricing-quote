// Package migrations embeds the SQL migration files used to provision the
// Postgres schema for pricing-quote. The pricing-quote server applies them
// automatically on startup when DB_URL (or DATABASE_URL) is set; cmd/migrate
// remains available for operators to run them out-of-band.
package migrations

import _ "embed"

//go:embed 001_init.up.sql
var initUp string

//go:embed 001_init.down.sql
var initDown string

// upMigrations is the ordered set of up migrations keyed by filename.
var upMigrations = []migration{
	{name: "001_init.up.sql", version: 1, ddl: initUp},
}

// downMigrations is the ordered set of down migrations keyed by filename,
// applied in reverse order of upMigrations.
var downMigrations = []migration{
	{name: "001_init.down.sql", version: 1, ddl: initDown},
}

type migration struct {
	name    string
	version int
	ddl     string
}
