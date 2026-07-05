# Pricing / Quote

Real-time crypto on-ramp rate quotes with a ~30-second rate-lock window; sources spot rates, applies spreads, and marks up fees.

## Overview / Responsibilities

The Pricing / Quote service is the single source of truth for what a user pays per unit of crypto at any moment. It sits in the Fiat, Pricing & Liquidity subgraph and is called synchronously by the API Gateway / BFF for every quote request. Downstream, the Transaction Orchestrator consumes the **locked quote** at settlement to honor the rate the user was shown.

Core responsibilities:

- Source real-time spot rates from exchange / OTC feeds (via `exchange-connectors`).
- Apply a configurable spread plus fee markup, varying by user tier, asset, and order size.
- Lock the rate for a ~30-second window so the client can complete the buy at the displayed price.
- Honor the locked rate at settlement (orchestrator redeems the quote by ID).
- Guard against stale rates and excessive slippage between quote and settlement.
- Emit audit events for every quote issued, refreshed, expired, and claimed.

## Language & Tech Stack

- **Language:** Go (standard library + `net/http`; concurrency via goroutines/channels).
- **L1 cache:** In-process LRU cache for the latest spot rates per trading pair, refreshed by pub/sub updates and direct polls. Short TTL (sub-second) to bound staleness.
- **Locked quotes:** Redis is the system of record for the ~30s locked-quote window (TTL + atomic claim via `SETNX`/Lua). This survives restarts and is shared across horizontal replicas.
- **Rate updates:** Pub/sub fan-out from `exchange-connectors`; the service subscribes to the spot-rate topic and also polls as a fallback. WebSocket fan-out to clients for live rates.
- **Observability:** structured logs (JSON), Prometheus metrics, OpenTelemetry traces.

## System Requirements

1. **Real-time spot rate sourcing** — consume live spot rates from exchange and OTC feeds via `exchange-connectors`; support multiple venues per pair and pick best bid/ask.
2. **Configurable spread + fee markup** — spread (in bps) and flat/percentage fees configurable per user tier, asset, and order-size band; sourced from `fee_schedules`.
3. **~30-second rate lock with TTL** — each issued quote gets a `quote_id` and an `expires_at`; the rate is reserved in Redis with a TTL of `RATE_LOCK_TTL_SECONDS` (default 30).
4. **Quote expiry** — quotes past `expires_at` are rejected at claim time; clients must `refresh` to get a new quote.
5. **Quote honoring at settlement** — the Transaction Orchestrator claims the quote by ID atomically; if the claim succeeds, the locked rate is used for ledger posting.
6. **Slippage guard** — if the live spot at claim time has moved beyond a configurable tolerance vs. the locked rate, the claim is rejected and a refresh is required (protects treasury).
7. **Per-pair pricing (fiat → crypto)** — pricing is computed per `from` (fiat) / `to` (crypto) pair, including cross-currency pairs via `fx-hedging` for non-USD fiat.
8. **Bulk quote support** — accept batched quote requests (multiple pairs / sizes) in a single call; return a batched response with one `quote_id` per item.

## Non-Functional Requirements

| Requirement | Target |
|---|---|
| Quote latency (p99) | < 30 ms (cache hit path) |
| Stale-rate protection | max age `MAX_STALE_AGE_MS` (default 250 ms); force refresh on exceeding |
| Availability | 99.99% (quote path is on the critical transaction path) |
| Rate source failover | automatic failover across venues; degrade to last-good with staleness flag if all sources down |
| Throughput | ≥ 5,000 quote RPS per replica |
| Rate-lock correctness | atomic claim; no double-claim; TTL eviction guaranteed |

## Technical Specifications

### API Surface

- **REST** for request/response quote lifecycle (`POST /v1/quotes`, `GET /v1/quotes/:id`, `POST /v1/quotes/:id/refresh`).
- **WebSocket** for streaming live rates to clients (`WS /v1/rates/subscribe`); server pushes on every rate update for the subscribed pairs.

### Endpoints

#### `POST /v1/quotes`

Create a quote (single or bulk).

Request body:

```json
{
  "from": "USD",
  "to": "BTC",
  "amount": "500.00",
  "user_tier": "tier_2",
  "side": "buy"
}
```

Response `201`:

```json
{
  "quote_id": "q_01HABC...",
  "from": "USD",
  "to": "BTC",
  "amount": "500.00",
  "rate": "0.00001625",
  "spread_bps": 80,
  "fee": "2.50",
  "fee_currency": "USD",
  "total": "497.50",
  "crypto_amount": "0.00808125",
  "expires_at": "2026-07-06T12:00:30Z",
  "created_at": "2026-07-06T12:00:00Z"
}
```

Bulk: request body `{ "items": [ {...}, {...} ] }`; response `{ "items": [ {...}, {...} ] }` with per-item `quote_id`.

#### `GET /v1/quotes/:id`

Fetch a quote by ID (must not have expired). Returns the same shape as above with `status` (`open | claimed | expired | canceled`).

#### `POST /v1/quotes/:id/refresh`

