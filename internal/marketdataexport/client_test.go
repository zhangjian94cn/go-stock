package marketdataexport

import (
	"bytes"
	"context"
	"encoding/json"
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
	request := `{"schema":"MarketDataRequest/v1","request_id":"side-effect","as_of":"2026-08-19T10:00:00+08:00","timezone":"Asia/Shanghai","symbols":[],"operations":[{"name":"trading_calendar","start":"2026-08-18","end":"2026-08-19"}]}`
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
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("command created runtime files: %v", entries)
	}
	var envelope Envelope
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil || envelope.Status != "complete" {
		t.Fatalf("stdout=%s err=%v", stdout.String(), err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}

func TestDecodeRequestRejectsUnknownSchemaAndOperation(t *testing.T) {
	for _, body := range []string{
		`{"schema":"MarketDataRequest/v2","request_id":"x","as_of":"2026-08-19T10:00:00+08:00","timezone":"Asia/Shanghai","operations":[{"name":"quotes"}]}`,
		`{"schema":"MarketDataRequest/v1","request_id":"x","as_of":"2026-08-19T10:00:00+08:00","timezone":"Asia/Shanghai","operations":[{"name":"write_database"}]}`,
		`{"schema":"MarketDataRequest/v1","request_id":"x","as_of":"2026-08-19T10:00:00+08:00","timezone":"Asia/Shanghai","operations":[{"name":"quotes"}],"extra":true}`,
	} {
		if _, err := DecodeRequest(bytes.NewBufferString(body)); err == nil {
			t.Fatalf("expected rejection for %s", body)
		}
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
