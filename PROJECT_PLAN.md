# Project Plan — Pricing / Quote

The Pricing / Quote service is the single source of truth for the price a user pays per unit of crypto at any moment. It sources real-time spot rates from `exchange-connectors`, applies a configurable spread plus fee markup, locks the rate for a ~30-second window in Redis, and emits audit events for every quote lifecycle transition. This plan decomposes the build into eight ordered stages that progress from foundational storage through ingestion, pricing logic, the quote lifecycle, hardening, and finally live streaming and operational readiness.

## Stage 1

### Goal

Establish the durable storage layer (OLTP schema for `quotes`, `fee_schedules`, `rate_sources`) and the Redis locked-quote store with TTL + atomic claim primitives.

### Tasks

- [x] Define Go migrations for `quotes` table (quote_id PK, from, to, amount, rate, spread_bps, fee, fee_currency, total, crypto_amount, user_tier, status, created_at, expires_at, claimed_at, claimed_by, source_venue) with indexes on `status` and `expires_at`.
- [x] Define migrations for `fee_schedules` (id, user_tier, asset, size_band_min, size_band_max, side, spread_bps, fee_type, fee_amount, fee_bps, enabled, updated_at).
- [x] Define migrations for `rate_sources` (id, name, priority, enabled, endpoint_ref, weight, created_at, updated_at).
- [x] Implement a `Store` interface wrapping the OLTP connection pool with CRUD methods for each table.
- [x] Implement a Redis `LockStore` with `SetNX(key, value, ttl)`, `Get(key)`, `Del(key)`, and a Lua-based atomic claim helper.
- [x] Wire configuration (`REDIS_URL`, `DATABASE_URL`) and a connection health check on startup.
- [ ] Add unit tests for `Store` and `LockStore` against a test Redis and a sqlite/postgres test container.

### Acceptance criteria

- `go test ./...` passes with migrations applied and the Redis lock primitives round-trip successfully.
- A locked quote key auto-expires after `RATE_LOCK_TTL_SECONDS`.
- Atomic claim Lua script returns the prior value and deletes the key in a single round-trip.

## Stage 2

### Goal

Ingest real-time spot rates from `exchange-connectors` via the pub/sub topic with a poll fallback, populating an in-process LRU cache with sub-second TTL.

### Tasks

- [x] Implement an LRU cache keyed by `from-to` pair storing `{bid, ask, mid, ts, source_venue}` with `L1_CACHE_SIZE` and `L1_CACHE_TTL_MS`.
- [x] Implement a pub/sub subscriber for `RATE_FEED_TOPIC` that updates the L1 cache on every message.
- [x] Implement an on-demand poll client against `EXCHANGE_CONNECTOR_URL` for one or more pairs, used as fallback when the cache is empty or stale beyond `MAX_STALE_AGE_MS`.
- [x] Define a `RateSource` selector that picks the best bid/ask across enabled `rate_sources` rows (priority then weight).
- [x] Expose a `SpotService.Get(pair) (Rate, error)` used by the quote engine.
- [x] Emit Prometheus metrics: `spot_cache_hits`, `spot_cache_misses`, `spot_poll_total`, `spot_pubsub_lag_ms`.
- [x] Add unit + integration tests for cache TTL, pub/sub update, and poll fallback.

### Acceptance criteria

- After a pub/sub message arrives, `SpotService.Get` returns the fresh rate within `L1_CACHE_TTL_MS`.
- When the cache is empty, the poll fallback populates it and the next `Get` returns the polled value.
- `MAX_STALE_AGE_MS` is enforced: a stale entry triggers a synchronous poll before returning.

## Stage 3

### Goal

Load `fee_schedules` into memory, hot-reload on config change, and implement spread + fee markup computation per `(user_tier, asset, size_band, side)`.

### Tasks

- [x] Implement a `FeeSchedule` in-memory index keyed by `(user_tier, asset, side, size_band)` with O(1) lookup.
- [ ] Implement a loader that fetches `fee_schedules` from `FEE_SCHEDULE_URL` on startup and on a 60s refresh tick.
- [x] Implement a hot-reload path triggered by a config-change signal (HTTP `POST /internal/v1/fee-schedules/reload`).
- [x] Implement `Pricer.Compute(pair, amount, user_tier, side) (rate, spread_bps, fee, total, crypto_amount)`.
- [x] Apply `DEFAULT_SPREAD_BPS` when no schedule matches.
- [x] Add structured logging and a Prometheus gauge for loaded schedule count.
- [x] Add unit tests covering tier/asset/size-band matching, default fallback, and hot-reload.

### Acceptance criteria

- A quote for a `tier_2` user buying BTC with amount in a configured size band returns the configured spread and fee.
- Hot-reload replaces the in-memory index without dropping in-flight requests.
- Unknown combinations fall back to `DEFAULT_SPREAD_BPS` with a warning log.

## Stage 4

### Goal

Implement the `POST /v1/quotes` (single + bulk) and `GET /v1/quotes/:id` endpoints with rate computation, Redis lock write, and durable `quotes` row persistence.

### Tasks

- [x] Define request/response DTOs matching the README contract (single and bulk `items` shapes).
- [x] Implement `POST /v1/quotes`: compute price, `quote_id = q_ULID`, write Redis lock with `EX = RATE_LOCK_TTL_SECONDS` + `NX`, persist `quotes` row, return `201`.
- [x] Implement bulk path honoring `BULK_QUOTE_MAX_ITEMS`; per-item errors do not abort the batch.
- [x] Implement `GET /v1/quotes/:id`: load row, return `404` if missing, `410` if expired, otherwise the full payload with `status`.
- [x] Add input validation (currency codes, amount > 0, supported tiers, side in {buy, sell}).
- [x] Add Prometheus histograms for quote latency (`quote_request_seconds`) and counters per status code.
- [x] Add unit + HTTP integration tests for single, bulk, validation errors, and lock write.

