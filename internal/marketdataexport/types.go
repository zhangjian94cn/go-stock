package marketdataexport

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const (
	RequestSchemaV1  = "MarketDataRequest/v1"
	RequestSchemaV2  = "MarketDataRequest/v2"
	RequestSchemaV3  = "MarketDataRequest/v3"
	EnvelopeSchemaV1 = "MarketDataEnvelope/v1"
	EnvelopeSchemaV2 = "MarketDataEnvelope/v2"
	EnvelopeSchemaV3 = "MarketDataEnvelope/v3"

	// Backward-compatible aliases retained for existing v1 callers and tests.
	RequestSchema  = RequestSchemaV1
	EnvelopeSchema = EnvelopeSchemaV1
)

var supportedOperations = map[string]bool{
	"etf_universe":     true,
	"quotes":           true,
	"bars":             true,
	"bars_history":     true,
	"etf_profile":      true,
	"events":           true,
	"trading_calendar": true,
	"market_context":   true,
	"fund_flow":        true,
	"sentiment":        true,
}

// Request is the only accepted stdin contract. Unknown fields are rejected by DecodeRequest.
type Request struct {
	Schema     string      `json:"schema"`
	RequestID  string      `json:"request_id"`
	AsOf       string      `json:"as_of"`
	Timezone   string      `json:"timezone"`
	Symbols    []string    `json:"symbols"`
	Operations []Operation `json:"operations"`
}

