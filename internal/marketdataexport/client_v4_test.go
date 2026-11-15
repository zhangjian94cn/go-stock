package marketdataexport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The v4 tests replay provider responses recorded on 2026-11-15 (see
// testdata/v4/README.md). recordedAt is the capture time.
var recordedAt = time.Date(2026, 11, 15, 3, 11, 23, 0, time.UTC)

func fixtureV4(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "v4", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

type serverV4 struct {
	t *testing.T
	// fail lists route names that answer HTTP 502.
	fail map[string]bool

	mu      sync.Mutex
	queries map[string][]string
	server  *httptest.Server
}

// newServerV4 serves the recorded payloads on one route per provider
// endpoint and records the query each route received.
func newServerV4(t *testing.T, fail ...string) *serverV4 {
	t.Helper()
	s := &serverV4{t: t, fail: map[string]bool{}, queries: map[string][]string{}}
	for _, route := range fail {
		s.fail[route] = true
	}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.server.Close)
	return s
}

func (s *serverV4) handle(w http.ResponseWriter, r *http.Request) {
	route := strings.TrimPrefix(r.URL.Path, "/")
	query := r.URL.Query()
	key := map[string]string{
		"tencent-quote": "q", "sina-quote": "list", "tencent-bars": "param",
		"eastmoney-bars": "secid", "sina-bars": "symbol", "sina-us-bars": "symbol", "eastmoney-quote": "secid",
	}[route]
	value := query.Get(key)
	s.mu.Lock()
	s.queries[route] = append(s.queries[route], value)
	s.mu.Unlock()
	if s.fail[route] {
		http.Error(w, "unavailable", http.StatusBadGateway)
		return
	}
	file := ""
	switch route {
	case "tencent-quote":
		file = "tencent_quotes.gbk"
	case "sina-quote":
		file = "sina_quotes.gbk"
	case "tencent-bars":
		file = "tencent_bars_" + strings.Split(value, ",")[0] + ".json"
	case "eastmoney-bars":
		file = "eastmoney_bars_" + value + ".json"
	case "sina-bars":
		file = "sina_bars_" + value + ".txt"
	case "sina-us-bars":
		file = "sina_bars_us_" + value + ".txt"
	case "eastmoney-quote":
		file = "eastmoney_quote_push2delay_" + value + ".json"
	}
	data, err := os.ReadFile(filepath.Join("testdata", "v4", file))
	if err != nil {
		if route == "eastmoney-bars" || route == "eastmoney-quote" {
			_, _ = w.Write([]byte(`{"rc":100,"data":null}`))
			return
		}
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(data)
}

func (s *serverV4) calls(route string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.queries[route]...)
}

func (s *serverV4) client(now time.Time) *Client {
	base := s.server.URL
	return &Client{
		HTTP: s.server.Client(),
		Endpoints: Endpoints{
			QuoteTencent: base + "/tencent-quote?q=",
			QuoteSina:    base + "/sina-quote?list=",
			Quote:        base + "/eastmoney-quote",
			BarsTencent:  base + "/tencent-bars",
			Bars:         base + "/eastmoney-bars",
			BarsSina:     base + "/sina-bars",
			BarsSinaUS:   base + "/sina-us-bars",
		},
		Now: func() time.Time { return now },
	}
}

func requestV4(asOf string, symbols []string, operations ...Operation) Request {
	return Request{Schema: RequestSchemaV4, RequestID: "v4", AsOf: asOf, Timezone: "Asia/Shanghai", Symbols: symbols, Operations: operations}
}

func quotesBySymbol(t *testing.T, envelope Envelope) map[string]map[string]any {
	t.Helper()
	var records []map[string]any
	if err := json.Unmarshal(envelope.Results["quotes"], &records); err != nil {
		t.Fatalf("decode quotes: %v (%s)", err, envelope.Results["quotes"])
	}
	result := map[string]map[string]any{}
	for _, record := range records {
		result[record["symbol"].(string)] = record
	}
	return result
}

func seriesBySymbol(t *testing.T, envelope Envelope, key string) map[string]map[string]any {
	t.Helper()
	var result map[string]map[string]any
	if err := json.Unmarshal(envelope.Results[key], &result); err != nil {
		t.Fatalf("decode %s: %v (%s)", key, err, envelope.Results[key])
	}
	return result
}

func expectFields(t *testing.T, label string, record map[string]any, want map[string]any) {
	t.Helper()
	if record == nil {
		t.Fatalf("%s: record missing", label)
	}
	for field, expected := range want {
		if actual, ok := record[field]; !ok || actual != expected {
			t.Errorf("%s: %s = %#v, want %#v", label, field, record[field], expected)
		}
	}
}

func nested(record map[string]any, field string) map[string]any {
	value, _ := record[field].(map[string]any)
	return value
}

func lastRecord(series map[string]any) map[string]any {
	records, _ := series["records"].([]any)
	if len(records) == 0 {
		return nil
	}
	record, _ := records[len(records)-1].(map[string]any)
	return record
}

func TestNormalizeSymbolV4(t *testing.T) {
	accepted := map[string]string{
		"600519.SH": "600519.SH", "600519": "600519.SH", "sh600519": "600519.SH", "600519.ss": "600519.SH",
		"000001": "000001.SZ", "000001.SZ": "000001.SZ", "300750": "300750.SZ", "688981": "688981.SH",
		"510300": "510300.SH", "159915": "159915.SZ", "920799": "920799.BJ", "bj920799": "920799.BJ", "430047": "430047.BJ",
		"000001.SH": "000001.SH", "399001.SZ": "399001.SZ",
		"00700.HK": "00700.HK", "700.hk": "00700.HK", "hk00700": "00700.HK", "HSI.HK": "HSI.HK",
		"AAPL.US": "AAPL.US", "aapl.us": "AAPL.US", "BRK.B.US": "BRK.B.US", "brk-b.us": "BRK.B.US", ".IXIC.US": ".IXIC.US",
	}
	for raw, want := range accepted {
		if got, err := NormalizeSymbolV4(raw); err != nil || got != want {
			t.Errorf("NormalizeSymbolV4(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "AAPL", "00700", "1234567.SH", "60051.SH", "00000.HK", "123456.HK", "700000", ".US", "TOOLONGTICKER.US", "600519.XX"} {
		if got, err := NormalizeSymbolV4(raw); err == nil {
			t.Errorf("NormalizeSymbolV4(%q) = %q, want an error", raw, got)
		}
	}
	kinds := map[string]string{"000001.SH": kindIndex, "000001.SZ": kindSecurity, "399001.SZ": kindIndex, "899050.BJ": kindIndex, "HSI.HK": kindIndex, "02800.HK": kindSecurity, ".NDX.US": kindIndex, "NDX.US": kindSecurity}
	for raw, want := range kinds {
		if item, err := parseInstrument(raw); err != nil || item.Kind != want {
			t.Errorf("parseInstrument(%q).Kind = %q, %v; want %q", raw, item.Kind, err, want)
		}
	}
}

func TestV4RequestValidation(t *testing.T) {
	body := func(symbols, operations string) string {
		return `{"schema":"MarketDataRequest/v4","request_id":"x","as_of":"2026-11-15T12:00:00+08:00","timezone":"Asia/Shanghai","symbols":` + symbols + `,"operations":` + operations + `}`
	}
	if _, err := DecodeRequest(bytes.NewBufferString(body(`["600519","00700.HK","AAPL.US",".IXIC.US"]`, `[{"name":"quotes"},{"name":"bars","timeframe":"1d","limit":60}]`))); err != nil {
		t.Fatalf("valid v4 request rejected: %v", err)
	}
	rejected := []string{
		body(`["510300.SH"]`, `[{"name":"etf_profile"}]`),
		body(`["510300.SH"]`, `[{"name":"trading_calendar"}]`),
		body(`["510300.SH"]`, `[{"name":"bars","timeframe":"1d","start":"2026-11-01","end":"2026-11-13"}]`),
		body(`["510300.SH"]`, `[{"name":"bars","timeframe":"1h"}]`),
		body(`["AAPL"]`, `[{"name":"quotes"}]`),
		body(`["`+strings.Repeat(`600519.SH","`, maxSymbolsV4)+`600519.SH"]`, `[{"name":"quotes"}]`),
	}
	for _, request := range rejected {
		if _, err := DecodeRequest(bytes.NewBufferString(request)); err == nil {
			t.Errorf("expected rejection for %.160s", request)
		}
	}
	// Multi-market symbols stay out of the earlier schemas.
	v3 := `{"schema":"MarketDataRequest/v3","request_id":"x","as_of":"2026-11-15T12:00:00+08:00","timezone":"Asia/Shanghai","symbols":["00700.HK"],"operations":[{"name":"quotes"}]}`
	if _, err := DecodeRequest(bytes.NewBufferString(v3)); err == nil {
		t.Error("v3 accepted a Hong Kong symbol")
	}
}

func TestV4QuotesUseOneBatchRequestAcrossMarkets(t *testing.T) {
	server := newServerV4(t)
	symbols := []string{"600519", "sz000001", "920799.BJ", "510300.SH", "688981.SH", "00700.HK", "5.HK", "AAPL.US", "BABA.US", "brk-b.us", "000001.SH", "399001.SZ", "HSI.HK", ".IXIC.US", "999999.SH", "600519.SH"}
	envelope := server.client(recordedAt).Execute(context.Background(), requestV4("2026-11-15T12:00:00+08:00", symbols, Operation{Name: "quotes"}))

	if got := server.calls("tencent-quote"); len(got) != 1 || got[0] != "sh600519,sz000001,bj920799,sh510300,sh688981,r_hk00700,r_hk00005,usAAPL,usBABA,usBRK.B,sh000001,sz399001,r_hkHSI,us.IXIC,sh999999" {
		t.Fatalf("Tencent requests = %q", got)
	}
	// Only the symbol Tencent did not return goes to the next provider.
	if got := server.calls("sina-quote"); len(got) != 1 || got[0] != "sh999999" {
		t.Fatalf("Sina requests = %q", got)
	}
	if got := server.calls("eastmoney-quote"); len(got) != 0 {
		t.Fatalf("EastMoney requests = %q", got)
	}
	if envelope.Schema != EnvelopeSchemaV4 || envelope.Status != "partial" {
		t.Fatalf("schema=%s status=%s errors=%+v", envelope.Schema, envelope.Status, envelope.Errors)
	}
	if len(envelope.Errors) != 1 || envelope.Errors[0].Symbol != "999999.SH" || envelope.Errors[0].Code != "symbol_not_found" || envelope.Errors[0].Retryable {
		t.Fatalf("errors=%+v", envelope.Errors)
	}
	if health := envelope.ProviderHealth[providerTencent]; health.Attempts != 1 || health.Successes != 1 {
		t.Fatalf("provider_health=%+v", envelope.ProviderHealth)
	}
	quotes := quotesBySymbol(t, envelope)
	if len(quotes) != 14 {
		t.Fatalf("got %d quotes", len(quotes))
	}

	maotai := quotes["600519.SH"]
	expectFields(t, "600519.SH", maotai, map[string]any{
		"market": "CN", "exchange": "SH", "instrument_type": "security", "name": "贵州茅台",
		"price": 1435.26, "open": 1404.0, "high": 1437.69, "low": 1394.0, "previous_close": 1410.27,
		"change": 24.99, "change_ratio": 0.01772, "volume": 4178000.0, "amount": 5954834070.0,
		"bid1_price": 1435.0, "bid1_volume": 100.0, "ask1_price": 1435.26, "ask1_volume": 200.0,
		"source_timestamp": "2026-11-13T08:14:55Z", "source_timezone": "Asia/Shanghai", "timestamp_quality": "provider",
		"lag_seconds": 154588.0, "provider": providerTencent, "provider_feed": "tencent:sh",
	})
	expectFields(t, "600519.SH unit", nested(maotai, "unit"), map[string]any{"currency": "CNY", "price": "CNY_per_share", "volume": "share", "amount": "CNY", "lot_size": 100.0, "lot_size_source": "market_rule"})
	expectFields(t, "600519.SH delay", nested(maotai, "delay"), map[string]any{"level": "realtime", "minutes": nil, "basis": "provider_declared"})

	// STAR Market: Tencent's total volume is already in shares while the
	// order book is in lots; the board has no fixed lot multiple.
	expectFields(t, "688981.SH", quotes["688981.SH"], map[string]any{"volume": 60681953.0, "bid1_volume": 1900.0})
	expectFields(t, "688981.SH unit", nested(quotes["688981.SH"], "unit"), map[string]any{"currency": "CNY", "lot_size": nil, "lot_size_source": nil})
	expectFields(t, "920799.BJ", quotes["920799.BJ"], map[string]any{"exchange": "BJ", "price": 26.27, "volume": 2183800.0})
	expectFields(t, "920799.BJ unit", nested(quotes["920799.BJ"], "unit"), map[string]any{"lot_size": nil})
	expectFields(t, "510300.SH", quotes["510300.SH"], map[string]any{"price": 4.083, "volume": 851050200.0})

	tencent := quotes["00700.HK"]
	expectFields(t, "00700.HK", tencent, map[string]any{
		"market": "HK", "name": "腾讯控股", "price": 386.6, "previous_close": 398.8, "volume": 31493353.0, "amount": 12251162809.622,
		"bid1_price": nil, "ask1_price": nil, "source_timestamp": "2026-11-13T08:08:22Z", "source_timezone": "Asia/Hong_Kong", "provider_feed": "tencent:r_hk",
	})
	expectFields(t, "00700.HK unit", nested(tencent, "unit"), map[string]any{"currency": "HKD", "price": "HKD_per_share", "lot_size": 100.0, "lot_size_source": "provider"})
	expectFields(t, "00005.HK unit", nested(quotes["00005.HK"], "unit"), map[string]any{"lot_size": 400.0, "lot_size_source": "provider"})

	apple := quotes["AAPL.US"]
	expectFields(t, "AAPL.US", apple, map[string]any{
		"market": "US", "price": 356.27, "previous_close": 354.88, "volume": 40027467.0, "amount": 14318623506.0,
		"bid1_price": nil, "source_timestamp": "2026-11-13T21:00:01Z", "source_timezone": "America/New_York", "provider_feed": "tencent:us",
	})
	expectFields(t, "AAPL.US unit", nested(apple, "unit"), map[string]any{"currency": "USD", "lot_size": 1.0, "lot_size_source": "market_rule"})
	// Tencent flags its US security quotes as delayed without saying by
	// how much.
	expectFields(t, "AAPL.US delay", nested(apple, "delay"), map[string]any{"level": "delayed", "minutes": nil, "basis": "provider_declared"})
	expectFields(t, "BRK.B.US", quotes["BRK.B.US"], map[string]any{"price": 592.94})

	composite := quotes["000001.SH"]
	expectFields(t, "000001.SH", composite, map[string]any{"instrument_type": "index", "price": 3634.35, "volume": nil, "amount": 770614169984.0, "bid1_price": nil})
	expectFields(t, "000001.SH unit", nested(composite, "unit"), map[string]any{"currency": nil, "price": "index_point", "volume": nil, "amount": "CNY", "lot_size": nil})
	expectFields(t, "HSI.HK", quotes["HSI.HK"], map[string]any{"instrument_type": "index", "price": 23637.6, "volume": nil, "amount": 195857007140.0})
	expectFields(t, ".IXIC.US", quotes[".IXIC.US"], map[string]any{"instrument_type": "index", "price": 25136.27, "volume": nil, "amount": nil})
	expectFields(t, ".IXIC.US unit", nested(quotes[".IXIC.US"], "unit"), map[string]any{"price": "index_point", "amount": nil})
	expectFields(t, ".IXIC.US delay", nested(quotes[".IXIC.US"], "delay"), map[string]any{"level": "realtime", "basis": "provider_declared"})
}

func TestV4QuotesFallBackToSinaBatch(t *testing.T) {
	server := newServerV4(t, "tencent-quote")
	symbols := []string{"600519.SH", "688981.SH", "00700.HK", "AAPL.US", "BRK.B.US", "HSI.HK", ".IXIC.US", "000001.SH", "399001.SZ"}
	envelope := server.client(recordedAt).Execute(context.Background(), requestV4("2026-11-15T12:00:00+08:00", symbols, Operation{Name: "quotes"}))

	if got := server.calls("sina-quote"); len(got) != 1 || got[0] != "sh600519,sh688981,rt_hk00700,gb_aapl,gb_brk.b,rt_hkHSI,gb_ixic,sh000001,sz399001" {
		t.Fatalf("Sina requests = %q", got)
	}
	if envelope.Status != "partial" || len(envelope.Errors) != 1 || envelope.Errors[0].Symbol != "BRK.B.US" || envelope.Errors[0].Code != "all_providers_failed" || !envelope.Errors[0].Retryable {
		t.Fatalf("status=%s errors=%+v", envelope.Status, envelope.Errors)
	}
	if envelope.ProviderHealth[providerTencent].Failures != 1 || envelope.ProviderHealth[providerSina].Successes != 1 {
		t.Fatalf("provider_health=%+v", envelope.ProviderHealth)
	}
	quotes := quotesBySymbol(t, envelope)

	maotai := quotes["600519.SH"]
	expectFields(t, "600519.SH", maotai, map[string]any{
		"price": 1435.26, "open": 1404.0, "previous_close": 1410.27, "volume": 4177984.0, "amount": 5954834070.0,
		"bid1_price": 1435.0, "bid1_volume": 100.0, "ask1_price": 1435.26, "ask1_volume": 175.0,
		"source_timestamp": "2026-11-13T07:34:59Z", "provider": providerSina, "provider_feed": "sina:sh",
	})
	expectFields(t, "600519.SH delay", nested(maotai, "delay"), map[string]any{"level": "unknown", "basis": "none"})
	if chain, _ := maotai["provider_chain"].([]any); len(chain) != 2 || maotai["fallback_reason"] == nil {
		t.Errorf("600519.SH chain=%v fallback=%v", maotai["provider_chain"], maotai["fallback_reason"])
	}
	expectFields(t, "688981.SH", quotes["688981.SH"], map[string]any{"volume": 60681953.0, "bid1_volume": 1863.0})

	// Sina states neither the Hong Kong trading currency nor the board lot.
	tencent := quotes["00700.HK"]
	expectFields(t, "00700.HK", tencent, map[string]any{
		"name": "腾讯控股", "price": 386.6, "open": 393.0, "previous_close": 398.8, "volume": 31493353.0, "amount": 12251162809.622,
		"bid1_price": 386.6, "ask1_price": 386.8, "bid1_volume": nil, "source_timestamp": "2026-11-13T08:08:22Z", "provider_feed": "sina:rt_hk",
	})
	expectFields(t, "00700.HK unit", nested(tencent, "unit"), map[string]any{"currency": nil, "price": nil, "volume": "share", "lot_size": nil})
	expectFields(t, "00700.HK delay", nested(tencent, "delay"), map[string]any{"level": "realtime", "basis": "provider_declared"})

	// The regular-session trade time has minute resolution; the second-level
	// field had moved on with after-hours prints.
	apple := quotes["AAPL.US"]
	expectFields(t, "AAPL.US", apple, map[string]any{
		"price": 356.27, "open": 357.96, "high": 360.83, "low": 355.75, "previous_close": 354.88, "volume": 40027467.0, "amount": 14310641987.0,
		"source_timestamp": "2026-11-13T21:00:00Z", "timestamp_quality": "provider_minute", "provider_feed": "sina:gb",
	})
	expectFields(t, "AAPL.US unit", nested(apple, "unit"), map[string]any{"currency": "USD", "lot_size": 1.0})
	expectFields(t, "AAPL.US delay", nested(apple, "delay"), map[string]any{"level": "unknown", "basis": "none"})

	expectFields(t, "HSI.HK", quotes["HSI.HK"], map[string]any{"price": 23637.6, "volume": nil, "amount": 195857007140.0, "source_timestamp": "2026-11-13T08:08:40Z"})
	expectFields(t, ".IXIC.US", quotes[".IXIC.US"], map[string]any{"price": 25136.2726, "volume": nil, "amount": nil, "source_timestamp": "2026-11-13T22:16:00Z"})
	expectFields(t, "000001.SH", quotes["000001.SH"], map[string]any{"volume": nil, "amount": 770614169984.0})
	expectFields(t, "399001.SZ", quotes["399001.SZ"], map[string]any{"volume": nil})
}

func TestV4QuoteDelayFollowsMeasuredLag(t *testing.T) {
	server := newServerV4(t)
	quote := func(now time.Time, symbol string) map[string]any {
		t.Helper()
		envelope := server.client(now).Execute(context.Background(), requestV4("2026-11-13T23:00:00Z", []string{symbol}, Operation{Name: "quotes"}))
		return quotesBySymbol(t, envelope)[symbol]
	}
	// Eight seconds after the Hong Kong print the quote is evidently live.
	live := quote(time.Date(2026, 11, 13, 8, 8, 30, 0, time.UTC), "00700.HK")
	expectFields(t, "live", live, map[string]any{"lag_seconds": 8.0})
	expectFields(t, "live delay", nested(live, "delay"), map[string]any{"level": "realtime", "basis": "measured_lag"})

	// A measured lag overrides the provider's delayed flag; a stale print
	// keeps it.
	fresh := quote(time.Date(2026, 11, 13, 21, 0, 30, 0, time.UTC), "AAPL.US")
	expectFields(t, "fresh delay", nested(fresh, "delay"), map[string]any{"level": "realtime", "basis": "measured_lag"})
	stale := quote(time.Date(2026, 11, 13, 21, 15, 1, 0, time.UTC), "AAPL.US")
	expectFields(t, "stale", stale, map[string]any{"lag_seconds": 900.0})
	expectFields(t, "stale delay", nested(stale, "delay"), map[string]any{"level": "delayed", "basis": "provider_declared"})
}

func TestV4QuoteRejectsFutureTimestamp(t *testing.T) {
	server := newServerV4(t)
	// One hour before the recorded print: both batch providers are refused
	// and the symbol fails rather than surfacing a time-travelling quote.
	now := time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC)
	envelope := server.client(now).Execute(context.Background(), requestV4("2026-11-13T23:00:00Z", []string{"00700.HK"}, Operation{Name: "quotes"}))
	if envelope.Status != "failed" || len(envelope.Errors) != 1 || envelope.Errors[0].Code != "all_providers_failed" || !strings.Contains(envelope.Errors[0].Message, "in the future") {
		t.Fatalf("status=%s errors=%+v", envelope.Status, envelope.Errors)
	}
}

// redirectingDoer answers batch providers with a transport error and
// EastMoney with the recorded payload as served after its redirect to the
// delayed-quote host.
type redirectingDoer struct{ payload []byte }

func (d redirectingDoer) Do(req *http.Request) (*http.Response, error) {
	if req.URL.Path != "/eastmoney-quote" || req.URL.Query().Get("secid") != "1.600519" {
		return nil, errors.New("connection refused")
	}
	final := req.Clone(req.Context())
	final.URL.Host = "push2delay.eastmoney.com"
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(d.payload)), Request: final}, nil
}

