// Package store implements the durable OLTP storage layer (quotes,
// fee_schedules, rate_sources) and the Redis locked-quote store with TTL and
// atomic claim primitives.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate applies all embedded SQL migrations in lexical order against db.
func Migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		var applied string
		err := db.QueryRowContext(ctx, `SELECT name FROM schema_migrations WHERE name = ?`, e.Name()).Scan(&applied)
		if err == nil {
			continue
		}
		if err != sql.ErrNoRows {
			return fmt.Errorf("check migration %s: %w", e.Name(), err)
		}

		body, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return fmt.Errorf("read %s: %w", e.Name(), err)
		}
		if _, err := db.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", e.Name(), err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`,
			e.Name(), time.Now().UTC().Format(time.RFC3339Nano),
		); err != nil {
			return fmt.Errorf("record %s: %w", e.Name(), err)
		}
	}
	return nil
}

// Quote is the durable record of an issued quote.
type Quote struct {
	QuoteID      string
	FromCurrency string
	ToCurrency   string
	Amount       string
	Rate         string
	SpreadBPS    int
	Fee          string
	FeeCurrency  string
	Total        string
	CryptoAmount string
	UserTier     string
	Status       string
	CreatedAt    string
	ExpiresAt    string
	ClaimedAt    sql.NullString
	ClaimedBy    sql.NullString
	SourceVenue  string
}

// FeeSchedule is the spread + fee model for a (user_tier, asset, size_band, side).
type FeeSchedule struct {
	ID          int64
	UserTier    string
	Asset       string
	SizeBandMin string
	SizeBandMax string
	Side        string
	SpreadBPS   int
	FeeType     string
	FeeAmount   string
	FeeBPS      int
	Enabled     bool
	UpdatedAt   string
}

// RateSource is an upstream venue registry row.
type RateSource struct {
	ID          int64
	Name        string
	Priority    int
	Enabled     bool
	EndpointRef string
	Weight      int
	CreatedAt   string
	UpdatedAt   string
}

// Store wraps the OLTP connection pool with CRUD methods for each table.
type Store interface {
	// Quotes
	CreateQuote(ctx context.Context, q *Quote) error
	GetQuote(ctx context.Context, quoteID string) (*Quote, error)
	UpdateQuoteStatus(ctx context.Context, quoteID, status string, claimedAt, claimedBy sql.NullString) error
	ListQuotesByStatus(ctx context.Context, status string) ([]*Quote, error)
	ListQuotesExpiringBefore(ctx context.Context, expiresAt string) ([]*Quote, error)

	// Fee schedules
	UpsertFeeSchedule(ctx context.Context, s *FeeSchedule) (int64, error)
	ListFeeSchedules(ctx context.Context) ([]*FeeSchedule, error)
	DeleteFeeSchedule(ctx context.Context, id int64) error

	// Rate sources
	UpsertRateSource(ctx context.Context, r *RateSource) (int64, error)
	ListRateSources(ctx context.Context) ([]*RateSource, error)
	DeleteRateSource(ctx context.Context, id int64) error

	// Health
	Ping(ctx context.Context) error
	Close() error
}

// SQLiteStore is a Store backed by a database/sql connection (SQLite by default).
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore opens db (already connected) and applies migrations.
func NewSQLiteStore(ctx context.Context, db *sql.DB) (*SQLiteStore, error) {
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
		return nil, fmt.Errorf("set WAL: %w", err)
	}
	if err := Migrate(ctx, db); err != nil {
		return nil, err
	}
	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
func (s *SQLiteStore) Close() error                  { return s.db.Close() }

