package marketdataexport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type Endpoints struct {
	Quote        string
	Bars         string
	FundSuggest  string
	Events       string
	QuoteTencent string
	QuoteSina    string
	BarsSina     string
	BarsTencent  string
	FundProfile  string
}

func DefaultEndpoints() Endpoints {
	return Endpoints{
		Quote:        envOr("GO_STOCK_EXPORT_QUOTE_URL", "https://push2.eastmoney.com/api/qt/stock/get"),
		Bars:         envOr("GO_STOCK_EXPORT_BARS_URL", "https://push2his.eastmoney.com/api/qt/stock/kline/get"),
		FundSuggest:  envOr("GO_STOCK_EXPORT_FUND_URL", "https://fundsuggest.eastmoney.com/FundSearch/api/FundSearchAPI.ashx"),
		Events:       envOr("GO_STOCK_EXPORT_EVENTS_URL", "https://np-anotice-stock.eastmoney.com/api/security/ann"),
		QuoteTencent: envOr("GO_STOCK_EXPORT_TENCENT_QUOTE_URL", "https://qt.gtimg.cn/q="),
		QuoteSina:    envOr("GO_STOCK_EXPORT_SINA_QUOTE_URL", "https://hq.sinajs.cn/list="),
		BarsSina:     envOr("GO_STOCK_EXPORT_SINA_BARS_URL", "https://quotes.sina.cn/cn/api/jsonp_v2.php/var_kline/CN_MarketDataService.getKLineData"),
		BarsTencent:  envOr("GO_STOCK_EXPORT_TENCENT_BARS_URL", "https://web.ifzq.gtimg.cn/appstock/app/fqkline/get"),
		FundProfile:  envOr("GO_STOCK_EXPORT_FUND_PROFILE_URL", "https://fund.eastmoney.com/%s.html"),
	}
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

type Client struct {
	HTTP      HTTPDoer
	Endpoints Endpoints
	Now       func() time.Time
}

type providerHealthKey struct{}

type providerHealthTracker struct {
	mu     sync.Mutex
	health map[string]ProviderHealth
}

func (t *providerHealthTracker) record(provider, operation string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	value := t.health[provider]
	value.Attempts++
	if err == nil {
		value.Successes++
	} else {
		value.Failures++
		value.LastError = err.Error()
	}
	found := false
	for _, existing := range value.Operations {
		if existing == operation {
			found = true
			break
		}
	}
	if !found {
		value.Operations = append(value.Operations, operation)
	}
	t.health[provider] = value
}

func (t *providerHealthTracker) snapshot() map[string]ProviderHealth {
	t.mu.Lock()
	defer t.mu.Unlock()
	result := make(map[string]ProviderHealth, len(t.health))
	for key, value := range t.health {
		value.Operations = append([]string(nil), value.Operations...)
		result[key] = value
	}
	return result
}

func recordProvider(ctx context.Context, provider, operation string, err error) {
	if tracker, ok := ctx.Value(providerHealthKey{}).(*providerHealthTracker); ok {
		tracker.record(provider, operation, err)
	}
}

func NewClient() *Client {
	return &Client{
		HTTP:      &http.Client{Timeout: 15 * time.Second},
		Endpoints: DefaultEndpoints(),
		Now:       time.Now,
	}
}

func (c *Client) Execute(ctx context.Context, req Request) Envelope {
	observedAt := c.Now().UTC().Truncate(time.Second)
	schema := EnvelopeSchemaV1
	var tracker *providerHealthTracker
	if req.Schema == RequestSchemaV2 {
		schema = EnvelopeSchemaV2
		tracker = &providerHealthTracker{health: map[string]ProviderHealth{}}
		ctx = context.WithValue(ctx, providerHealthKey{}, tracker)
	}
	envelope := Envelope{
		Schema: schema, RequestID: req.RequestID, AsOf: req.AsOf,
		ObservedAt: observedAt.Format(time.RFC3339), Results: map[string]json.RawMessage{},
	}
	for _, op := range req.Operations {
		var result any
		var errs []PartialError
		switch op.Name {
		case "quotes":
			if req.Schema == RequestSchemaV2 {
				result, errs = c.fetchQuotesV2(ctx, req, observedAt)
			} else {
				result, errs = c.fetchQuotes(ctx, req, observedAt)
			}
		case "bars":
			if req.Schema == RequestSchemaV2 {
				result, errs = c.fetchBarsV2(ctx, req, op, observedAt)
			} else {
				result, errs = c.fetchBars(ctx, req, op, observedAt)
			}
		case "etf_universe":
			result, errs = c.fetchUniverse(ctx, op, observedAt)
		case "etf_profile":
			if req.Schema == RequestSchemaV2 {
				result, errs = c.fetchProfilesV2(ctx, req, observedAt)
			} else {
				result, errs = c.fetchProfiles(ctx, req, observedAt)
			}
		case "events":
			result, errs = c.fetchEvents(ctx, req, observedAt)
		case "trading_calendar":
			result, errs = c.buildCalendar(req, op, observedAt)
		case "market_context":
			result, errs = c.fetchMarketContextV2(ctx, req, observedAt)
		case "fund_flow":
			result, errs = c.fetchFundFlowV2(ctx, req, observedAt)
		case "sentiment":
			result, errs = c.fetchSentimentV2(ctx, req, observedAt)
		}
		key := op.Name
		if op.Timeframe != "" {
			key += "/" + op.Timeframe
		}
		if result != nil {
			encoded, err := json.Marshal(result)
			if err != nil {
				errs = append(errs, PartialError{Operation: op.Name, Code: "encode_failed", Message: err.Error()})
			} else {
				envelope.Results[key] = encoded
			}
		}
		envelope.Errors = append(envelope.Errors, errs...)
	}
	if tracker != nil {
		envelope.ProviderHealth = tracker.snapshot()
	}
	envelope.Finalize()
	return envelope
}

func operationError(operation, symbol, code string, err error, retryable bool) PartialError {
	return PartialError{Operation: operation, Symbol: symbol, Code: code, Message: fmt.Sprint(err), Retryable: retryable}
}
