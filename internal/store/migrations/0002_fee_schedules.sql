-- fee_schedules: spread_bps + fee model per (user_tier, asset, size_band, side).
CREATE TABLE IF NOT EXISTS fee_schedules (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    user_tier     TEXT NOT NULL,
    asset         TEXT NOT NULL,
    size_band_min TEXT NOT NULL,
    size_band_max TEXT NOT NULL,
    side          TEXT NOT NULL,
    spread_bps    INTEGER NOT NULL,
    fee_type      TEXT NOT NULL,
    fee_amount    TEXT NOT NULL,
    fee_bps       INTEGER NOT NULL,
    enabled       INTEGER NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_fee_schedules_lookup
    ON fee_schedules(user_tier, asset, side, enabled);