### Acceptance criteria

- A valid single quote returns `201` with all required fields and a Redis lock key present with the correct TTL.
- Bulk requests return one `quote_id` per item and respect `BULK_QUOTE_MAX_ITEMS`.
- `GET /v1/quotes/:id` returns `410` for an expired quote and `404` for an unknown one.
- p99 quote latency on the cache-hit path is under 30 ms in a local benchmark.

## Stage 5

### Goal

Implement quote refresh, expiry handling, and the internal atomic claim endpoint used by the Transaction Orchestrator at settlement.

### Tasks

- [x] Implement `POST /v1/quotes/:id/refresh`: cancel the old quote (status `canceled`, delete lock key), compute a new quote, persist and return it.
- [x] Implement `POST /internal/v1/quotes/:id/claim`: run the Lua claim script that checks `expires_at`, compares current spot vs. locked rate against `SLIPPAGE_TOLERANCE_BPS`, and `DEL`s the key atomically.
- [x] Return `409` with a reason (`expired`, `missing`, `slippage_exceeded`, `already_claimed`) on failure and emit `quote.slippage_rejected` where applicable.
- [x] On success, update the `quotes` row to `claimed` with `claimed_at` and `claimed_by`.
- [x] Add a background sweeper that marks un-claimed expired rows as `expired` for audit completeness.
- [x] Add unit + integration tests covering refresh, claim success, and each `409` reason.

### Acceptance criteria

- `refresh` produces a new `quote_id` and cancels the previous one.
- A claim within the window succeeds atomically; a second claim returns `409 already_claimed`.
- A claim after `expires_at` returns `409 expired`; a claim when the spot moved beyond tolerance returns `409 slippage_exceeded`.
- The sweeper converges `quotes.status` to `expired` for abandoned rows.

## Stage 6

### Goal

Add stale-rate protection, automatic source failover across venues, and graceful degradation to last-good value with a staleness flag.

### Tasks

- [x] Enforce `MAX_STALE_AGE_MS` in `SpotService.Get`: force a poll when the cached entry is older than the threshold.
- [x] Implement venue failover ordered by `rate_sources.priority`; on N errors from a venue, advance to the next.
- [x] When all sources fail, serve the last-good cached rate and tag the response with a `stale` flag in logs and a `quote_source_stale` metric.
- [x] Add circuit-breaker state per venue with configurable thresholds and half-open probing.
- [x] Add integration tests simulating venue errors and pub/sub outage.
- [x] Add runbook-style structured logs for failover transitions.

### Acceptance criteria

- A primary venue failure triggers failover to the secondary within one request.
- When every source is down, the service still returns a quote flagged `stale` instead of `503`.
- `MAX_STALE_AGE_MS` violations always force a synchronous poll before pricing.

## Stage 7

### Goal

Implement the WebSocket live-rates subscription (`WS /v1/rates/subscribe`) and integrate `fx-hedging` for non-USD fiat pairs plus hedge-cost markup.

### Tasks

- [x] Implement a WebSocket upgrader and a per-connection subscription registry keyed by client-requested pairs.
- [x] Fan out L1 cache updates to subscribed clients with `{pair, rate, ts, source}` frames.
- [x] Handle subscribe/unsubscribe messages and connection lifecycle (ping/pong, backpressure, graceful close).
- [x] Integrate `fx-hedging` client: for non-USD `from`, fetch the fiat→USD leg and combine with the crypto spot to produce the cross pair rate.
- [x] Apply hedge-cost markup from `fx-hedging` to the spread for pre-hedged tiers.
- [x] Add metrics for active WS connections, messages sent/dropped, and FX lookup latency.
- [x] Add unit + integration tests for WS fan-out and FX-cross pricing.

### Acceptance criteria

- A client subscribing to `USD-BTC` and `USD-ETH` receives a frame within the L1 cache update interval.
- A `EUR-BTC` quote is computed via `fx-hedging` EUR/USD leg combined with the USD/BTC spot.
- Hedge-cost markup is reflected in `spread_bps` for pre-hedged tiers.
- WS connections are cleaned up on disconnect without leaking goroutines.

## Stage 8

### Goal

Emit audit events for every quote lifecycle transition, add OpenTelemetry tracing, and round out tests and the production Docker image.

### Tasks

- [x] Emit `quote.issued`, `quote.refreshed`, `quote.expired`, `quote.claimed`, `quote.slippage_rejected` to `audit-event-log` asynchronously with at-least-once semantics and a bounded queue.
- [x] Add OpenTelemetry spans around `POST /v1/quotes`, `GET /v1/quotes/:id`, `refresh`, `claim`, and the spot lookup; export to `OTEL_EXPORTER_OTLP_ENDPOINT`.
- [x] Raise unit + integration test coverage; add a `make cover` target.
- [x] Add a multi-stage Dockerfile building a distroless final image with healthcheck.
- [x] Add a `/healthz` and `/readyz` endpoint reflecting Redis + OLTP + spot cache readiness.
- [x] Update CI to run `go vet`, `go test -race`, coverage upload, and the Docker build.
- [x] Finalize the Makefile with `build`, `test`, `cover`, `lint`, `docker` targets.

### Acceptance criteria

- Every quote lifecycle transition produces a corresponding audit event with `quote_id`, `user_tier`, and `source_venue`.
- Traces propagate a root span from HTTP entry through spot lookup and Redis lock write.
- `go test -race ./...` passes.
- `docker build` produces a working image that serves `/healthz` returning `200` when Redis is reachable.