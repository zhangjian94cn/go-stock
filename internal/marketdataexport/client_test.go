package marketdataexport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCommandDoesNotWriteRuntimeFiles(t *testing.T) {
	temp := t.TempDir()
	binary := filepath.Join(temp, "market-data-export")
	build := exec.Command("go", "build", "-buildvcs=false", "-o", binary, "../../cmd/market-data-export")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build command: %v\n%s", err, output)
	}
	runtimeDir := filepath.Join(temp, "runtime")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, schema := range []string{RequestSchemaV1, RequestSchemaV2} {
		request := fmt.Sprintf(`{"schema":%q,"request_id":"side-effect","as_of":"2026-08-19T10:00:00+08:00","timezone":"Asia/Shanghai","symbols":[],"operations":[{"name":"trading_calendar","start":"2026-08-18","end":"2026-08-19"}]}`, schema)
		command := exec.Command(binary)
		command.Dir = runtimeDir
		command.Stdin = bytes.NewBufferString(request)
		stdout := &bytes.Buffer{}
		stderr := &bytes.Buffer{}
		command.Stdout = stdout
		command.Stderr = stderr
		if err := command.Run(); err != nil {
			t.Fatalf("run command: %v stderr=%s", err, stderr.String())
		}
		var envelope Envelope
		if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil || envelope.Status != "complete" {
			t.Fatalf("stdout=%s err=%v", stdout.String(), err)
		}
		if stderr.Len() != 0 {
			t.Fatalf("unexpected stderr: %s", stderr.String())
		}
	}
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("command created runtime files: %v", entries)
	}
}

func TestDecodeRequestRejectsUnknownSchemaAndOperation(t *testing.T) {
	for _, body := range []string{
		`{"schema":"MarketDataRequest/v3","request_id":"x","as_of":"2026-08-19T10:00:00+08:00","timezone":"Asia/Shanghai","operations":[{"name":"quotes"}]}`,
		`{"schema":"MarketDataRequest/v1","request_id":"x","as_of":"2026-08-19T10:00:00+08:00","timezone":"Asia/Shanghai","operations":[{"name":"write_database"}]}`,
		`{"schema":"MarketDataRequest/v1","request_id":"x","as_of":"2026-08-19T10:00:00+08:00","timezone":"Asia/Shanghai","operations":[{"name":"quotes"}],"extra":true}`,
	} {
		if _, err := DecodeRequest(bytes.NewBufferString(body)); err == nil {
			t.Fatalf("expected rejection for %s", body)
		}
	}
}

func TestDecodeRequestAcceptsV2AndKeepsV2OnlyOperationsOutOfV1(t *testing.T) {
	v2 := `{"schema":"MarketDataRequest/v2","request_id":"v2","as_of":"2026-08-19T10:00:00+08:00","timezone":"Asia/Shanghai","symbols":["510300.SH"],"operations":[{"name":"market_context"},{"name":"fund_flow"},{"name":"sentiment"}]}`
	if request, err := DecodeRequest(bytes.NewBufferString(v2)); err != nil || request.Schema != RequestSchemaV2 {
		t.Fatalf("request=%+v err=%v", request, err)
	}
	v1 := `{"schema":"MarketDataRequest/v1","request_id":"v1","as_of":"2026-08-19T10:00:00+08:00","timezone":"Asia/Shanghai","symbols":["510300.SH"],"operations":[{"name":"sentiment"}]}`
	if _, err := DecodeRequest(bytes.NewBufferString(v1)); err == nil {
		t.Fatal("expected v1 to reject v2-only operation")
	}
}

