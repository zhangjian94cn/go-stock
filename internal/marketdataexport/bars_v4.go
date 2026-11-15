package marketdataexport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type rawBarV4 struct {
	Stamp  string
	Open   float64
	Close  float64
	High   float64
	Low    float64
	Volume *float64
	Amount *float64
}

// barSeriesV4 is one provider's answer for one symbol. Currency and lot size
// are filled only when the provider payload states them.
type barSeriesV4 struct {
	Bars     []rawBarV4
	Currency string
	LotSize  *float64
}

func sessionClose(market string) (int, int) {
	if market == marketCN {
		return 15, 0
	}
	return 16, 0
}

func parseBarNumbers(open, closeValue, high, low string) (rawBarV4, bool) {
	values := make([]float64, 4)
	for index, text := range []string{open, closeValue, high, low} {
		value, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err != nil {
			return rawBarV4{}, false
		}
		values[index] = value
	}
	return rawBarV4{Open: values[0], Close: values[1], High: values[2], Low: values[3]}, true
}

func optionalBarNumber(text string, multiplier float64, allowZero bool) *float64 {
	value, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || (!allowZero && value == 0) {
		return nil
	}
	value = scaled(value, multiplier)
	return &value
}

// makeSeriesV4 validates one provider's bars and renders the per-symbol
// result. Daily bars are stamped at the market's nominal session close in
// the market's own timezone; bars after as_of are withheld, and a bar whose
// session had not closed when it was observed is marked incomplete.
func makeSeriesV4(item instrument, timeframe, providerName string, series barSeriesV4, limit int, asOf, observed time.Time) (map[string]any, error) {
	location, err := time.LoadLocation(marketZone(item.Market))
	if err != nil {
		return nil, err
	}
	closeHour, closeMinute := sessionClose(item.Market)
	records := []any{}
	previous := time.Time{}
	hasAmount := false
	for _, bar := range series.Bars {
		var stamp time.Time
		var parseErr error
		tradeDate := bar.Stamp
		switch {
		case timeframe == "1d":
			var day time.Time
			day, parseErr = time.ParseInLocation("2006-01-02", bar.Stamp, location)
			stamp = time.Date(day.Year(), day.Month(), day.Day(), closeHour, closeMinute, 0, 0, location)
		case len(bar.Stamp) == len("2006-01-02 15:04:05"):
			stamp, parseErr = time.ParseInLocation("2006-01-02 15:04:05", bar.Stamp, location)
		default:
			stamp, parseErr = time.ParseInLocation("2006-01-02 15:04", bar.Stamp, location)
		}
		if parseErr != nil || (!previous.IsZero() && !stamp.After(previous)) {
			return nil, fmt.Errorf("duplicate or unordered bar %q", bar.Stamp)
		}
		previous = stamp
		if len(tradeDate) > 10 {
			tradeDate = tradeDate[:10]
		}
		for _, value := range []float64{bar.Open, bar.Close, bar.High, bar.Low} {
			if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
				return nil, fmt.Errorf("invalid OHLC at %s", bar.Stamp)
			}
		}
		if bar.High < math.Max(bar.Open, bar.Close) || bar.Low > math.Min(bar.Open, bar.Close) {
			return nil, fmt.Errorf("invalid OHLC at %s", bar.Stamp)
		}
		if stamp.After(asOf) {
			continue
		}
		volume, amount := bar.Volume, bar.Amount
		if item.Kind == kindIndex {
			// Providers disagree on index volume units (lots, shares or
			// turnover), so index volume is never exported.
			volume = nil
		}
		hasAmount = hasAmount || amount != nil
		records = append(records, map[string]any{
			"timestamp": stamp.UTC().Format(time.RFC3339), "trade_date": tradeDate,
			"open": bar.Open, "close": bar.Close, "high": bar.High, "low": bar.Low,
			"volume": volume, "amount": amount, "complete": !observed.Before(stamp),
		})
	}
	if len(records) == 0 {
		return nil, errors.New("no bars at or before as_of")
	}
	if len(records) > limit {
		records = records[len(records)-limit:]
	}
	currency := series.Currency
	if !validCurrency(currency) {
		currency = ruleCurrency(item)
	}
	lot, lotSource := series.LotSize, "provider"
	if lot == nil {
		lot, lotSource = ruleLotSize(item)
	}
	return map[string]any{
		"symbol": item.Symbol, "market": item.Market, "exchange": item.Exchange, "instrument_type": item.Kind,
		"timeframe": timeframe, "adjustment": "none", "timezone": marketZone(item.Market),
		"unit": unitV4(item, currency, lot, lotSource, hasAmount), "records": records,
		"provider":   providerName,
		"provenance": "go-stock:readonly-export-v4/bars/" + strings.TrimSuffix(providerName, "_public_web"),
	}, nil
}

