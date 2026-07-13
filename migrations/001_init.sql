-- quotes: durable record of every issued quote.
CREATE TABLE IF NOT EXISTS quotes (
    quote_id       TEXT        NOT NULL PRIMARY KEY,
    from_ccy       TEXT        NOT NULL,
    to_ccy         TEXT        NOT NULL,
    amount         NUMERIC     NOT NULL,
    rate           NUMERIC     NOT NULL,
    spread_bps     INT         NOT NULL DEFAULT 0,
    fee            NUMERIC     NOT NULL DEFAULT 0,
    fee_currency   TEXT        NOT NULL,
    total          NUMERIC     NOT NULL,
    crypto_amount  NUMERIC     NOT NULL,
    user_tier      TEXT        NOT NULL,
    side           TEXT        NOT NULL,
    status         TEXT        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at     TIMESTAMPTZ NOT NULL,
    claimed_at     TIMESTAMPTZ,
    claimed_by     TEXT        NOT NULL DEFAULT '',
    source_venue   TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS quotes_status_idx ON quotes(status);
CREATE INDEX IF NOT EXISTS quotes_expires_at_idx ON quotes(expires_at);

-- fee_schedules: spread/fee model per (tier, asset, side, size band).
CREATE TABLE IF NOT EXISTS fee_schedules (
    id            BIGSERIAL    PRIMARY KEY,
    user_tier     TEXT         NOT NULL,
    asset         TEXT         NOT NULL,
    size_band_min NUMERIC      NOT NULL DEFAULT 0,
    size_band_max NUMERIC      NOT NULL DEFAULT 0,
    side          TEXT         NOT NULL,
    spread_bps    INT          NOT NULL DEFAULT 0,
    fee_type      TEXT         NOT NULL,
    fee_amount    NUMERIC      NOT NULL DEFAULT 0,
    fee_bps       INT          NOT NULL DEFAULT 0,
    enabled       BOOLEAN      NOT NULL DEFAULT true,
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS fee_schedules_lookup_idx ON fee_schedules(user_tier, asset, side, enabled);

-- rate_sources: registry of upstream venues.
CREATE TABLE IF NOT EXISTS rate_sources (
    id           BIGSERIAL    PRIMARY KEY,
    name         TEXT         NOT NULL UNIQUE,
    priority     INT          NOT NULL DEFAULT 0,
    enabled      BOOLEAN      NOT NULL DEFAULT true,
    endpoint_ref TEXT         NOT NULL DEFAULT '',
    weight       INT          NOT NULL DEFAULT 1,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT now()
);