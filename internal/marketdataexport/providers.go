package marketdataexport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const provider = "eastmoney_public_web"

var universeKeywords = []string{"ETF", "宽基ETF", "沪深300ETF", "中证500ETF", "中证1000ETF", "创业板ETF", "科创ETF", "红利ETF", "证券ETF", "芯片ETF", "医药ETF", "消费ETF", "新能源ETF", "军工ETF", "人工智能ETF", "机器人ETF", "黄金ETF"}

type provenance struct {
	Provider        string `json:"provider"`
	Provenance      string `json:"provenance"`
	Unit            any    `json:"unit"`
	SourceTimestamp string `json:"source_timestamp"`
}

func NormalizeSymbol(raw string) (string, error) {
	value := strings.ToUpper(strings.TrimSpace(raw))
	value = strings.TrimPrefix(value, "SH")
	value = strings.TrimPrefix(value, "SZ")
	if strings.HasSuffix(value, ".SS") {
		value = strings.TrimSuffix(value, ".SS") + ".SH"
	}
	if !strings.Contains(value, ".") {
		if len(value) != 6 {
			return "", fmt.Errorf("invalid symbol %q", raw)
		}
		if strings.HasPrefix(value, "5") {
			value += ".SH"
		} else if strings.HasPrefix(value, "1") {
			value += ".SZ"
		} else {
			return "", fmt.Errorf("symbol %q is not a supported Shanghai/Shenzhen ETF code", raw)
		}
	}
	parts := strings.Split(value, ".")
	if len(parts) != 2 || len(parts[0]) != 6 || (parts[1] != "SH" && parts[1] != "SZ") {
		return "", fmt.Errorf("invalid symbol %q", raw)
	}
	if _, err := strconv.Atoi(parts[0]); err != nil {
		return "", fmt.Errorf("invalid symbol %q", raw)
	}
	return value, nil
}

func secID(symbol string) string {
	if strings.HasSuffix(symbol, ".SH") {
		return "1." + strings.TrimSuffix(symbol, ".SH")
	}
	return "0." + strings.TrimSuffix(symbol, ".SZ")
}

func (c *Client) getJSON(ctx context.Context, endpoint string, values url.Values, target any) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	query := u.Query()
	for key, items := range values {
		for _, item := range items {
			query.Add(key, item)
		}
	}
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "go-stock-market-data-export/1")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("provider returned HTTP %d", resp.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 16<<20))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode provider response: %w", err)
	}
	return nil
}

func finiteFloat(value any, field string) (float64, error) {
	var number float64
	switch typed := value.(type) {
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, fmt.Errorf("%s: %w", field, err)
		}
		number = parsed
	case float64:
		number = typed
	case string:
		parsed, err := strconv.ParseFloat(typed, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", field, err)
		}
		number = parsed
	default:
		return 0, fmt.Errorf("%s is not numeric", field)
	}
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, fmt.Errorf("%s is not finite", field)
	}
	return number, nil
}