func (c *Client) barsTencentV4(ctx context.Context, item instrument, timeframe string, limit int, usProviderCode string) (barSeriesV4, error) {
	ticker, reason := item.tencentBarTicker(usProviderCode)
	if ticker == "" {
		return barSeriesV4{}, errors.New(reason)
	}
	if timeframe != "1d" {
		return barSeriesV4{}, errors.New("Tencent bars are used for daily data only")
	}
	const period = "day"
	// The trailing empty field requests unadjusted prices.
	param := fmt.Sprintf("%s,%s,,,%d,", ticker, period, limit)
	body, err := c.getBytes(ctx, c.Endpoints.BarsTencent, url.Values{"param": {param}}, map[string]string{"Referer": "https://gu.qq.com/"})
	if err != nil {
		return barSeriesV4{}, err
	}
	var payload struct {
		Data map[string]map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return barSeriesV4{}, fmt.Errorf("decode Tencent bars: %w", err)
	}
	stock := payload.Data[ticker]
	var items [][]json.RawMessage
	if raw, ok := stock[period]; ok {
		_ = json.Unmarshal(raw, &items)
	}
	if len(items) == 0 {
		return barSeriesV4{}, errors.New("Tencent bars missing")
	}
	volumeMultiplier := 1.0
	if item.Market == marketCN {
		volumeMultiplier = tencentMainlandVolumeMultiplier(item)
	}
	series := barSeriesV4{Bars: make([]rawBarV4, 0, len(items))}
	for _, fields := range items {
		if len(fields) < 6 {
			return barSeriesV4{}, errors.New("Tencent bar fields missing")
		}
		text := make([]string, 6)
		for index := range text {
			if err := json.Unmarshal(fields[index], &text[index]); err != nil {
				return barSeriesV4{}, errors.New("Tencent bar field is not a string")
			}
		}
		bar, ok := parseBarNumbers(text[1], text[2], text[3], text[4])
		if !ok {
			return barSeriesV4{}, fmt.Errorf("invalid Tencent bar %q", text[0])
		}
		bar.Stamp = text[0]
		bar.Volume = optionalBarNumber(text[5], volumeMultiplier, true)
		series.Bars = append(series.Bars, bar)
	}
	// The payload embeds the instrument's quote row, which states currency
	// and, for Hong Kong, the board lot.
	var embedded map[string]json.RawMessage
	if raw, ok := stock["qt"]; ok && json.Unmarshal(raw, &embedded) == nil {
		var quoteFields []string
		if json.Unmarshal(embedded[ticker], &quoteFields) == nil {
			switch item.Market {
			case marketCN:
				series.Currency = fieldAt(quoteFields, 82)
			case marketHK:
				series.Currency = fieldAt(quoteFields, 75)
				if item.Kind == kindSecurity {
					series.LotSize = positiveAt(quoteFields, 60, 1)
				}
			case marketUS:
				series.Currency = fieldAt(quoteFields, 35)
			}
		}
	}
	return series, nil
}