func (s *SQLiteStore) CreateQuote(ctx context.Context, q *Quote) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO quotes (
		quote_id, from_currency, to_currency, amount, rate, spread_bps, fee,
		fee_currency, total, crypto_amount, user_tier, status, created_at,
		expires_at, claimed_at, claimed_by, source_venue
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		q.QuoteID, q.FromCurrency, q.ToCurrency, q.Amount, q.Rate, q.SpreadBPS,
		q.Fee, q.FeeCurrency, q.Total, q.CryptoAmount, q.UserTier, q.Status,
		q.CreatedAt, q.ExpiresAt, q.ClaimedAt, q.ClaimedBy, q.SourceVenue,
	)
	if err != nil {
		return fmt.Errorf("create quote: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetQuote(ctx context.Context, quoteID string) (*Quote, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		quote_id, from_currency, to_currency, amount, rate, spread_bps, fee,
		fee_currency, total, crypto_amount, user_tier, status, created_at,
		expires_at, claimed_at, claimed_by, source_venue
	FROM quotes WHERE quote_id = ?`, quoteID)
	q := &Quote{}
	err := row.Scan(
		&q.QuoteID, &q.FromCurrency, &q.ToCurrency, &q.Amount, &q.Rate,
		&q.SpreadBPS, &q.Fee, &q.FeeCurrency, &q.Total, &q.CryptoAmount,
		&q.UserTier, &q.Status, &q.CreatedAt, &q.ExpiresAt, &q.ClaimedAt,
		&q.ClaimedBy, &q.SourceVenue,
	)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get quote: %w", err)
	}
	return q, nil
}

func (s *SQLiteStore) UpdateQuoteStatus(ctx context.Context, quoteID, status string, claimedAt, claimedBy sql.NullString) error {
	res, err := s.db.ExecContext(ctx, `UPDATE quotes SET status = ?, claimed_at = ?, claimed_by = ? WHERE quote_id = ?`,
		status, claimedAt, claimedBy, quoteID)
	if err != nil {
		return fmt.Errorf("update quote status: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) ListQuotesByStatus(ctx context.Context, status string) ([]*Quote, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT
		quote_id, from_currency, to_currency, amount, rate, spread_bps, fee,
		fee_currency, total, crypto_amount, user_tier, status, created_at,
		expires_at, claimed_at, claimed_by, source_venue
	FROM quotes WHERE status = ?`, status)
	if err != nil {
		return nil, fmt.Errorf("list quotes by status: %w", err)
	}
	defer rows.Close()
	return scanQuotes(rows)
}

