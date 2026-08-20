# Market data export v3

`MarketDataRequest/v3` extends the side-effect-free JSON CLI with paged historical bars for point-in-time research. V1 and V2 remain unchanged.

## Request

Use `bars_history` with a single timeframe (`1d` or `5m`), an inclusive `start`/`end` date and an optional RFC3339 `cursor`.

```json
{
  "schema": "MarketDataRequest/v3",
  "request_id": "backtest-001",
  "as_of": "2026-08-20T15:30:00+08:00",
  "timezone": "Asia/Shanghai",
  "symbols": ["510300.SH"],
  "operations": [
    {
      "name": "bars_history",
      "timeframe": "5m",
      "start": "2026-02-20",
      "end": "2026-08-20",
      "limit": 10000
    }
  ]
}
```

Each symbol returns one provider-continuous page under `bars_history/<timeframe>`:

```json
{
  "510300.SH": {
    "records": [],
    "start": "2026-02-20",
    "end": "2026-08-20",
    "provider": "eastmoney",
    "next_cursor": null,
    "complete": true
  }
}
```

## Safety and continuity

- The CLI continues to read JSON from stdin, write JSON to stdout and diagnostics to stderr.
- It does not initialize SQLite, write caches or logs, load brokerage credentials, or read the Wails application's runtime database.
- A history page is built from one provider only. Consumers must not concatenate pages from different providers into a structure window.
- Partial symbol failures remain structured errors and do not discard successful symbols.
- `complete=false` or a non-null `next_cursor` means callers must request another page before claiming full historical coverage.
- The decision desk independently checks bar count, completed trading-day coverage, provider continuity and non-finite or duplicate records before certifying a backtest.
