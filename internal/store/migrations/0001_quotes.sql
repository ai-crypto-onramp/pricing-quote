-- quotes: durable record of every issued quote.
CREATE TABLE IF NOT EXISTS quotes (
    quote_id      TEXT PRIMARY KEY,
    from_currency TEXT NOT NULL,
    to_currency   TEXT NOT NULL,
    amount        TEXT NOT NULL,
    rate          TEXT NOT NULL,
    spread_bps    INTEGER NOT NULL,
    fee           TEXT NOT NULL,
    fee_currency  TEXT NOT NULL,
    total         TEXT NOT NULL,
    crypto_amount TEXT NOT NULL,
    user_tier     TEXT NOT NULL,
    status        TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    expires_at    TEXT NOT NULL,
    claimed_at    TEXT,
    claimed_by    TEXT,
    source_venue  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_quotes_status     ON quotes(status);
CREATE INDEX IF NOT EXISTS idx_quotes_expires_at ON quotes(expires_at);