func TestV2QuoteFallbackAndProviderHealth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tencent":
			http.Error(w, "unavailable", http.StatusBadGateway)
		case "/sina":
			_, _ = w.Write([]byte(`var hq_str_sh510300="CSI300 ETF,3.80,3.81,3.90,3.91,3.79,0,0,1234500,4814550,1000,3.89,0,0,0,0,0,0,0,0,1200,3.90,0,0,0,0,0,0,0,0,2026-08-19,10:00:00,00";`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := &Client{
		HTTP: server.Client(),
		Endpoints: Endpoints{
			QuoteTencent: server.URL + "/tencent?q=",
			QuoteSina:    server.URL + "/sina?list=",
			Quote:        server.URL + "/eastmoney",
		},
		Now: func() time.Time { return time.Date(2026, 8, 19, 2, 0, 20, 0, time.UTC) },
	}
	req := Request{Schema: RequestSchemaV2, RequestID: "fallback", AsOf: "2026-08-19T10:01:00+08:00", Timezone: "Asia/Shanghai", Symbols: []string{"510300.SH"}, Operations: []Operation{{Name: "quotes"}}}
	envelope := c.Execute(context.Background(), req)
	if envelope.Schema != EnvelopeSchemaV2 || envelope.Status != "complete" {
		t.Fatalf("envelope=%+v", envelope)
	}
	if envelope.ProviderHealth[providerTencent].Failures != 1 || envelope.ProviderHealth[providerSina].Successes != 1 {
		t.Fatalf("provider_health=%+v", envelope.ProviderHealth)
	}
	var quotes []map[string]any
	if err := json.Unmarshal(envelope.Results["quotes"], &quotes); err != nil || len(quotes) != 1 {
		t.Fatalf("quotes=%+v err=%v", quotes, err)
	}
	if quotes[0]["provider"] != providerSina || quotes[0]["fallback_reason"] == nil {
		t.Fatalf("quote=%+v", quotes[0])
	}
	chain, ok := quotes[0]["provider_chain"].([]any)
	if !ok || len(chain) != 2 {
		t.Fatalf("provider_chain=%+v", quotes[0]["provider_chain"])
	}
}

func TestV2ProfileParsesTrackingHistoryScaleAndFees(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><body><div class="merchandiseDetail"><div class="fundDetail-tit">沪深300ETF</div></div><div class="infoOfFund"><table><tr><td>基金类型：指数型</td><td>成立日期：2012-05-28</td><td>基金规模：560.25亿元</td></tr><tr><td>管理人：测试基金</td><td>跟踪标的：沪深300指数</td></tr></table></div><div>管理费率：0.50% 托管费率：0.10%</div></body></html>`))
	}))
	defer server.Close()
	c := &Client{HTTP: server.Client(), Endpoints: Endpoints{FundProfile: server.URL + "/%s.html"}, Now: func() time.Time { return time.Date(2026, 8, 19, 2, 0, 0, 0, time.UTC) }}
	req := Request{Schema: RequestSchemaV2, RequestID: "profile", AsOf: "2026-08-19T10:00:00+08:00", Timezone: "Asia/Shanghai", Symbols: []string{"510300.SH"}, Operations: []Operation{{Name: "etf_profile"}}}
	envelope := c.Execute(context.Background(), req)
	if envelope.Status != "complete" {
		t.Fatalf("envelope=%+v", envelope)
	}
	var profiles []map[string]any
	if err := json.Unmarshal(envelope.Results["etf_profile"], &profiles); err != nil || len(profiles) != 1 {
		t.Fatalf("profiles=%+v err=%v", profiles, err)
	}
	profile := profiles[0]
	if profile["tracking_index"] != "沪深300指数" || profile["establishment_date"] != "2012-05-28" || profile["assets_cny"] != 56025000000.0 {
		t.Fatalf("profile=%+v", profile)
	}
	if profile["management_fee_rate"] != 0.005 || profile["custody_fee_rate"] != 0.001 {
		t.Fatalf("fees=%+v/%+v", profile["management_fee_rate"], profile["custody_fee_rate"])
	}
}

func TestV2SinaFiveMinuteBarsAcceptSecondsAndUseProviderAmount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`var_kline([{"day":"2026-08-19 09:35:00","open":"3.80","high":"3.91","low":"3.79","close":"3.90","volume":"10000","amount":"39000.50"},{"day":"2026-08-19 09:40:00","open":"3.90","high":"3.92","low":"3.88","close":"3.91","volume":"12000","amount":"46920.25"}])`))
	}))
	defer server.Close()
	c := &Client{
		HTTP: server.Client(),
		Endpoints: Endpoints{
			BarsSina:    server.URL,
			Bars:        server.URL + "/eastmoney",
			BarsTencent: server.URL + "/tencent",
		},
		Now: func() time.Time { return time.Date(2026, 8, 19, 2, 0, 0, 0, time.UTC) },
	}
	req := Request{Schema: RequestSchemaV2, RequestID: "sina-bars", AsOf: "2026-08-19T10:00:00+08:00", Timezone: "Asia/Shanghai", Symbols: []string{"510300.SH"}, Operations: []Operation{{Name: "bars", Timeframe: "5m", Limit: 2}}}
	envelope := c.Execute(context.Background(), req)
	if envelope.Status != "complete" || envelope.ProviderHealth[providerSina].Successes != 1 {
		t.Fatalf("envelope=%+v", envelope)
	}
	var grouped map[string][]map[string]any
	if err := json.Unmarshal(envelope.Results["bars/5m"], &grouped); err != nil {
		t.Fatal(err)
	}
	bars := grouped["510300.SH"]
	if len(bars) != 2 || bars[0]["amount_cny"] != 39000.50 || bars[0]["amount_quality"] != nil {
		t.Fatalf("bars=%+v", bars)
	}
	if bars[0]["timestamp"] != "2026-08-19T01:35:00Z" {
		t.Fatalf("timestamp=%v", bars[0]["timestamp"])
	}
}

func TestV1EnvelopeRemainsV1WithoutV2ProviderHealth(t *testing.T) {
	c := &Client{HTTP: http.DefaultClient, Endpoints: Endpoints{}, Now: func() time.Time { return time.Date(2026, 8, 19, 2, 0, 0, 0, time.UTC) }}
	req := Request{Schema: RequestSchemaV1, RequestID: "v1", AsOf: "2026-08-19T10:00:00+08:00", Timezone: "Asia/Shanghai", Operations: []Operation{{Name: "trading_calendar", Start: "2026-08-19", End: "2026-08-19"}}}
	envelope := c.Execute(context.Background(), req)
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Schema != EnvelopeSchemaV1 || bytes.Contains(encoded, []byte("provider_health")) {
		t.Fatalf("envelope=%s", encoded)
	}
}

func TestExecuteKeepsPartialFailuresStructured(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/quote":
			if r.URL.Query().Get("secid") == "1.510300" {
				_, _ = w.Write([]byte(`{"rc":0,"data":{"f58":"沪深300ETF","f43":3.9,"f44":4,"f45":3.8,"f46":3.85,"f47":100,"f48":39000000,"f60":3.84,"f19":3.89,"f20":10,"f39":3.9,"f40":12,"f124":1787104800}}`))
			} else {
				_, _ = w.Write([]byte(`{"rc":0,"data":null}`))
			}
		case "/bars":
			_, _ = w.Write([]byte(`{"rc":0,"data":{"klines":["2026-08-19 09:35,3.8,3.9,3.91,3.79,100,390000"]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := &Client{HTTP: server.Client(), Endpoints: Endpoints{Quote: server.URL + "/quote", Bars: server.URL + "/bars"}, Now: func() time.Time { return time.Date(2026, 8, 19, 10, 30, 0, 0, time.UTC) }}
	req := Request{Schema: RequestSchema, RequestID: "partial", AsOf: "2026-08-19T18:00:00+08:00", Timezone: "Asia/Shanghai", Symbols: []string{"510300.SH", "159915.SZ"}, Operations: []Operation{{Name: "quotes"}, {Name: "bars", Timeframe: "5m", Limit: 10}}}
	envelope := c.Execute(context.Background(), req)
	if envelope.Status != "partial" {
		t.Fatalf("status=%s errors=%+v", envelope.Status, envelope.Errors)
	}
	if len(envelope.Errors) != 1 || envelope.Errors[0].Symbol != "159915.SZ" {
		t.Fatalf("errors=%+v", envelope.Errors)
	}
	if len(envelope.Results) != 2 {
		t.Fatalf("results=%v", envelope.Results)
	}
	var quotes []map[string]any
	if err := json.Unmarshal(envelope.Results["quotes"], &quotes); err != nil || len(quotes) != 1 {
		t.Fatalf("quotes=%v err=%v", quotes, err)
	}
}

func TestBarsFailClosedOnDuplicateAndNonFinite(t *testing.T) {
	tests := []string{
		`{"rc":0,"data":{"klines":["2026-08-19 09:35,3.8,3.9,3.91,3.79,100,390000","2026-08-19 09:35,3.8,3.9,3.91,3.79,100,390000"]}}`,
		`{"rc":0,"data":{"klines":["2026-08-19 09:35,NaN,3.9,3.91,3.79,100,390000"]}}`,
		`{"rc":0,"data":{"klines":["2026-08-19 09:35,3.95,3.9,3.91,3.79,100,390000"]}}`,
	}
	for _, payload := range tests {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(payload)) }))
		c := &Client{HTTP: server.Client(), Endpoints: Endpoints{Bars: server.URL}, Now: time.Now}
		req := Request{Schema: RequestSchema, RequestID: "bad", AsOf: "2026-08-19T10:00:00+08:00", Timezone: "Asia/Shanghai", Symbols: []string{"510300.SH"}, Operations: []Operation{{Name: "bars", Timeframe: "5m"}}}
		envelope := c.Execute(context.Background(), req)
		server.Close()
		if envelope.Status != "failed" || len(envelope.Errors) == 0 {
			t.Fatalf("envelope=%+v", envelope)
		}
	}
}

