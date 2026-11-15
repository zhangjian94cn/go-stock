# MarketDataRequest/v4 recorded provider responses

Captured from the public web endpoints on 2026-11-15 03:11 UTC, a Sunday with
all three markets closed, so every payload is the final snapshot of the
2026-11-13 session. Files are stored byte-for-byte as received, with one
exception noted below.

| File | Request |
|---|---|
| `tencent_quotes.gbk` | `qt.gtimg.cn/q=sh600519,sz000001,bj920799,sh510300,sh688981,r_hk00700,r_hk00005,usAAPL,usBABA,usBRK.B,sh000001,sz399001,r_hkHSI,us.IXIC,sh999999` (GB18030; the unknown `sh999999` is absent from the response) |
| `sina_quotes.gbk` | `hq.sinajs.cn/list=` with the same instruments in Sina tickers (GB18030; `gb_brk.b` and `sh999999` come back empty) |
| `tencent_bars_<ticker>.json` | `web.ifzq.gtimg.cn/appstock/app/fqkline/get?param=<ticker>,day,,,5,` (unadjusted) |
| `eastmoney_bars_<secid>.json` | `push2his.eastmoney.com/api/qt/stock/kline/get?secid=<secid>&klt=101&fqt=0&lmt=5` |
| `sina_bars_sh600519.txt` | `CN_MarketDataService.getKLineData?symbol=sh600519&scale=240&datalen=5` |
| `sina_bars_us_aapl.txt` | `US_MinKService.getDailyK?symbol=aapl`. The endpoint returns the full history (10,075 rows, 943 KB); the recording is trimmed to the last 30 rows with the JSONP wrapper kept. |
| `eastmoney_quote_push2delay_1.600519.json` | `push2.eastmoney.com/api/qt/stock/get?secid=1.600519`, as served after the redirect to `push2delay.eastmoney.com` |