func (c *Client) barsEastmoneyV4(ctx context.Context, item instrument, timeframe string, limit int, usProviderCode string) (barSeriesV4, error) {
	secIDs := item.eastmoneySecIDs(usProviderCode)
	if len(secIDs) == 0 {
		return barSeriesV4{}, errors.New("no EastMoney mapping for this instrument")
	}
	klt := "101"
	if timeframe == "5m" {
		klt = "5"
	}
	volumeMultiplier := 1.0
	if item.Market == marketCN {
		volumeMultiplier = 100
	}
	var lastErr error
	for _, secID := range secIDs {
		var payload struct {
			RC   int `json:"rc"`
			Data *struct {
				Code   string   `json:"code"`
				KLines []string `json:"klines"`
			} `json:"data"`
		}
		values := url.Values{"secid": {secID}, "klt": {klt}, "fqt": {"0"}, "end": {"20500101"}, "lmt": {strconv.Itoa(limit)}, "fields1": {"f1,f2,f3,f4,f5,f6"}, "fields2": {"f51,f52,f53,f54,f55,f56,f57"}}
		if err := c.getJSON(ctx, c.Endpoints.Bars, values, &payload); err != nil {
			return barSeriesV4{}, err
		}
		if payload.RC != 0 || payload.Data == nil || len(payload.Data.KLines) == 0 {
			lastErr = errors.New("EastMoney bars missing")
			continue
		}
		if item.Kind == kindSecurity && !strings.EqualFold(payload.Data.Code, item.Code) {
			lastErr = fmt.Errorf("EastMoney returned a different instrument (%s)", payload.Data.Code)
			continue
		}
		series := barSeriesV4{Bars: make([]rawBarV4, 0, len(payload.Data.KLines))}
		for _, line := range payload.Data.KLines {
			parts := strings.Split(line, ",")
			if len(parts) < 7 {
				return barSeriesV4{}, errors.New("EastMoney bar fields missing")
			}
			bar, ok := parseBarNumbers(parts[1], parts[2], parts[3], parts[4])
			if !ok {
				return barSeriesV4{}, fmt.Errorf("invalid EastMoney bar %q", parts[0])
			}
			bar.Stamp = parts[0]
			bar.Volume = optionalBarNumber(parts[5], volumeMultiplier, true)
			// EastMoney reports zero turnover where it has none (US indices).
			bar.Amount = optionalBarNumber(parts[6], 1, false)
			series.Bars = append(series.Bars, bar)
		}
		return series, nil
	}
	return barSeriesV4{}, lastErr
}

func jsonpArray(body []byte) ([]byte, error) {
	text := string(body)
	start := strings.Index(text, "([")
	end := strings.LastIndex(text, "])")
	if start < 0 || end < start {
		return nil, errors.New("JSONP array missing")
	}
	return []byte(text[start+1 : end+1]), nil
}

func (c *Client) barsSinaCNV4(ctx context.Context, item instrument, timeframe string, limit int) (barSeriesV4, error) {
	if item.Market != marketCN {
		return barSeriesV4{}, errors.New("Sina mainland bars cover mainland symbols only")
	}
	scale := "240"
	if timeframe == "5m" {
		scale = "5"
	}
	ticker := strings.ToLower(item.Exchange) + item.Code
	body, err := c.getBytes(ctx, c.Endpoints.BarsSina, url.Values{"symbol": {ticker}, "scale": {scale}, "ma": {"no"}, "datalen": {strconv.Itoa(limit)}}, map[string]string{"Referer": "https://finance.sina.com.cn/"})
	if err != nil {
		return barSeriesV4{}, err
	}
	array, err := jsonpArray(body)
	if err != nil {
		return barSeriesV4{}, fmt.Errorf("Sina bars: %w", err)
	}
	var items []struct {
		Day, Open, High, Low, Close, Volume, Amount string
	}
	if err := json.Unmarshal(array, &items); err != nil {
		return barSeriesV4{}, fmt.Errorf("decode Sina bars: %w", err)
	}
	series := barSeriesV4{Bars: make([]rawBarV4, 0, len(items))}
	for _, current := range items {
		bar, ok := parseBarNumbers(current.Open, current.Close, current.High, current.Low)
		if !ok {
			return barSeriesV4{}, fmt.Errorf("invalid Sina bar %q", current.Day)
		}
		bar.Stamp = current.Day
		bar.Volume = optionalBarNumber(current.Volume, 1, true)
		bar.Amount = optionalBarNumber(current.Amount, 1, true)
		series.Bars = append(series.Bars, bar)
	}
	return series, nil
}

// barsSinaUSV4 is the last US fallback: the endpoint has no limit parameter
// and returns the full daily history, so it is trimmed locally.
func (c *Client) barsSinaUSV4(ctx context.Context, item instrument, limit int) (barSeriesV4, error) {
	if item.Market != marketUS || item.Kind != kindSecurity {
		return barSeriesV4{}, errors.New("Sina US bars cover US securities only")
	}
	body, err := c.getBytes(ctx, c.Endpoints.BarsSinaUS, url.Values{"symbol": {strings.ToLower(item.Code)}}, map[string]string{"Referer": "https://finance.sina.com.cn/"})
	if err != nil {
		return barSeriesV4{}, err
	}
	array, err := jsonpArray(body)
	if err != nil {
		return barSeriesV4{}, fmt.Errorf("Sina US bars: %w", err)
	}
	var items []struct {
		D, O, H, L, C, V, A string
	}
	if err := json.Unmarshal(array, &items); err != nil {
		return barSeriesV4{}, fmt.Errorf("decode Sina US bars: %w", err)
	}
	// Keep a margin beyond limit so the as_of filter still leaves enough.
	if keep := limit + 10; len(items) > keep {
		items = items[len(items)-keep:]
	}
	series := barSeriesV4{Bars: make([]rawBarV4, 0, len(items))}
	for _, current := range items {
		bar, ok := parseBarNumbers(current.O, current.C, current.H, current.L)
		if !ok {
			return barSeriesV4{}, fmt.Errorf("invalid Sina US bar %q", current.D)
		}
		bar.Stamp = current.D
		bar.Volume = optionalBarNumber(current.V, 1, true)
		bar.Amount = optionalBarNumber(current.A, 1, false)
		series.Bars = append(series.Bars, bar)
	}
	return series, nil
}

