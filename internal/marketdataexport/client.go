package marketdataexport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type Endpoints struct {
	Quote       string
	Bars        string
	FundSuggest string
	Events      string
}

func DefaultEndpoints() Endpoints {
	return Endpoints{
		Quote:       envOr("GO_STOCK_EXPORT_QUOTE_URL", "https://push2.eastmoney.com/api/qt/stock/get"),
		Bars:        envOr("GO_STOCK_EXPORT_BARS_URL", "https://push2his.eastmoney.com/api/qt/stock/kline/get"),
		FundSuggest: envOr("GO_STOCK_EXPORT_FUND_URL", "https://fundsuggest.eastmoney.com/FundSearch/api/FundSearchAPI.ashx"),
		Events:      envOr("GO_STOCK_EXPORT_EVENTS_URL", "https://np-anotice-stock.eastmoney.com/api/security/ann"),
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

func NewClient() *Client {
	return &Client{
		HTTP:      &http.Client{Timeout: 15 * time.Second},
		Endpoints: DefaultEndpoints(),
		Now:       time.Now,
	}
}

func (c *Client) Execute(ctx context.Context, req Request) Envelope {
	observedAt := c.Now().UTC().Truncate(time.Second)
	envelope := Envelope{
		Schema: EnvelopeSchema, RequestID: req.RequestID, AsOf: req.AsOf,
		ObservedAt: observedAt.Format(time.RFC3339), Results: map[string]json.RawMessage{},
	}
	for _, op := range req.Operations {
		var result any
		var errs []PartialError
		switch op.Name {
		case "quotes":
			result, errs = c.fetchQuotes(ctx, req, observedAt)
		case "bars":
			result, errs = c.fetchBars(ctx, req, op, observedAt)
		case "etf_universe":
			result, errs = c.fetchUniverse(ctx, op, observedAt)
		case "etf_profile":
			result, errs = c.fetchProfiles(ctx, req, observedAt)
		case "events":
			result, errs = c.fetchEvents(ctx, req, observedAt)
		case "trading_calendar":
			result, errs = c.buildCalendar(req, op, observedAt)
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
	envelope.Finalize()
	return envelope
}

func operationError(operation, symbol, code string, err error, retryable bool) PartialError {
	return PartialError{Operation: operation, Symbol: symbol, Code: code, Message: fmt.Sprint(err), Retryable: retryable}
}