Re-issue a fresh quote for the same `from`/`to`/`amount`/`user_tier` at the current spot. The old quote is canceled and a new `quote_id` + `expires_at` are returned.

#### `WS /v1/rates/subscribe`

Client sends `{ "pairs": ["USD-BTC", "USD-ETH"] }`. Server streams:

```json
{ "pair": "USD-BTC", "rate": "0.00001625", "ts": "2026-07-06T12:00:00.123Z", "source": "kraken" }
```

Internal claim endpoint (service-to-service, mTLS): `POST /internal/v1/quotes/:id/claim` — atomic claim by the Transaction Orchestrator at settlement; enforces slippage guard.

### Data Model

- **`rate_sources`** — registry of upstream venues (name, priority, enabled, endpoint ref, weight). Drives venue selection and failover order.
- **`quotes`** — durable record of every issued quote (quote_id, from, to, amount, rate, spread_bps, fee, fee_currency, total, crypto_amount, user_tier, status, created_at, expires_at, claimed_at, claimed_by, source_venue). Persisted to the service's OLTP store for audit/recon.
- **`locked_quotes`** (Redis) — short-lived key `lock:quote:{quote_id}` → JSON `{rate, from, to, amount, expires_at, source_venue, ts}` with TTL = `RATE_LOCK_TTL_SECONDS`. Claimed atomically; deleted on successful claim.
- **`fee_schedules`** — spread_bps + fee model per (user_tier, asset, size_band, side). Loaded into memory at startup and hot-reloaded on config change.

### Rate Lock Mechanism

1. On `POST /v1/quotes`, compute the rate from the freshest L1 spot + spread + fee.
2. Write `lock:quote:{quote_id}` to Redis with `EX = RATE_LOCK_TTL_SECONDS` and `NX` semantics (idempotent on the same ID).
3. Persist the durable `quotes` row.
4. On claim (`POST /internal/v1/quotes/:id/claim`):
   - Run a Lua script that `GET`s the lock key, checks `expires_at` against `now`, compares current spot vs. locked rate against the slippage tolerance, and `DEL`s the key — all atomically.
   - If the lock is missing, expired, or slippage is exceeded, return `409` with a reason; orchestrator must re-quote.
5. Redis TTL ensures abandoned quotes self-evict even if the claim never arrives.

### Integrations

- **`exchange-connectors`** (spot feed) — primary source of spot rates via pub/sub topic and on-demand poll.
- **`fx-hedging`** — for fiat → fiat leg on non-USD pairs (e.g. EUR → BTC requires EUR/USD); also exposes hedge cost markup that feeds the spread for pre-hedged tiers.
- **`transaction-orchestrator`** — consumes locked quotes via the internal claim endpoint at settlement.
- **`audit-event-log`** — async events for `quote.issued`, `quote.refreshed`, `quote.expired`, `quote.claimed`, `quote.slippage_rejected`.
- **`api-gateway`** — sole public caller for `POST /v1/quotes` and `GET /v1/quotes/:id`.

## Dependencies

| Dependency | Purpose |
|---|---|
| Redis | locked-quote store (TTL + atomic claim) |
| `exchange-connectors` | spot rate feed (pub/sub + poll fallback) |
| `fx-hedging` | cross-currency exposure + hedge cost input |
| `audit-event-log` | append-only audit trail consumer |

Build/runtime: Go 1.22+, Redis 7+, no other external runtime deps.

## Configuration

| Env var | Default | Description |
|---|---|---|
| `PORT` | `8080` | HTTP listen port |
| `REDIS_URL` | `redis://localhost:6379` | Redis endpoint for locked quotes |
| `RATE_LOCK_TTL_SECONDS` | `30` | Locked-quote TTL (the ~30s window) |
| `MAX_STALE_AGE_MS` | `250` | Max acceptable age of a cached spot rate before forced refresh |
| `DEFAULT_SPREAD_BPS` | `100` | Fallback spread (bps) when no fee-schedule match |
| `FEE_SCHEDULE_URL` | `http://config-svc/v1/fee-schedules` | Source for `fee_schedules` (hot-reloadable) |
| `FX_HEDGING_URL` | `http://fx-hedging:8080` | FX & Hedging service base URL |
| `EXCHANGE_CONNECTOR_URL` | `http://exchange-connectors:8080` | Exchange connectors poll endpoint |
| `RATE_FEED_TOPIC` | `spot.rates` | Pub/sub topic for spot-rate updates |
| `SLIPPAGE_TOLERANCE_BPS` | `150` | Max acceptable move vs. locked rate at claim |
| `BULK_QUOTE_MAX_ITEMS` | `25` | Max items per bulk quote request |
| `L1_CACHE_SIZE` | `4096` | In-process spot-rate LRU capacity (entries) |
| `L1_CACHE_TTL_MS` | `200` | In-process spot-rate TTL |
| `LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `http://otel-collector:4317` | OpenTelemetry OTLP gRPC endpoint |

## Local Development

```bash
# Build
go build ./...

# Run (requires Redis on $REDIS_URL)
go run ./cmd/pricing-quote

# Test
go test ./...

# Lint / vet
go vet ./...
```