func TestV4EastmoneyFallbackLabelsDelayedHost(t *testing.T) {
	client := &Client{
		HTTP:      redirectingDoer{payload: fixtureV4(t, "eastmoney_quote_push2delay_1.600519.json")},
		Endpoints: Endpoints{QuoteTencent: "http://provider.test/tencent-quote?q=", QuoteSina: "http://provider.test/sina-quote?list=", Quote: "http://provider.test/eastmoney-quote"},
		Now:       func() time.Time { return recordedAt },
	}
	envelope := client.Execute(context.Background(), requestV4("2026-11-15T12:00:00+08:00", []string{"600519.SH", "00700.HK"}, Operation{Name: "quotes"}))
	if envelope.Status != "partial" || len(envelope.Errors) != 1 || envelope.Errors[0].Symbol != "00700.HK" {
		t.Fatalf("status=%s errors=%+v", envelope.Status, envelope.Errors)
	}
	maotai := quotesBySymbol(t, envelope)["600519.SH"]
	// The delayed host leaves f124 at zero; the trade time comes from f86.
	expectFields(t, "600519.SH", maotai, map[string]any{
		"price": 1435.26, "volume": 4178000.0, "amount": 5954834070.0, "previous_close": 1410.27,
		"source_timestamp": "2026-11-13T08:11:35Z", "timestamp_quality": "provider",
		"provider": providerEast, "provider_feed": "eastmoney:push2delay",
	})
	expectFields(t, "600519.SH delay", nested(maotai, "delay"), map[string]any{"level": "delayed", "minutes": nil, "basis": "provider_declared"})
	if chain, _ := maotai["provider_chain"].([]any); len(chain) != 3 {
		t.Errorf("provider_chain=%v", maotai["provider_chain"])
	}
}

