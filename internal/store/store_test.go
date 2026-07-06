package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newTestSQLiteStore(t *testing.T) *SQLiteStore {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:test?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := NewSQLiteStore(ctx, db)
	if err != nil {
		t.Fatalf("new sqlite store: %v", err)
	}
	return s
}

func TestMigrateAppliesAllTables(t *testing.T) {
	ctx := context.Background()
	s := newTestSQLiteStore(t)

	for _, table := range []string{"quotes", "fee_schedules", "rate_sources", "schema_migrations"} {
		var name string
		err := s.db.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`,
			table,
		).Scan(&name)
		if err != nil {
			t.Fatalf("table %s not created: %v", table, err)
		}
	}

	// Indexes on quotes(status) and quotes(expires_at) must exist.
	for _, idx := range []string{"idx_quotes_status", "idx_quotes_expires_at"} {
		var name string
		err := s.db.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`,
			idx,
		).Scan(&name)
		if err != nil {
			t.Fatalf("index %s not created: %v", idx, err)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestSQLiteStore(t)
	if err := Migrate(ctx, s.db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestQuoteCRUD(t *testing.T) {
	ctx := context.Background()
	s := newTestSQLiteStore(t)

	q := &Quote{
		QuoteID: "q_test_1", FromCurrency: "USD", ToCurrency: "BTC",
		Amount: "500.00", Rate: "0.00001625", SpreadBPS: 80, Fee: "2.50",
		FeeCurrency: "USD", Total: "497.50", CryptoAmount: "0.00808125",
		UserTier: "tier_2", Status: "open", CreatedAt: "2026-07-06T12:00:00Z",
		ExpiresAt: "2026-07-06T12:00:30Z", SourceVenue: "kraken",
	}
	if err := s.CreateQuote(ctx, q); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetQuote(ctx, q.QuoteID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.QuoteID != q.QuoteID || got.Status != "open" || got.SourceVenue != "kraken" {
		t.Fatalf("got %+v, want %+v", got, q)
	}

	if _, err := s.GetQuote(ctx, "missing"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	if err := s.UpdateQuoteStatus(ctx, q.QuoteID, "claimed",
		sql.NullString{String: "2026-07-06T12:00:20Z", Valid: true},
		sql.NullString{String: "orchestrator", Valid: true},
	); err != nil {
		t.Fatalf("update status: %v", err)
	}
	got, _ = s.GetQuote(ctx, q.QuoteID)
	if got.Status != "claimed" || !got.ClaimedBy.Valid || got.ClaimedBy.String != "orchestrator" {
		t.Fatalf("got %+v", got)
	}

	if err := s.UpdateQuoteStatus(ctx, "missing", "claimed", sql.NullString{}, sql.NullString{}); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestListQuotesByStatusAndExpiry(t *testing.T) {
	ctx := context.Background()
	s := newTestSQLiteStore(t)
	now := time.Now().UTC()
	base := now.Add(-time.Minute).Format(time.RFC3339Nano)
	future := now.Add(time.Hour).Format(time.RFC3339Nano)
	for i, status := range []string{"open", "open", "claimed", "expired"} {
		q := &Quote{
			QuoteID: "q_" + status + "_" + string(rune('a'+i)),
			FromCurrency: "USD", ToCurrency: "BTC", Amount: "1", Rate: "1",
			SpreadBPS: 1, Fee: "0", FeeCurrency: "USD", Total: "1",
			CryptoAmount: "1", UserTier: "tier_1", Status: status,
			CreatedAt: base, ExpiresAt: future, SourceVenue: "kraken",
		}
		if err := s.CreateQuote(ctx, q); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	open, err := s.ListQuotesByStatus(ctx, "open")
	if err != nil {
		t.Fatalf("list by status: %v", err)
	}
	if len(open) != 2 {
		t.Fatalf("expected 2 open, got %d", len(open))
	}

	// Add a row that already expired.
	q := &Quote{
		QuoteID: "q_expired_past", FromCurrency: "USD", ToCurrency: "BTC",
		Amount: "1", Rate: "1", SpreadBPS: 1, Fee: "0", FeeCurrency: "USD",
		Total: "1", CryptoAmount: "1", UserTier: "tier_1", Status: "open",
		CreatedAt: base, ExpiresAt: now.Add(-time.Second).Format(time.RFC3339Nano),
		SourceVenue: "kraken",
	}
	if err := s.CreateQuote(ctx, q); err != nil {
		t.Fatalf("create: %v", err)
	}
	expiring, err := s.ListQuotesExpiringBefore(ctx, now.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatalf("list expiring: %v", err)
	}
	if len(expiring) != 1 {
		t.Fatalf("expected 1 expiring, got %d", len(expiring))
	}
}

func TestFeeScheduleCRUD(t *testing.T) {
	ctx := context.Background()
	s := newTestSQLiteStore(t)
	fs := &FeeSchedule{
		UserTier: "tier_2", Asset: "BTC", SizeBandMin: "0", SizeBandMax: "1000",
		Side: "buy", SpreadBPS: 80, FeeType: "flat", FeeAmount: "2.50",
		FeeBPS: 0, Enabled: true, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	id, err := s.UpsertFeeSchedule(ctx, fs)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if id == 0 {
		t.Fatalf("expected non-zero id")
	}

	list, err := s.ListFeeSchedules(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || !list[0].Enabled || list[0].SpreadBPS != 80 {
		t.Fatalf("got %+v", list)
	}

	fs.ID = id
	fs.SpreadBPS = 100
	if _, err := s.UpsertFeeSchedule(ctx, fs); err != nil {
		t.Fatalf("update: %v", err)
	}
	list, _ = s.ListFeeSchedules(ctx)
	if len(list) != 1 || list[0].SpreadBPS != 100 {
		t.Fatalf("update did not take effect: %+v", list)
	}

	if err := s.DeleteFeeSchedule(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	list, _ = s.ListFeeSchedules(ctx)
	if len(list) != 0 {
		t.Fatalf("expected empty, got %d", len(list))
	}
}

func TestRateSourceCRUD(t *testing.T) {
	ctx := context.Background()
	s := newTestSQLiteStore(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	r := &RateSource{
		Name: "kraken", Priority: 1, Enabled: true, EndpointRef: "wss://kraken",
		Weight: 50, CreatedAt: now, UpdatedAt: now,
	}
	id, err := s.UpsertRateSource(ctx, r)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if id == 0 {
		t.Fatalf("expected non-zero id")
	}

	list, err := s.ListRateSources(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Name != "kraken" || !list[0].Enabled {
		t.Fatalf("got %+v", list)
	}

	r.ID = id
	r.Priority = 2
	if _, err := s.UpsertRateSource(ctx, r); err != nil {
		t.Fatalf("update: %v", err)
	}
	list, _ = s.ListRateSources(ctx)
	if len(list) != 1 || list[0].Priority != 2 {
		t.Fatalf("update did not take effect: %+v", list)
	}

	if err := s.DeleteRateSource(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	list, _ = s.ListRateSources(ctx)
	if len(list) != 0 {
		t.Fatalf("expected empty, got %d", len(list))
	}
}

func TestSQLiteStorePing(t *testing.T) {
	ctx := context.Background()
	s := newTestSQLiteStore(t)
	if err := s.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
}