func TestEventsCarryRequestedSymbolAndExcludePostCutoffRecords(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"list":[{"title":"基金公告","notice_date":"2026-08-19 10:00:00","codes":[{"stock_code":"510300"}]},{"title":"未来公告","notice_date":"2026-08-19 11:00:00","codes":[{"stock_code":"510300"}]}]}}`))
	}))
	defer server.Close()
	c := &Client{HTTP: server.Client(), Endpoints: Endpoints{Events: server.URL}, Now: time.Now}
	req := Request{Schema: RequestSchema, RequestID: "events", AsOf: "2026-08-19T10:30:00+08:00", Timezone: "Asia/Shanghai", Symbols: []string{"510300.SH", "159915.SZ"}, Operations: []Operation{{Name: "events"}}}
	envelope := c.Execute(context.Background(), req)
	if envelope.Status != "complete" {
		t.Fatalf("envelope=%+v", envelope)
	}
	var events []map[string]any
	if err := json.Unmarshal(envelope.Results["events"], &events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0]["title"] != "基金公告" {
		t.Fatalf("events=%+v", events)
	}
	symbols, ok := events[0]["symbols"].([]any)
	if !ok || len(symbols) != 1 || symbols[0] != "510300.SH" {
		t.Fatalf("event symbols=%+v", events[0]["symbols"])
	}
	if events[0]["published_at"] != "2026-08-19T02:00:00Z" {
		t.Fatalf("published_at=%v", events[0]["published_at"])
	}
}