func TestV4DailyBarsAcrossMarkets(t *testing.T) {
	server := newServerV4(t)
	symbols := []string{"600519.SH", "00700.HK", "AAPL.US", "HSI.HK", ".IXIC.US"}
	envelope := server.client(recordedAt).Execute(context.Background(), requestV4("2026-11-15T12:00:00+08:00", symbols, Operation{Name: "bars", Timeframe: "1d", Limit: 5}))
	if envelope.Status != "complete" {
		t.Fatalf("status=%s errors=%+v", envelope.Status, envelope.Errors)
	}
	// US securities are addressed by exchange; one batched quote resolves it.
	if got := server.calls("tencent-quote"); len(got) != 1 || got[0] != "usAAPL" {
		t.Fatalf("Tencent quote requests = %q", got)
	}
	if got := strings.Join(server.calls("tencent-bars"), " "); got != "hk00700,day,,,5, usAAPL.OQ,day,,,5, hkHSI,day,,,5, us.IXIC,day,,,5," {
		t.Fatalf("Tencent bar requests = %q", got)
	}
	series := seriesBySymbol(t, envelope, "bars/1d")

	maotai := series["600519.SH"]
	expectFields(t, "600519.SH", maotai, map[string]any{"provider": providerSina, "adjustment": "none", "timeframe": "1d", "timezone": "Asia/Shanghai", "instrument_type": "security"})
	expectFields(t, "600519.SH unit", nested(maotai, "unit"), map[string]any{"currency": "CNY", "volume": "share", "lot_size": 100.0})
	if records, _ := maotai["records"].([]any); len(records) != 5 {
		t.Fatalf("600519.SH has %d bars", len(records))
	}
	expectFields(t, "600519.SH last", lastRecord(maotai), map[string]any{
		"trade_date": "2026-11-13", "timestamp": "2026-11-13T07:00:00Z", "open": 1404.0, "close": 1435.26, "high": 1437.69, "low": 1394.0,
		"volume": 4177984.0, "amount": nil, "complete": true,
	})

	tencent := series["00700.HK"]
	expectFields(t, "00700.HK", tencent, map[string]any{"provider": providerTencent, "adjustment": "none", "timezone": "Asia/Hong_Kong"})
	expectFields(t, "00700.HK unit", nested(tencent, "unit"), map[string]any{"currency": "HKD", "price": "HKD_per_share", "lot_size": 100.0, "lot_size_source": "provider"})
	expectFields(t, "00700.HK last", lastRecord(tencent), map[string]any{"trade_date": "2026-11-13", "timestamp": "2026-11-13T08:00:00Z", "close": 386.6, "volume": 31493353.0, "amount": nil})

	apple := series["AAPL.US"]
	expectFields(t, "AAPL.US", apple, map[string]any{"provider": providerTencent, "timezone": "America/New_York"})
	expectFields(t, "AAPL.US unit", nested(apple, "unit"), map[string]any{"currency": "USD", "lot_size": 1.0, "lot_size_source": "market_rule"})
	expectFields(t, "AAPL.US last", lastRecord(apple), map[string]any{"trade_date": "2026-11-13", "timestamp": "2026-11-13T21:00:00Z", "close": 356.27, "volume": 40027467.0})
	// The ex-dividend bar carries an extra annotation element.
	records, _ := apple["records"].([]any)
	expectFields(t, "AAPL.US ex-dividend", records[3].(map[string]any), map[string]any{"trade_date": "2026-11-12", "close": 354.88, "volume": 31874978.0})

	// Tencent puts turnover in the index volume slot; it is not exported.
	expectFields(t, "HSI.HK last", lastRecord(series["HSI.HK"]), map[string]any{"close": 23637.6, "volume": nil, "amount": nil})
	expectFields(t, "HSI.HK unit", nested(series["HSI.HK"], "unit"), map[string]any{"price": "index_point", "currency": nil})
	expectFields(t, ".IXIC.US last", lastRecord(series[".IXIC.US"]), map[string]any{"close": 25136.27, "timestamp": "2026-11-13T21:00:00Z", "volume": nil})
}