func (c *Client) fetchQuotes(ctx context.Context, req Request, observed time.Time) (any, []PartialError) {
	records := []any{}
	errs := []PartialError{}
	for _, raw := range req.Symbols {
		symbol, _ := NormalizeSymbol(raw)
		var payload struct {
			RC   int            `json:"rc"`
			Data map[string]any `json:"data"`
		}
		values := url.Values{"secid": {secID(symbol)}, "fltt": {"2"}, "invt": {"2"}, "fields": {"f57,f58,f43,f44,f45,f46,f47,f48,f60,f19,f20,f39,f40,f124"}}
		if err := c.getJSON(ctx, c.Endpoints.Quote, values, &payload); err != nil {
			errs = append(errs, operationError("quotes", symbol, "provider_failed", err, true))
			continue
		}
		if payload.RC != 0 || payload.Data == nil {
			errs = append(errs, operationError("quotes", symbol, "empty_provider_data", errors.New("quote data missing"), true))
			continue
		}
		price, err := finiteFloat(payload.Data["f43"], "price")
		if err != nil || price <= 0 {
			errs = append(errs, operationError("quotes", symbol, "invalid_price", fmt.Errorf("%v", err), false))
			continue
		}
		timestampValue, timestampErr := finiteFloat(payload.Data["f124"], "source timestamp")
		sourceTime := observed
		timestampQuality := "observed_at_fallback"
		if timestampErr == nil && timestampValue > 0 {
			sourceTime = time.Unix(int64(timestampValue), 0).UTC()
			timestampQuality = "provider"
		}
		if sourceTime.After(observed.Add(5 * time.Minute)) {
			errs = append(errs, operationError("quotes", symbol, "future_timestamp", fmt.Errorf("source timestamp %s is in the future", sourceTime.Format(time.RFC3339)), false))
			continue
		}
		num := func(field string) any {
			value, e := finiteFloat(payload.Data[field], field)
			if e != nil {
				return nil
			}
			return value
		}
		records = append(records, map[string]any{
			"symbol": symbol, "name": payload.Data["f58"], "price": price,
			"open": num("f46"), "high": num("f44"), "low": num("f45"), "previous_close": num("f60"),
			"volume_hands": num("f47"), "amount_cny": num("f48"),
			"bid1_price": num("f19"), "bid1_volume_hands": num("f20"), "ask1_price": num("f39"), "ask1_volume_hands": num("f40"),
			"provider": provider, "provenance": "go-stock:readonly-export/quotes",
			"unit":              map[string]string{"price": "CNY_per_share", "volume": "hand_100_shares", "amount": "CNY"},
			"source_timestamp":  sourceTime.Format(time.RFC3339),
			"timestamp_quality": timestampQuality,
		})
	}
	return records, errs
}

func (c *Client) fetchBars(ctx context.Context, req Request, op Operation, observed time.Time) (any, []PartialError) {
	result := map[string]any{}
	errs := []PartialError{}
	limit := op.Limit
	if limit == 0 {
		if op.Timeframe == "5m" {
			limit = 240
		} else {
			limit = 180
		}
	}
	asOf, _ := time.Parse(time.RFC3339, req.AsOf)
	location, _ := time.LoadLocation(req.Timezone)
	for _, raw := range req.Symbols {
		symbol, _ := NormalizeSymbol(raw)
		klt := "101"
		if op.Timeframe == "5m" {
			klt = "5"
		}
		var payload struct {
			RC   int `json:"rc"`
			Data *struct {
				KLines []string `json:"klines"`
			} `json:"data"`
		}
		values := url.Values{"secid": {secID(symbol)}, "klt": {klt}, "fqt": {"0"}, "end": {"20500101"}, "lmt": {strconv.Itoa(limit)}, "fields1": {"f1,f2,f3,f4,f5,f6"}, "fields2": {"f51,f52,f53,f54,f55,f56,f57,f58,f59,f60,f61"}}
		if err := c.getJSON(ctx, c.Endpoints.Bars, values, &payload); err != nil {
			errs = append(errs, operationError("bars", symbol, "provider_failed", err, true))
			continue
		}
		if payload.RC != 0 || payload.Data == nil {
			errs = append(errs, operationError("bars", symbol, "empty_provider_data", errors.New("bar data missing"), true))
			continue
		}
		bars := []any{}
		seen := map[string]bool{}
		previous := time.Time{}
		invalid := false
		for _, line := range payload.Data.KLines {
			parts := strings.Split(line, ",")
			if len(parts) < 7 {
				invalid = true
				errs = append(errs, operationError("bars", symbol, "invalid_bar", fmt.Errorf("expected at least 7 fields"), false))
				break
			}
			layout := "2006-01-02 15:04"
			stampText := parts[0]
			if op.Timeframe == "1d" {
				layout = "2006-01-02 15:04"
				stampText += " 15:00"
			}
			stamp, err := time.ParseInLocation(layout, stampText, location)
			if err != nil || seen[stampText] || (!previous.IsZero() && !stamp.After(previous)) {
				invalid = true
				errs = append(errs, operationError("bars", symbol, "bar_time_order", fmt.Errorf("duplicate or unordered bar %q", parts[0]), false))
				break
			}
			seen[stampText] = true
			previous = stamp
			if stamp.After(asOf) {
				continue
			}
			values := make([]float64, 6)
			for index := 0; index < 6; index++ {
				values[index], err = finiteFloat(parts[index+1], "bar value")
				if err != nil {
					break
				}
			}
			if err != nil || values[0] <= 0 || values[1] <= 0 || values[2] <= 0 || values[3] <= 0 || values[4] < 0 || values[5] < 0 || values[2] < values[3] || values[3] > math.Min(values[0], values[1]) || values[2] < math.Max(values[0], values[1]) {
				invalid = true
				errs = append(errs, operationError("bars", symbol, "invalid_bar_value", fmt.Errorf("invalid OHLCV at %s", parts[0]), false))
				break
			}
			bars = append(bars, map[string]any{"symbol": symbol, "timeframe": op.Timeframe, "timestamp": stamp.UTC().Format(time.RFC3339), "open": values[0], "close": values[1], "high": values[2], "low": values[3], "volume_hands": values[4], "amount_cny": values[5], "provider": provider, "provenance": "go-stock:readonly-export/bars", "unit": map[string]string{"price": "CNY_per_share", "volume": "hand_100_shares", "amount": "CNY"}, "source_timestamp": stamp.UTC().Format(time.RFC3339)})
		}
		if !invalid {
			result[symbol] = bars
		}
	}
	if len(result) == 0 {
		return nil, errs
	}
	return result, errs
}

