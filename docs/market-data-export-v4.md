# Market data export v4

`MarketDataRequest/v4` turns the side-effect-free JSON CLI into a multi-market quote and daily-bar source that a polling dashboard can afford to call every few seconds. V1, V2 and V3 are unchanged: their requests, responses and provider order behave exactly as before.

What v4 adds:

- **Batched quotes.** All symbols of a request go to a provider in one HTTP request (50 symbols per URL) instead of one request per symbol.
- **Stocks, not only ETFs.** Mainland stocks, ETFs and Beijing listings, with or without an exchange suffix.
- **Hong Kong and US** quotes and daily bars, plus the indices a market strip needs.
- **Per-record units.** Currency and board lot travel with each record and are `null` when no source states them.
- **Lag and delay evidence.** Every quote carries its measured lag and what is known about the feed's delay.

## Request

```json
{
  "schema": "MarketDataRequest/v4",
  "request_id": "board-001",
  "as_of": "2026-11-16T10:00:00+08:00",
  "timezone": "Asia/Shanghai",
  "symbols": ["600519", "000001.SZ", "920799.BJ", "00700.HK", "AAPL.US", "000001.SH", "HSI.HK", ".IXIC.US"],
  "operations": [
    {"name": "quotes"},
    {"name": "bars", "timeframe": "1d", "limit": 240}
  ]
}
```

- Operations: `quotes` and `bars` only. The mainland ETF research operations (`etf_universe`, `etf_profile`, `events`, `trading_calendar`, `market_context`, `fund_flow`, `sentiment`, `bars_history`) keep their v2/v3 contracts and are rejected in v4.
- `bars` takes `timeframe` (`1d` for every market, `5m` for mainland symbols only) and `limit` (default 240, maximum 2000). `start`, `end` and `cursor` are rejected.
- At most 500 symbols per request. Duplicates are collapsed.
- The response is `MarketDataEnvelope/v4`, with the same envelope fields as v2/v3 (`status`, `results`, `errors`, `provider_health`).

## Symbols

| Market | Canonical form | Also accepted |
|---|---|---|
| Shanghai / Shenzhen / Beijing | `600519.SH`, `000001.SZ`, `920799.BJ` | `600519.SS`, `sh600519`, bare `600519` |
| Mainland index | `000001.SH`, `399001.SZ`, `899050.BJ` | `sh000001` |
| Hong Kong | `00700.HK` | `700.HK`, `hk00700` |
| Hong Kong index | `HSI.HK`, `HSTECH.HK` | |
| United States | `AAPL.US`, `BRK.B.US` | `aapl.us`, `BRK-B.US` |
| US index | `.IXIC.US`, `.NDX.US`, `.DJI.US`, `.INX.US` | |

A bare six-digit code takes its exchange from the prefix (`6`, `5`, `90` → SH; `0`, `1`, `2`, `3` → SZ; `4`, `8`, `92` → BJ) and always means a tradable security. An index therefore needs its suffix: `000001.SH` is the SSE Composite, bare `000001` is `000001.SZ`. A US index is marked by a leading dot so that it can never collide with a ticker.

Every record reports `market` (`CN`, `HK`, `US`), `exchange` and `instrument_type` (`security` or `index`).

## Quotes

Provider order is Tencent → Sina → EastMoney. Tencent and Sina are asked once for the whole batch; only the symbols a provider did not answer move on to the next one. EastMoney is a per-symbol third fallback for mainland symbols, is skipped for a symbol that both batch providers answered without, and stops after two consecutive transport failures so a dead endpoint cannot stall a poll.

Each provider request has an 8-second timeout. A quote whose provider timestamp is more than five minutes ahead of the local clock is rejected for that provider.

```json
{
  "symbol": "00700.HK", "market": "HK", "exchange": "HK", "instrument_type": "security", "name": "腾讯控股",
  "price": 386.6, "open": 393, "high": 395.2, "low": 385.6, "previous_close": 398.8,
  "change": -12.2, "change_ratio": -0.030592,
  "volume": 31493353, "amount": 12251162809.622,
  "bid1_price": null, "bid1_volume": null, "ask1_price": null, "ask1_volume": null,
  "unit": {"currency": "HKD", "price": "HKD_per_share", "volume": "share", "amount": "HKD", "lot_size": 100, "lot_size_source": "provider"},
  "source_timestamp": "2026-11-13T08:08:22Z", "source_timezone": "Asia/Hong_Kong", "timestamp_quality": "provider",
  "lag_seconds": 155582,
  "delay": {"level": "realtime", "minutes": null, "basis": "provider_declared"},
  "provider": "tencent_public_web", "provider_feed": "tencent:r_hk",
  "provenance": "go-stock:readonly-export-v4/quotes/tencent",
  "provider_chain": [{"provider": "tencent_public_web", "status": "selected"}]
}
```