func TestV4BarsWithholdAndFlagUnfinishedSessions(t *testing.T) {
	server := newServerV4(t)
	bars := func(now time.Time, asOf string) map[string]any {
		t.Helper()
		envelope := server.client(now).Execute(context.Background(), requestV4(asOf, []string{"00700.HK"}, Operation{Name: "bars", Timeframe: "1d", Limit: 5}))
		return seriesBySymbol(t, envelope, "bars/1d")["00700.HK"]
	}
	// 15:30 Hong Kong time: the session closes at 16:00, so the day's bar is
	// withheld from a point-in-time request.
	midSession := time.Date(2026, 11, 13, 7, 30, 0, 0, time.UTC)
	expectFields(t, "point in time", lastRecord(bars(midSession, "2026-11-13T15:30:00+08:00")), map[string]any{"trade_date": "2026-11-12", "complete": true})
	// A later as_of lets the forming bar through, marked incomplete.
	expectFields(t, "forming", lastRecord(bars(midSession, "2026-11-13T23:59:00+08:00")), map[string]any{"trade_date": "2026-11-13", "complete": false})
}

func TestV4BarsFallBackWithoutSplicingProviders(t *testing.T) {
	server := newServerV4(t, "tencent-quote", "tencent-bars", "sina-bars")
	symbols := []string{"600519.SH", "000001.SH", "00700.HK", "AAPL.US", "HSI.HK"}
	envelope := server.client(recordedAt).Execute(context.Background(), requestV4("2026-11-15T12:00:00+08:00", symbols, Operation{Name: "bars", Timeframe: "1d", Limit: 5}))
	if envelope.Status != "complete" {
		t.Fatalf("status=%s errors=%+v", envelope.Status, envelope.Errors)
	}
	series := seriesBySymbol(t, envelope, "bars/1d")
	for _, symbol := range symbols {
		expectFields(t, symbol, series[symbol], map[string]any{"provider": providerEast})
		if series[symbol]["fallback_reason"] == nil {
			t.Errorf("%s: fallback_reason missing", symbol)
		}
	}
	// EastMoney reports mainland volume in lots and states turnover.
	expectFields(t, "600519.SH last", lastRecord(series["600519.SH"]), map[string]any{"volume": 4178000.0, "amount": 5954834070.0})
	expectFields(t, "000001.SH last", lastRecord(series["000001.SH"]), map[string]any{"volume": nil, "amount": 770614169983.6})
	expectFields(t, "000001.SH unit", nested(series["000001.SH"], "unit"), map[string]any{"price": "index_point", "amount": "CNY"})
	// Without Tencent nothing states the Hong Kong currency or lot.
	expectFields(t, "00700.HK last", lastRecord(series["00700.HK"]), map[string]any{"volume": 31493353.0, "amount": 12251162880.0})
	expectFields(t, "00700.HK unit", nested(series["00700.HK"], "unit"), map[string]any{"currency": nil, "lot_size": nil})
	// With no exchange suffix the US market numbers are probed in order.
	expectFields(t, "AAPL.US last", lastRecord(series["AAPL.US"]), map[string]any{"close": 356.27, "volume": 40027467.0, "amount": 14310989056.0})
	if got := strings.Join(server.calls("eastmoney-bars"), " "); got != "1.600519 1.000001 116.00700 105.AAPL 100.HSI" {
		t.Errorf("EastMoney requests = %q", got)
	}

	// Last US fallback: Sina returns the whole history and is trimmed.
	last := newServerV4(t, "tencent-quote", "tencent-bars", "eastmoney-bars")
	envelope = last.client(recordedAt).Execute(context.Background(), requestV4("2026-11-15T12:00:00+08:00", []string{"AAPL.US", "00700.HK"}, Operation{Name: "bars", Timeframe: "1d", Limit: 5}))
	if envelope.Status != "partial" || len(envelope.Errors) != 1 || envelope.Errors[0].Symbol != "00700.HK" || envelope.Errors[0].Code != "all_providers_failed" {
		t.Fatalf("status=%s errors=%+v", envelope.Status, envelope.Errors)
	}
	apple := seriesBySymbol(t, envelope, "bars/1d")["AAPL.US"]
	expectFields(t, "AAPL.US via Sina", apple, map[string]any{"provider": providerSina})
	if records, _ := apple["records"].([]any); len(records) != 5 {
		t.Fatalf("AAPL.US via Sina has %d bars", len(records))
	}
	expectFields(t, "AAPL.US via Sina last", lastRecord(apple), map[string]any{"trade_date": "2026-11-13", "close": 356.27, "volume": 40027467.0, "amount": 14310600000.0})
}

