// Package migrations embeds the SQL migration files used by cmd/migrate to
// provision the Postgres schema for pricing-quote. The pricing-quote service
// itself remains in-memory by default; these migrations are only applied when
// an operator explicitly runs cmd/migrate against DB_URL.
package migrations

import _ "embed"

//go:embed 001_init.up.sql
var initUp string

//go:embed 001_init.down.sql
var initDown string

// upMigrations is the ordered set of up migrations keyed by filename.
var upMigrations = []migration{
	{name: "001_init.up.sql", ddl: initUp},
}

// downMigrations is the ordered set of down migrations keyed by filename,
// applied in reverse order of upMigrations.
var downMigrations = []migration{
	{name: "001_init.down.sql", ddl: initDown},
}

type migration struct {
	name string
	ddl  string
}