type Operation struct {
	Name      string `json:"name"`
	Timeframe string `json:"timeframe,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Start     string `json:"start,omitempty"`
	End       string `json:"end,omitempty"`
	Cursor    string `json:"cursor,omitempty"`
}

type PartialError struct {
	Operation string `json:"operation"`
	Symbol    string `json:"symbol,omitempty"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type Envelope struct {
	Schema         string                     `json:"schema"`
	RequestID      string                     `json:"request_id"`
	AsOf           string                     `json:"as_of"`
	ObservedAt     string                     `json:"observed_at"`
	Status         string                     `json:"status"`
	Results        map[string]json.RawMessage `json:"results"`
	Errors         []PartialError             `json:"errors"`
	ProviderHealth map[string]ProviderHealth  `json:"provider_health,omitempty"`
}

// MarshalJSON keeps the v1 wire contract unchanged while making the v2
// provider_health field structurally stable. A v2 operation such as
// trading_calendar may not contact a market-data provider, but consumers must
// still receive an object rather than an omitted field.
func (e Envelope) MarshalJSON() ([]byte, error) {
	type envelopeV1 Envelope
	if e.Schema != EnvelopeSchemaV2 && e.Schema != EnvelopeSchemaV3 {
		return json.Marshal(envelopeV1(e))
	}
	health := e.ProviderHealth
	if health == nil {
		health = map[string]ProviderHealth{}
	}
	type envelopeV2 struct {
		Schema         string                     `json:"schema"`
		RequestID      string                     `json:"request_id"`
		AsOf           string                     `json:"as_of"`
		ObservedAt     string                     `json:"observed_at"`
		Status         string                     `json:"status"`
		Results        map[string]json.RawMessage `json:"results"`
		Errors         []PartialError             `json:"errors"`
		ProviderHealth map[string]ProviderHealth  `json:"provider_health"`
	}
	return json.Marshal(envelopeV2{
		Schema:         e.Schema,
		RequestID:      e.RequestID,
		AsOf:           e.AsOf,
		ObservedAt:     e.ObservedAt,
		Status:         e.Status,
		Results:        e.Results,
		Errors:         e.Errors,
		ProviderHealth: health,
	})
}

// ProviderHealth is request-scoped evidence about bounded provider fallback.
// It deliberately contains no credentials or local runtime paths.
type ProviderHealth struct {
	Attempts   int      `json:"attempts"`
	Successes  int      `json:"successes"`
	Failures   int      `json:"failures"`
	Operations []string `json:"operations"`
	LastError  string   `json:"last_error,omitempty"`
}

func DecodeRequest(r io.Reader) (Request, error) {
	decoder := json.NewDecoder(io.LimitReader(r, 1<<20))
	decoder.DisallowUnknownFields()
	var req Request
	if err := decoder.Decode(&req); err != nil {
		return Request{}, fmt.Errorf("decode request: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Request{}, errors.New("request must contain exactly one JSON object")
	}
	if err := req.Validate(); err != nil {
		return Request{}, err
	}
	return req, nil
}

func (r Request) Validate() error {
	if r.Schema != RequestSchemaV1 && r.Schema != RequestSchemaV2 && r.Schema != RequestSchemaV3 {
		return fmt.Errorf("unsupported schema %q", r.Schema)
	}
	if strings.TrimSpace(r.RequestID) == "" {
		return errors.New("request_id is required")
	}
	if _, err := time.Parse(time.RFC3339, r.AsOf); err != nil {
		return fmt.Errorf("as_of must be RFC3339: %w", err)
	}
	if _, err := time.LoadLocation(r.Timezone); err != nil {
		return fmt.Errorf("invalid timezone %q: %w", r.Timezone, err)
	}
	if len(r.Operations) == 0 {
		return errors.New("at least one operation is required")
	}
	seen := map[string]bool{}
	for _, op := range r.Operations {
		if !supportedOperations[op.Name] {
			return fmt.Errorf("unsupported operation %q", op.Name)
		}
		if r.Schema == RequestSchemaV1 && (op.Name == "market_context" || op.Name == "fund_flow" || op.Name == "sentiment") {
			return fmt.Errorf("operation %q requires %s", op.Name, RequestSchemaV2)
		}
		if op.Name == "bars_history" && r.Schema != RequestSchemaV3 {
			return fmt.Errorf("operation %q requires %s", op.Name, RequestSchemaV3)
		}
		if seen[op.Name+"/"+op.Timeframe] {
			return fmt.Errorf("duplicate operation %q timeframe %q", op.Name, op.Timeframe)
		}
		seen[op.Name+"/"+op.Timeframe] = true
		if (op.Name == "bars" || op.Name == "bars_history") && op.Timeframe != "1d" && op.Timeframe != "5m" {
			return fmt.Errorf("bars timeframe must be 1d or 5m, got %q", op.Timeframe)
		}
		maximum := 2000
		if op.Name == "bars_history" {
			maximum = 10000
			if strings.TrimSpace(op.Start) == "" || strings.TrimSpace(op.End) == "" {
				return errors.New("bars_history requires start and end")
			}
			start, startErr := time.Parse("2006-01-02", op.Start)
			end, endErr := time.Parse("2006-01-02", op.End)
			if startErr != nil || endErr != nil || end.Before(start) {
				return errors.New("bars_history start/end must be ordered YYYY-MM-DD dates")
			}
			if op.Cursor != "" {
				if _, err := time.Parse(time.RFC3339, op.Cursor); err != nil {
					return fmt.Errorf("bars_history cursor must be RFC3339: %w", err)
				}
			}
		}
		if op.Limit < 0 || op.Limit > maximum {
			return fmt.Errorf("operation %q limit must be between 0 and %d", op.Name, maximum)
		}
	}
	for _, symbol := range r.Symbols {
		if _, err := NormalizeSymbol(symbol); err != nil {
			return err
		}
	}
	return nil
}

func (e *Envelope) Finalize() {
	switch {
	case len(e.Errors) == 0:
		e.Status = "complete"
	case len(e.Results) == 0:
		e.Status = "failed"
	default:
		e.Status = "partial"
	}
	if e.Results == nil {
		e.Results = map[string]json.RawMessage{}
	}
	if e.Errors == nil {
		e.Errors = []PartialError{}
	}
	sort.Slice(e.Errors, func(i, j int) bool {
		left := e.Errors[i].Operation + "/" + e.Errors[i].Symbol + "/" + e.Errors[i].Code
		right := e.Errors[j].Operation + "/" + e.Errors[j].Symbol + "/" + e.Errors[j].Code
		return left < right
	})
}