type fundSuggestion struct {
	Code         string         `json:"CODE"`
	Name         string         `json:"NAME"`
	Category     int            `json:"CATEGORY"`
	FundBaseInfo map[string]any `json:"FundBaseInfo"`
	StockHolder  map[string]any `json:"StockHolder"`
}

func (c *Client) suggestFunds(ctx context.Context, keyword string) ([]fundSuggestion, error) {
	var payload struct {
		ErrCode int              `json:"ErrCode"`
		Datas   []fundSuggestion `json:"Datas"`
	}
	if err := c.getJSON(ctx, c.Endpoints.FundSuggest, url.Values{"m": {"1"}, "key": {keyword}}, &payload); err != nil {
		return nil, err
	}
	if payload.ErrCode != 0 {
		return nil, fmt.Errorf("fund provider error %d", payload.ErrCode)
	}
	return payload.Datas, nil
}

func (c *Client) fetchUniverse(ctx context.Context, op Operation, observed time.Time) (any, []PartialError) {
	limit := op.Limit
	if limit == 0 {
		limit = 100
	}
	if limit > 100 {
		limit = 100
	}
	type response struct {
		key   string
		funds []fundSuggestion
		err   error
	}
	ch := make(chan response, len(universeKeywords))
	var wg sync.WaitGroup
	for _, keyword := range universeKeywords {
		keyword := keyword
		wg.Add(1)
		go func() {
			defer wg.Done()
			funds, err := c.suggestFunds(ctx, keyword)
			ch <- response{keyword, funds, err}
		}()
	}
	wg.Wait()
	close(ch)
	unique := map[string]map[string]any{}
	errs := []PartialError{}
	for item := range ch {
		if item.err != nil {
			errs = append(errs, operationError("etf_universe", "", "provider_failed", fmt.Errorf("keyword %s: %w", item.key, item.err), true))
			continue
		}
		for _, fund := range item.funds {
			if !strings.Contains(strings.ToUpper(fund.Name), "ETF") || len(fund.Code) != 6 {
				continue
			}
			symbol, err := NormalizeSymbol(fund.Code)
			if err != nil {
				continue
			}
			exchange := map[string]string{"SH": "SSE", "SZ": "SZSE"}[strings.Split(symbol, ".")[1]]
			unique[symbol] = map[string]any{"symbol": symbol, "name": fund.Name, "security_type": "stock_etf", "exchange": exchange, "fund_type": fund.FundBaseInfo["FTYPE"], "fund_company": fund.FundBaseInfo["JJGS"], "tracking_index": nil, "listing_date": nil, "provider": provider, "provenance": "go-stock:readonly-export/etf_universe", "unit": map[string]string{}, "source_timestamp": observed.Format(time.RFC3339)}
		}
	}
	keys := make([]string, 0, len(unique))
	for key := range unique {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	records := []any{}
	for _, key := range keys {
		records = append(records, unique[key])
		if len(records) == limit {
			break
		}
	}
	if len(records) == 0 && len(errs) == 0 {
		errs = append(errs, operationError("etf_universe", "", "empty_provider_data", errors.New("no ETF records returned"), true))
	}
	return records, errs
}

func (c *Client) fetchProfiles(ctx context.Context, req Request, observed time.Time) (any, []PartialError) {
	records := []any{}
	errs := []PartialError{}
	for _, raw := range req.Symbols {
		symbol, _ := NormalizeSymbol(raw)
		code := strings.Split(symbol, ".")[0]
		funds, err := c.suggestFunds(ctx, code)
		if err != nil {
			errs = append(errs, operationError("etf_profile", symbol, "provider_failed", err, true))
			continue
		}
		var match *fundSuggestion
		for index := range funds {
			if funds[index].Code == code {
				match = &funds[index]
				break
			}
		}
		if match == nil {
			errs = append(errs, operationError("etf_profile", symbol, "not_found", errors.New("ETF profile not found"), false))
			continue
		}
		records = append(records, map[string]any{"symbol": symbol, "name": match.Name, "fund_type": match.FundBaseInfo["FTYPE"], "fund_company": match.FundBaseInfo["JJGS"], "tracking_index": nil, "assets_cny": nil, "management_fee_rate": nil, "custody_fee_rate": nil, "missing_fields": []string{"tracking_index", "assets_cny", "management_fee_rate", "custody_fee_rate", "listing_date"}, "provider": provider, "provenance": "go-stock:readonly-export/etf_profile", "unit": map[string]string{"assets": "CNY", "fee_rate": "fraction"}, "source_timestamp": observed.Format(time.RFC3339)})
	}
	return records, errs
}

func (c *Client) fetchEvents(ctx context.Context, req Request, observed time.Time) (any, []PartialError) {
	if len(req.Symbols) == 0 {
		return []any{}, nil
	}
	codes := make([]string, 0, len(req.Symbols))
	for _, raw := range req.Symbols {
		symbol, _ := NormalizeSymbol(raw)
		codes = append(codes, strings.Split(symbol, ".")[0])
	}
	var payload struct {
		Data *struct {
			List []map[string]any `json:"list"`
		} `json:"data"`
	}
	values := url.Values{"page_size": {"50"}, "page_index": {"1"}, "ann_type": {"SHA,CYB,SZA,BJA,INV"}, "client_source": {"web"}, "f_node": {"0"}, "stock_list": {strings.Join(codes, ",")}}
	if err := c.getJSON(ctx, c.Endpoints.Events, values, &payload); err != nil {
		return nil, []PartialError{operationError("events", "", "provider_failed", err, true)}
	}
	if payload.Data == nil {
		return nil, []PartialError{operationError("events", "", "empty_provider_data", errors.New("events data missing"), true)}
	}
	records := []any{}
	errs := []PartialError{}
	location, _ := time.LoadLocation(req.Timezone)
	asOf, _ := time.Parse(time.RFC3339, req.AsOf)
	requested := map[string]string{}
	for _, raw := range req.Symbols {
		symbol, _ := NormalizeSymbol(raw)
		requested[strings.Split(symbol, ".")[0]] = symbol
	}
	for _, item := range payload.Data.List {
		title, _ := item["title"].(string)
		noticeDate, _ := item["notice_date"].(string)
		if title == "" || noticeDate == "" {
			continue
		}
		stamp, err := time.ParseInLocation("2006-01-02 15:04:05", noticeDate, location)
		if err != nil {
			if parsed, e := time.Parse(time.RFC3339, noticeDate); e == nil {
				stamp = parsed
			} else {
				continue
			}
		}
		if stamp.After(asOf) {
			continue
		}
		symbols := eventSymbols(item, requested)
		if len(symbols) == 0 && len(requested) == 1 {
			for _, symbol := range requested {
				symbols = append(symbols, symbol)
			}
		}
		if len(symbols) == 0 {
			errs = append(errs, operationError("events", "", "event_symbol_unresolved", fmt.Errorf("cannot attribute event %q to a requested ETF", title), false))
			continue
		}
		records = append(records, map[string]any{"symbols": symbols, "title": title, "event_kind": "announcement", "published_at": stamp.UTC().Format(time.RFC3339), "raw": item, "provider": provider, "provenance": "go-stock:readonly-export/events", "unit": map[string]string{}, "source_timestamp": stamp.UTC().Format(time.RFC3339)})
	}
	return records, errs
}

func eventSymbols(value any, requested map[string]string) []string {
	found := map[string]bool{}
	var visit func(any)
	visit = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			for _, child := range typed {
				visit(child)
			}
		case []any:
			for _, child := range typed {
				visit(child)
			}
		case string:
			candidate := strings.TrimSpace(typed)
			if symbol, ok := requested[candidate]; ok {
				found[symbol] = true
			}
		}
	}
	visit(value)
	result := make([]string, 0, len(found))
	for symbol := range found {
		result = append(result, symbol)
	}
	sort.Strings(result)
	return result
}