func (s *SQLiteStore) ListQuotesExpiringBefore(ctx context.Context, expiresAt string) ([]*Quote, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT
		quote_id, from_currency, to_currency, amount, rate, spread_bps, fee,
		fee_currency, total, crypto_amount, user_tier, status, created_at,
		expires_at, claimed_at, claimed_by, source_venue
	FROM quotes WHERE expires_at <= ?`, expiresAt)
	if err != nil {
		return nil, fmt.Errorf("list quotes expiring before: %w", err)
	}
	defer rows.Close()
	return scanQuotes(rows)
}

func scanQuotes(rows *sql.Rows) ([]*Quote, error) {
	var out []*Quote
	for rows.Next() {
		q := &Quote{}
		if err := rows.Scan(
			&q.QuoteID, &q.FromCurrency, &q.ToCurrency, &q.Amount, &q.Rate,
			&q.SpreadBPS, &q.Fee, &q.FeeCurrency, &q.Total, &q.CryptoAmount,
			&q.UserTier, &q.Status, &q.CreatedAt, &q.ExpiresAt, &q.ClaimedAt,
			&q.ClaimedBy, &q.SourceVenue,
		); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) UpsertFeeSchedule(ctx context.Context, fs *FeeSchedule) (int64, error) {
	var id int64
	if fs.ID == 0 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO fee_schedules (
			user_tier, asset, size_band_min, size_band_max, side, spread_bps,
			fee_type, fee_amount, fee_bps, enabled, updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			fs.UserTier, fs.Asset, fs.SizeBandMin, fs.SizeBandMax, fs.Side,
			fs.SpreadBPS, fs.FeeType, fs.FeeAmount, fs.FeeBPS, fs.Enabled,
			fs.UpdatedAt,
		)
		if err != nil {
			return 0, fmt.Errorf("insert fee schedule: %w", err)
		}
		id, _ = res.LastInsertId()
	} else {
		_, err := s.db.ExecContext(ctx, `UPDATE fee_schedules SET
			user_tier = ?, asset = ?, size_band_min = ?, size_band_max = ?,
			side = ?, spread_bps = ?, fee_type = ?, fee_amount = ?, fee_bps = ?,
			enabled = ?, updated_at = ? WHERE id = ?`,
			fs.UserTier, fs.Asset, fs.SizeBandMin, fs.SizeBandMax, fs.Side,
			fs.SpreadBPS, fs.FeeType, fs.FeeAmount, fs.FeeBPS, fs.Enabled,
			fs.UpdatedAt, fs.ID,
		)
		if err != nil {
			return 0, fmt.Errorf("update fee schedule: %w", err)
		}
		id = fs.ID
	}
	return id, nil
}

func (s *SQLiteStore) ListFeeSchedules(ctx context.Context) ([]*FeeSchedule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT
		id, user_tier, asset, size_band_min, size_band_max, side, spread_bps,
		fee_type, fee_amount, fee_bps, enabled, updated_at
	FROM fee_schedules ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list fee schedules: %w", err)
	}
	defer rows.Close()
	var out []*FeeSchedule
	for rows.Next() {
		fs := &FeeSchedule{}
		var enabled int
		if err := rows.Scan(
			&fs.ID, &fs.UserTier, &fs.Asset, &fs.SizeBandMin, &fs.SizeBandMax,
			&fs.Side, &fs.SpreadBPS, &fs.FeeType, &fs.FeeAmount, &fs.FeeBPS,
			&enabled, &fs.UpdatedAt,
		); err != nil {
			return nil, err
		}
		fs.Enabled = enabled != 0
		out = append(out, fs)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) DeleteFeeSchedule(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM fee_schedules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete fee schedule: %w", err)
	}
	return nil
}

func (s *SQLiteStore) UpsertRateSource(ctx context.Context, r *RateSource) (int64, error) {
	var id int64
	if r.ID == 0 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO rate_sources (
			name, priority, enabled, endpoint_ref, weight, created_at, updated_at
		) VALUES (?,?,?,?,?,?,?)`,
			r.Name, r.Priority, r.Enabled, r.EndpointRef, r.Weight,
			r.CreatedAt, r.UpdatedAt,
		)
		if err != nil {
			return 0, fmt.Errorf("insert rate source: %w", err)
		}
		id, _ = res.LastInsertId()
	} else {
		_, err := s.db.ExecContext(ctx, `UPDATE rate_sources SET
			name = ?, priority = ?, enabled = ?, endpoint_ref = ?, weight = ?,
			updated_at = ? WHERE id = ?`,
			r.Name, r.Priority, r.Enabled, r.EndpointRef, r.Weight,
			r.UpdatedAt, r.ID,
		)
		if err != nil {
			return 0, fmt.Errorf("update rate source: %w", err)
		}
		id = r.ID
	}
	return id, nil
}

func (s *SQLiteStore) ListRateSources(ctx context.Context) ([]*RateSource, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT
		id, name, priority, enabled, endpoint_ref, weight, created_at, updated_at
	FROM rate_sources ORDER BY priority, id`)
	if err != nil {
		return nil, fmt.Errorf("list rate sources: %w", err)
	}
	defer rows.Close()
	var out []*RateSource
	for rows.Next() {
		r := &RateSource{}
		var enabled int
		if err := rows.Scan(
			&r.ID, &r.Name, &r.Priority, &enabled, &r.EndpointRef, &r.Weight,
			&r.CreatedAt, &r.UpdatedAt,
		); err != nil {
			return nil, err
		}
		r.Enabled = enabled != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) DeleteRateSource(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM rate_sources WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete rate source: %w", err)
	}
	return nil
}

// ErrNotFound is returned when a row lookup returns no result.
var ErrNotFound = fmt.Errorf("not found")