func (c *Client) fetchBarsV4(ctx context.Context, req Request, op Operation, observed time.Time) (any, []PartialError) {
	instruments := instrumentsV4(req.Symbols)
	asOf, _ := time.Parse(time.RFC3339, req.AsOf)
	limit := op.Limit
	if limit == 0 {
		limit = 240
	}
	result := map[string]any{}
	errs := []PartialError{}

	// Tencent and EastMoney address US securities by exchange. One batched
	// Tencent quote resolves the exchange-qualified code for all of them.
	usProviderCodes := map[string]string{}
	usSecurities := []instrument{}
	for _, item := range instruments {
		if item.Market == marketUS && item.Kind == kindSecurity && op.Timeframe == "1d" {
			usSecurities = append(usSecurities, item)
		}
	}
	if len(usSecurities) > 0 {
		states := map[string]*quoteStateV4{}
		for _, item := range usSecurities {
			states[item.Symbol] = &quoteStateV4{}
		}
		c.quoteRoundV4(ctx, c.quoteBatchProvidersV4()[0], usSecurities, states, observed)
		for symbol, state := range states {
			if state.record != nil {
				usProviderCodes[symbol] = state.quote.ProviderCode
			}
		}
	}

	type attempt struct {
		name string
		call func() (barSeriesV4, error)
	}
	for _, item := range instruments {
		item := item
		if op.Timeframe != "1d" && item.Market != marketCN {
			errs = append(errs, operationError("bars", item.Symbol, "unsupported_timeframe", fmt.Errorf("%s bars are available for mainland symbols only", op.Timeframe), false))
			continue
		}
		usCode := usProviderCodes[item.Symbol]
		tencent := attempt{providerTencent, func() (barSeriesV4, error) { return c.barsTencentV4(ctx, item, op.Timeframe, limit, usCode) }}
		eastmoney := attempt{providerEast, func() (barSeriesV4, error) { return c.barsEastmoneyV4(ctx, item, op.Timeframe, limit, usCode) }}
		var attempts []attempt
		switch item.Market {
		case marketCN:
			attempts = []attempt{{providerSina, func() (barSeriesV4, error) { return c.barsSinaCNV4(ctx, item, op.Timeframe, limit) }}, eastmoney}
			if op.Timeframe == "1d" {
				attempts = append(attempts, tencent)
			}
		case marketHK:
			attempts = []attempt{tencent, eastmoney}
		case marketUS:
			attempts = []attempt{tencent, eastmoney, {providerSina, func() (barSeriesV4, error) { return c.barsSinaUSV4(ctx, item, limit) }}}
		}
		chain := []providerAttempt{}
		failures := []string{}
		var selected map[string]any
		for _, current := range attempts {
			series, err := current.call()
			var rendered map[string]any
			if err == nil {
				rendered, err = makeSeriesV4(item, op.Timeframe, current.name, series, limit, asOf, observed)
			}
			recordProvider(ctx, current.name, "bars/"+op.Timeframe, err)
			if err != nil {
				chain = append(chain, providerAttempt{Provider: current.name, Status: "failed", Reason: err.Error()})
				failures = append(failures, current.name+": "+err.Error())
				continue
			}
			chain = append(chain, providerAttempt{Provider: current.name, Status: "selected"})
			selected = rendered
			break
		}
		if selected == nil {
			errs = append(errs, operationError("bars", item.Symbol, "all_providers_failed", errors.New(strings.Join(failures, "; ")), true))
			continue
		}
		selected["provider_chain"] = chain
		if len(failures) > 0 {
			selected["fallback_reason"] = strings.Join(failures, "; ")
		}
		result[item.Symbol] = selected
	}
	if len(result) == 0 && len(errs) > 0 {
		return nil, errs
	}
	return result, errs
}
