# Market data export v1

`cmd/market-data-export` is the side-effect-free market-data boundary for read-only consumers. It deliberately does not import `backend/data`, because that package initializes the Wails application's database, logger, settings, and model integrations.

The command reads exactly one `MarketDataRequest/v1` JSON object from stdin, writes exactly one `MarketDataEnvelope/v1` object to stdout, and writes diagnostics to stderr. It never reads the go-stock database or user/model credentials and does not create caches, logs, or configuration files.

Supported operations are `etf_universe`, `quotes`, `bars` (`1d` or `5m`), `etf_profile`, `events`, and `trading_calendar`. A malformed request exits non-zero. Per-symbol provider failures stay in the envelope's `errors` array so a partial batch remains usable.

The public web providers do not currently expose authoritative listing dates, tracking identities, fees, or a holiday calendar through this credential-free path. Those fields are explicitly `null`/missing and the calendar is marked `research_only`; consumers must fail closed for buy decisions until separately certified.
