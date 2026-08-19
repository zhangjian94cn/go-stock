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
	RequestSchema  = "MarketDataRequest/v1"
	EnvelopeSchema = "MarketDataEnvelope/v1"
)

var supportedOperations = map[string]bool{
	"etf_universe":     true,
	"quotes":           true,
	"bars":             true,
	"etf_profile":      true,
	"events":           true,
	"trading_calendar": true,
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
}

type PartialError struct {
	Operation string `json:"operation"`
	Symbol    string `json:"symbol,omitempty"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type Envelope struct {
	Schema     string                     `json:"schema"`
	RequestID  string                     `json:"request_id"`
	AsOf       string                     `json:"as_of"`
	ObservedAt string                     `json:"observed_at"`
	Status     string                     `json:"status"`
	Results    map[string]json.RawMessage `json:"results"`
	Errors     []PartialError             `json:"errors"`
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
	if r.Schema != RequestSchema {
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
		if seen[op.Name+"/"+op.Timeframe] {
			return fmt.Errorf("duplicate operation %q timeframe %q", op.Name, op.Timeframe)
		}
		seen[op.Name+"/"+op.Timeframe] = true
		if op.Name == "bars" && op.Timeframe != "1d" && op.Timeframe != "5m" {
			return fmt.Errorf("bars timeframe must be 1d or 5m, got %q", op.Timeframe)
		}
		if op.Limit < 0 || op.Limit > 2000 {
			return fmt.Errorf("operation %q limit must be between 0 and 2000", op.Name)
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
