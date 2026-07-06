-- rate_sources: registry of upstream venues driving selection and failover.
CREATE TABLE IF NOT EXISTS rate_sources (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT NOT NULL UNIQUE,
    priority     INTEGER NOT NULL,
    enabled      INTEGER NOT NULL,
    endpoint_ref TEXT NOT NULL,
    weight       INTEGER NOT NULL,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_rate_sources_priority
    ON rate_sources(enabled, priority);