`volume` is always in shares and `amount` in the record's currency. `change_ratio` is a fraction, not a percentage. `fallback_reason` is present when an earlier provider failed.

### Units

- `currency` comes from the provider when it states one (Tencent does for all three markets). Otherwise a venue rule applies only where it is unambiguous: `CNY` for mainland listings other than B shares, `USD` for US listings. Sina does not state the trading currency of a Hong Kong counter, so a Hong Kong record served by Sina has `currency: null`.
- `lot_size` is the board lot in shares. Hong Kong lots are per security and come from Tencent (`lot_size_source: "provider"`). A rule supplies 100 for Shanghai/Shenzhen main board, ChiNext and exchange-traded funds, and 1 for US securities (`"market_rule"`). It is `null` for the STAR Market and Beijing (minimum order plus one-share increments, not a lot multiple), for B shares, for anything unclassified, and for every Hong Kong record not served by Tencent.
- An index is quoted in `index_point`, has no currency or lot, and never exports `volume`: providers report it in lots, shares or turnover depending on the index. Index `amount` is exported for mainland and Hong Kong indices, where Tencent, Sina and EastMoney agree, and is `null` for US indices.

### Timestamp, lag and delay

- `timestamp_quality` is `provider` (second resolution), `provider_minute` (Sina's US trade time has no seconds) or `observed_at_fallback` (no usable provider time; `lag_seconds` is then `null`).
- `lag_seconds` is `observed_at − source_timestamp`. It is a fact about this record, and it is the number a consumer should gate on: a small lag proves the quote is live, a large lag during a session means the quote is stale or delayed.
- `delay.level` is `realtime`, `delayed` or `unknown`; `delay.minutes` is the size of a known delay or `null`; `delay.basis` says where the level comes from:

| `basis` | Meaning |
|---|---|
| `measured_lag` | This record's lag is at most 60 s (120 s for minute-resolution timestamps), so the quote is live now. Overrides any provider label. |
| `provider_declared` | The provider names or flags the feed. Not measured by this call. |
| `none` | Nothing is known; `level` is `unknown`. |

Declared levels, with the evidence behind each:

| Feed (`provider_feed`) | Declared | Evidence |
|---|---|---|
| `tencent:sh` / `sz` / `bj` | realtime | Tencent tags only delayed quotes; these carry no tag. |
| `tencent:r_hk` | realtime | `r_` is Tencent's realtime Hong Kong feed and carries no delay tag. |
| `tencent:us` securities | delayed, minutes unknown | Tencent's K-line payload embeds the same quote row with its first field set to `delay` for every US security checked (NASDAQ, NYSE, Arca). |
| `tencent:us` indices | realtime | No delay tag. |
| `sina:rt_hk` | realtime | `rt_` is Sina's realtime Hong Kong feed. The plain `hk` feed is a different, delayed one: after the 2026-11-13 close it still showed 387.000 at 16:07 while `rt_hk` showed the 386.600 closing-auction print at 16:08:22. |
| `sina:sh` / `sz` / `bj`, `sina:gb` | unknown | Sina does not label these feeds. |
| `eastmoney:push2` | unknown | Not labelled. |
| `eastmoney:push2delay` | delayed, minutes unknown | EastMoney redirected the quote request to its `push2delay` host. |

No declared level has been checked against a live session yet; see the smoke results below.

## Bars

`bars/<timeframe>` maps each symbol to one provider-continuous series. Units and provenance are stated once per series.

```json
{
  "00700.HK": {
    "symbol": "00700.HK", "market": "HK", "exchange": "HK", "instrument_type": "security",
    "timeframe": "1d", "adjustment": "none", "timezone": "Asia/Hong_Kong",
    "unit": {"currency": "HKD", "price": "HKD_per_share", "volume": "share", "amount": null, "lot_size": 100, "lot_size_source": "provider"},
    "provider": "tencent_public_web", "provenance": "go-stock:readonly-export-v4/bars/tencent",
    "provider_chain": [{"provider": "tencent_public_web", "status": "selected"}],
    "records": [
      {"trade_date": "2026-11-13", "timestamp": "2026-11-13T08:00:00Z", "open": 393, "close": 386.6, "high": 395.2, "low": 385.6, "volume": 31493353, "amount": null, "complete": true}
    ]
  }
}
```

| Market | Daily provider order | 5-minute |
|---|---|---|
| Mainland | Sina → EastMoney → Tencent | Sina → EastMoney |
| Hong Kong | Tencent → EastMoney | not available |
| United States | Tencent → EastMoney → Sina | not available |

- **Unadjusted prices.** Every series is `adjustment: "none"`, so bars line up with live quotes and with price levels a user typed in. Tencent's daily endpoint was compared with EastMoney's unadjusted series over 300 bars each for `00700.HK`, `AAPL.US` and `600519.SH`, across several ex-dividend dates, with no close differing.
- **Timestamps.** A daily bar is stamped at the market's nominal close in its own timezone (15:00 Shanghai, 16:00 Hong Kong, 16:00 New York), whatever the request `timezone` is. `trade_date` is the provider's session date.
- **Point in time.** A bar stamped after `as_of` is withheld, so a request with `as_of` = now returns completed sessions only; today's candle is built from `quotes`. If `as_of` is later than the local clock, the forming bar comes through with `complete: false`. Early-close days are not modelled.
- **US addressing.** Tencent and EastMoney address a US security by exchange. One batched Tencent quote resolves the exchange code for all US symbols of the request. If that fails, the three EastMoney US market numbers are probed in turn, and Sina, which needs no exchange but returns the whole history (about 1 MB for a long-listed stock), is the last resort.
- **No splicing.** As in v2/v3, one series comes from one provider. A consumer caching bars must rebuild a series when `provider` changes.

## Fields that are not available

| Field | Missing where | Why |
|---|---|---|
| `bid1_*`, `ask1_*` | US, all providers | Tencent's bid/ask slots tracked the extended-hours price rather than the regular session; Sina has none. |
| `bid1_price`, `ask1_price` | Hong Kong via Tencent | Tencent's slots repeat the last price. Sina supplies real ones. |
| `bid1_volume`, `ask1_volume` | Hong Kong and US | No provider supplies order-book sizes. |
| `unit.currency`, `unit.lot_size` | Hong Kong via Sina or EastMoney | Neither states them. |
| `unit.lot_size` | STAR Market, Beijing, B shares | No lot multiple applies or it is unverified. |
| `volume` | Every index | Unit differs by provider and index. |
| `amount` | US indices; daily bars from Tencent; daily mainland bars from Sina | The provider has no turnover there. |
| `delay.minutes` | Every delayed feed | Providers flag a delay without quantifying it. |
| Extended-hours prices | US | `price` is the regular-session price; pre- and post-market prints are not exported. |
| 5-minute bars | Hong Kong, US | Not implemented. |
| EastMoney quote fallback | Hong Kong, US | Volume units could not be verified, and a US lookup needs the exchange. |

A mainland quote keeps the lots-or-shares ambiguity out of `volume`: Tencent reports total volume in lots except on the STAR Market, where the same field holds shares. The multiplier is checked against turnover on each record, and `volume` is `null` if neither reading fits.

## Failure contract

Malformed requests exit non-zero, as before. Per-symbol failures stay in `errors` and never discard successful symbols.

| Code | Retryable | Meaning |
|---|---|---|
| `symbol_not_found` | no | Every provider asked answered, and none has the symbol. |
| `all_providers_failed` | yes | At least one provider failed to answer or returned an unusable record. |
| `unsupported_timeframe` | no | `5m` bars for a Hong Kong or US symbol. |

`provider_health` counts HTTP requests, so a healthy batch of 20 quotes is one attempt.

## Side effects

None, as in v1–v3. The command reads one JSON object from stdin, writes one envelope to stdout and diagnostics to stderr. It does not import the Wails runtime packages, read or write the go-stock database, settings or credentials, or create caches, logs or configuration files. The side-effect test runs the built binary in an empty directory for every schema including v4.

## Smoke results (2026-11-15)

Run against the live public endpoints at 03:28 UTC on Sunday 2026-11-15, from a network whose egress is outside mainland China. All three markets were closed.

### Batch latency

Twenty mainland symbols (15 stocks including one STAR listing, 5 ETFs), five alternating runs each, wall-clock time of the built binary including process start:

| Path | HTTP requests | Median | Range |
|---|---|---|---|
| v4 `quotes` | 1 | 1.48 s | 1.43–1.81 s |
| v3 `quotes` (per symbol) | 20 | 7.21 s | 7.08–7.43 s |

Most of the v4 time is connection setup: a TLS handshake to either quote host took 0.9–1.3 s from this network, and each CLI invocation pays it once.

Other paths, single runs: seven symbols across three markets plus four indices in one batch, 1.9 s; the same batch served by Sina with Tencent unreachable, 2.3 s; daily bars, 240 each for nine symbols across three markets, 7.8 s (bars are fetched one symbol at a time).

### Markets

| Market | Symbol | Quote | Daily bars | Source time of last print | Lag at observation |
|---|---|---|---|---|---|
| Mainland | `600519.SH` | 1435.26 CNY, Tencent | 240 bars to 2026-11-13, Sina | 2026-11-13 16:14:55 +08:00 | 43.2 h |
| Hong Kong | `00700.HK` | 386.6 HKD, lot 100, Tencent | 240 bars to 2026-11-13, Tencent | 2026-11-13 16:08:22 +08:00 | 43.3 h |
| United States | `AAPL.US` | 356.27 USD, Tencent | 240 bars to 2026-11-13, Tencent | 2026-11-13 16:00:01 −05:00 | 30.5 h |

Indices `000001.SH`, `399001.SZ`, `HSI.HK` and `.IXIC.US` returned in the same batch.

**These lags are the time since each market's last session, not feed delay.** With every market closed, a realtime feed and a delayed feed show the same final print, so this run cannot say whether Hong Kong or US quotes are delayed in session. What it does establish is in the declared-delay table above: Tencent flags its US security quotes as delayed, both providers' realtime-named Hong Kong feeds returned the closing-auction print that Sina's delayed feed lacked, and EastMoney serves this network from its delayed host.

To measure in-session lag, run this while the market in question is open and read `lag_seconds`; a live feed shows a few seconds and `delay.basis` switches to `measured_lag`:

```bash
printf '{"schema":"MarketDataRequest/v4","request_id":"lag","as_of":"%s","timezone":"Asia/Shanghai","symbols":["600519.SH","00700.HK","AAPL.US"],"operations":[{"name":"quotes"}]}' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  | go run ./cmd/market-data-export \
  | jq -r '.results.quotes[] | [.symbol, .provider_feed, .source_timestamp, .lag_seconds, .delay.level, .delay.basis] | @tsv'
```

### Fallback paths exercised live

| Scenario | Result |
|---|---|
| Tencent quotes unreachable | Sina served all seven symbols in one request. |
| Tencent and Sina quotes unreachable | EastMoney served the three mainland symbols, labelled `eastmoney:push2delay` / `delayed`; the Hong Kong symbol failed with `all_providers_failed`. |
| Tencent bars unreachable | EastMoney served `00700.HK`, `AAPL.US`, `HSI.HK` and `.IXIC.US`. |
| Tencent and EastMoney bars unreachable | Sina served `AAPL.US` from its full-history endpoint. |
| Unknown symbols (`999999.SH`, `ZZZZQ.US`) | `symbol_not_found`, with the other 14 symbols of the batch intact. |

### Observations outside v4

Found while testing; v1–v3 behaviour was left unchanged.

- From a network that EastMoney redirects to `push2delay`, the quote payload has `f124` at zero. V1 quotes, and the EastMoney fallback in v2/v3, read only `f124`, so they stamp such a quote with the local clock and `timestamp_quality: observed_at_fallback`. V4 reads `f86` and labels the feed delayed.
- The Tencent 5-minute fallback in v2/v3 requests `m5` from the daily `fqkline` endpoint, which answers `bad params`. It is the third fallback and normally unreached. V4 does not use Tencent for 5-minute bars.
- The v2/v3 Tencent daily fallback requests forward-adjusted prices while Sina and EastMoney return unadjusted ones.