func TestV4FiveMinuteBarsAreMainlandOnly(t *testing.T) {
	server := newServerV4(t)
	envelope := server.client(recordedAt).Execute(context.Background(), requestV4("2026-11-15T12:00:00+08:00", []string{"00700.HK", "AAPL.US"}, Operation{Name: "bars", Timeframe: "5m"}))
	if envelope.Status != "failed" || len(envelope.Errors) != 2 {
		t.Fatalf("status=%s errors=%+v", envelope.Status, envelope.Errors)
	}
	for _, item := range envelope.Errors {
		if item.Code != "unsupported_timeframe" || item.Retryable {
			t.Errorf("error=%+v", item)
		}
	}
	if got := len(server.calls("tencent-bars")) + len(server.calls("eastmoney-bars")) + len(server.calls("tencent-quote")); got != 0 {
		t.Errorf("made %d provider requests for an unsupported timeframe", got)
	}
}

func TestV4QuotesWithoutSymbolsAreEmptyAndComplete(t *testing.T) {
	server := newServerV4(t)
	envelope := server.client(recordedAt).Execute(context.Background(), requestV4("2026-11-15T12:00:00+08:00", nil, Operation{Name: "quotes"}))
	if envelope.Status != "complete" || string(envelope.Results["quotes"]) != "[]" {
		t.Fatalf("status=%s results=%s", envelope.Status, envelope.Results["quotes"])
	}
	encoded, err := json.Marshal(envelope)
	if err != nil || !bytes.Contains(encoded, []byte(`"provider_health":{}`)) {
		t.Fatalf("envelope=%s err=%v", encoded, err)
	}
}