func (c *Client) buildCalendar(req Request, op Operation, observed time.Time) (any, []PartialError) {
	location, _ := time.LoadLocation(req.Timezone)
	asOf, _ := time.Parse(time.RFC3339, req.AsOf)
	start := asOf.In(location).AddDate(0, 0, -30)
	end := asOf.In(location).AddDate(0, 0, 10)
	if op.Start != "" {
		parsed, err := time.Parse("2006-01-02", op.Start)
		if err != nil {
			return nil, []PartialError{operationError("trading_calendar", "", "invalid_start", err, false)}
		}
		start = parsed.In(location)
	}
	if op.End != "" {
		parsed, err := time.Parse("2006-01-02", op.End)
		if err != nil {
			return nil, []PartialError{operationError("trading_calendar", "", "invalid_end", err, false)}
		}
		end = parsed.In(location)
	}
	dates := []any{}
	for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
		weekday := day.Weekday()
		dates = append(dates, map[string]any{"date": day.Format("2006-01-02"), "is_trading_day": weekday != time.Saturday && weekday != time.Sunday, "authoritative": false})
	}
	return map[string]any{"timezone": req.Timezone, "calendar_kind": "weekday_fallback", "research_only": true, "sessions": []map[string]string{{"name": "opening_auction", "start": "09:15", "end": "09:25"}, {"name": "continuous_am", "start": "09:30", "end": "11:30"}, {"name": "continuous_pm", "start": "13:00", "end": "14:57"}, {"name": "closing_auction", "start": "14:57", "end": "15:00"}}, "dates": dates, "provider": "go-stock_session_rules_v1", "provenance": "go-stock:readonly-export/trading_calendar", "unit": map[string]string{}, "source_timestamp": observed.Format(time.RFC3339)}, nil
}
