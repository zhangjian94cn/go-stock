# Market data export v2

`cmd/market-data-export` accepts both `MarketDataRequest/v1` and `MarketDataRequest/v2`. The v1 response remains `MarketDataEnvelope/v1`; v2 responses use `MarketDataEnvelope/v2` and add request-scoped `provider_health`, record-level `provider_chain`, and explicit `fallback_reason` evidence.

The command remains a side-effect-free, read-only boundary. It reads exactly one JSON object from stdin, writes exactly one JSON envelope to stdout, sends diagnostics only to stderr, and does not import the Wails runtime packages that initialize SQLite, logging, settings, credentials, or model integrations.

## Provider policy

- Quotes: Tencent → Sina → EastMoney.
- Daily bars: Sina → EastMoney → Tencent.
- 5-minute bars: Sina → EastMoney → Tencent.
- ETF profile: EastMoney fund HTML parser → EastMoney fund suggestion fallback.
- Announcements: EastMoney public announcement endpoint.

Each bar window comes from one selected provider. A failed provider is never spliced together with a succeeding provider inside one structure window. Consumers can detect a source change through `provider`, `provider_chain`, and the source timestamp/provenance on every record and must rebuild cached structures when the selected provider changes.

## v2-only operations

`market_context`, `fund_flow`, and `sentiment` are v2-only. The first implementation uses transparent deterministic quote-derived proxies and marks them `research_only` or `is_proxy`; it does not represent the turnover-direction proxy as institutional fund-flow truth.

ETF profiles expose tracking identity, establishment date, scale, and fees when the public page contains them. A missing listing date is kept as `null`; consumers may prove the 120-completed-session history requirement from a complete daily-bar series but must not manufacture a listing date.

The credential-free trading calendar remains a weekday fallback with `research_only: true`. It cannot unlock production `BUY_ZONE` certification by itself.

## Failure contract

Malformed requests and unsupported schemas/operations exit non-zero. Per-symbol provider exhaustion appears as a structured partial error, allowing healthy symbols to remain usable. Duplicate or unordered bars, future/invalid timestamps, invalid OHLC relationships, non-finite values, and unknown units fail closed.
