package marketdataexport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

const (
	providerTencent = "tencent_public_web"
	providerSina    = "sina_public_web"
	providerEast    = "eastmoney_public_web"
	providerDerived = "go_stock_deterministic_derived"
)

type providerAttempt struct {
	Provider string `json:"provider"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
}

func tickerSymbol(symbol string) string {
	code := strings.Split(symbol, ".")[0]
	if strings.HasSuffix(symbol, ".SH") {
		return "sh" + code
	}
	return "sz" + code
}

func endpointWithTicker(endpoint, ticker string) string {
	if strings.Contains(endpoint, "%s") {
		return fmt.Sprintf(endpoint, ticker)
	}
	return endpoint + ticker
}

func (c *Client) getBytes(ctx context.Context, endpoint string, values url.Values, headers map[string]string) ([]byte, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	req.Header.Set("User-Agent", "go-stock-market-data-export/2")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("provider returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, errors.New("empty provider response")
	}
	return data, nil
}

func decodeGB18030(data []byte) (string, error) {
	decoded, err := io.ReadAll(transform.NewReader(bytes.NewReader(data), simplifiedchinese.GB18030.NewDecoder()))
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

func quotedProviderFields(body string) ([]string, error) {
	start := strings.Index(body, "\"")
	end := strings.LastIndex(body, "\"")
	if start < 0 || end <= start {
		return nil, errors.New("quoted quote payload missing")
	}
	fields := strings.Split(body[start+1:end], ",")
	if len(fields) < 10 {
		return nil, fmt.Errorf("quote payload has %d fields", len(fields))
	}
	return fields, nil
}

func numberAt(fields []string, index int) (float64, error) {
	if index < 0 || index >= len(fields) {
		return 0, fmt.Errorf("field %d missing", index)
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(fields[index]), 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
		return 0, fmt.Errorf("field %d is not finite", index)
	}
	return value, nil
}

func optionalNumberAt(fields []string, index int, multiplier float64) any {
	value, err := numberAt(fields, index)
	if err != nil {
		return nil
	}
	return value * multiplier
}

func sourceTimeFromLocal(text, layout string, observed time.Time) (time.Time, string) {
	location, _ := time.LoadLocation("Asia/Shanghai")
	parsed, err := time.ParseInLocation(layout, strings.TrimSpace(text), location)
	if err != nil {
		return observed, "observed_at_fallback"
	}
	return parsed.UTC(), "provider"
}

func (c *Client) quoteTencent(ctx context.Context, symbol string, observed time.Time) (map[string]any, error) {
	body, err := c.getBytes(ctx, endpointWithTicker(c.Endpoints.QuoteTencent, tickerSymbol(symbol)), nil, map[string]string{"Referer": "https://gu.qq.com/"})
	if err != nil {
		return nil, err
	}
	text, err := decodeGB18030(body)
	if err != nil {
		return nil, err
	}
	start := strings.Index(text, "\"")
	end := strings.LastIndex(text, "\"")
	if start < 0 || end <= start {
		return nil, errors.New("Tencent quote payload missing")
	}
	fields := strings.Split(text[start+1:end], "~")
	if len(fields) < 38 {
		return nil, fmt.Errorf("Tencent quote payload has %d fields", len(fields))
	}
	price, err := numberAt(fields, 3)
	if err != nil || price <= 0 {
		return nil, errors.New("Tencent quote price invalid")
	}
	sourceTime, quality := sourceTimeFromLocal(fields[30], "20060102150405", observed)
	return map[string]any{
		"symbol": symbol, "name": fields[1], "price": price,
		"open": optionalNumberAt(fields, 5, 1), "high": optionalNumberAt(fields, 33, 1), "low": optionalNumberAt(fields, 34, 1), "previous_close": optionalNumberAt(fields, 4, 1),
		"volume_hands": optionalNumberAt(fields, 36, 1), "amount_cny": optionalNumberAt(fields, 37, 10000),
		"bid1_price": optionalNumberAt(fields, 9, 1), "bid1_volume_hands": optionalNumberAt(fields, 10, 1), "ask1_price": optionalNumberAt(fields, 19, 1), "ask1_volume_hands": optionalNumberAt(fields, 20, 1),
		"provider": providerTencent, "provenance": "go-stock:readonly-export-v2/quotes/tencent",
		"unit":             map[string]string{"price": "CNY_per_share", "volume": "hand_100_shares", "amount": "CNY"},
		"source_timestamp": sourceTime.Format(time.RFC3339), "timestamp_quality": quality,
	}, nil
}

func (c *Client) quoteSina(ctx context.Context, symbol string, observed time.Time) (map[string]any, error) {
	body, err := c.getBytes(ctx, endpointWithTicker(c.Endpoints.QuoteSina, tickerSymbol(symbol)), nil, map[string]string{"Referer": "https://finance.sina.com.cn/"})
	if err != nil {
		return nil, err
	}
	text, err := decodeGB18030(body)
	if err != nil {
		return nil, err
	}
	fields, err := quotedProviderFields(text)
	if err != nil || len(fields) < 32 {
		return nil, fmt.Errorf("Sina quote invalid: %w", err)
	}
	price, err := numberAt(fields, 3)
	if err != nil || price <= 0 {
		return nil, errors.New("Sina quote price invalid")
	}
	sourceTime, quality := sourceTimeFromLocal(fields[30]+" "+fields[31], "2006-01-02 15:04:05", observed)
	return map[string]any{
		"symbol": symbol, "name": fields[0], "price": price,
		"open": optionalNumberAt(fields, 1, 1), "high": optionalNumberAt(fields, 4, 1), "low": optionalNumberAt(fields, 5, 1), "previous_close": optionalNumberAt(fields, 2, 1),
		"volume_hands": optionalNumberAt(fields, 8, 0.01), "amount_cny": optionalNumberAt(fields, 9, 1),
		"bid1_price": optionalNumberAt(fields, 11, 1), "bid1_volume_hands": optionalNumberAt(fields, 10, 0.01), "ask1_price": optionalNumberAt(fields, 21, 1), "ask1_volume_hands": optionalNumberAt(fields, 20, 0.01),
		"provider": providerSina, "provenance": "go-stock:readonly-export-v2/quotes/sina",
		"unit":             map[string]string{"price": "CNY_per_share", "volume": "hand_100_shares", "amount": "CNY"},
		"source_timestamp": sourceTime.Format(time.RFC3339), "timestamp_quality": quality,
	}, nil
}

func (c *Client) quoteEastmoney(ctx context.Context, req Request, symbol string, observed time.Time) (map[string]any, error) {
	single := req
	single.Symbols = []string{symbol}
	result, errs := c.fetchQuotes(ctx, single, observed)
	if len(errs) > 0 {
		return nil, errors.New(errs[0].Message)
	}
	records, ok := result.([]any)
	if !ok || len(records) != 1 {
		return nil, errors.New("EastMoney quote record missing")
	}
	record, ok := records[0].(map[string]any)
	if !ok {
		return nil, errors.New("EastMoney quote record invalid")
	}
	record["provenance"] = "go-stock:readonly-export-v2/quotes/eastmoney"
	return record, nil
}

func (c *Client) fetchQuotesV2(ctx context.Context, req Request, observed time.Time) (any, []PartialError) {
	records := []any{}
	errs := []PartialError{}
	for _, raw := range req.Symbols {
		symbol, _ := NormalizeSymbol(raw)
		chain := []providerAttempt{}
		var selected map[string]any
		var failures []string
		attempts := []struct {
			name string
			call func() (map[string]any, error)
		}{
			{providerTencent, func() (map[string]any, error) { return c.quoteTencent(ctx, symbol, observed) }},
			{providerSina, func() (map[string]any, error) { return c.quoteSina(ctx, symbol, observed) }},
			{providerEast, func() (map[string]any, error) { return c.quoteEastmoney(ctx, req, symbol, observed) }},
		}
		for _, attempt := range attempts {
			record, err := attempt.call()
			recordProvider(ctx, attempt.name, "quotes", err)
			if err != nil {
				chain = append(chain, providerAttempt{Provider: attempt.name, Status: "failed", Reason: err.Error()})
				failures = append(failures, attempt.name+": "+err.Error())
				continue
			}
			chain = append(chain, providerAttempt{Provider: attempt.name, Status: "selected"})
			selected = record
			break
		}
		if selected == nil {
			errs = append(errs, operationError("quotes", symbol, "all_providers_failed", errors.New(strings.Join(failures, "; ")), true))
			continue
		}
		selected["provider_chain"] = chain
		if len(failures) > 0 {
			selected["fallback_reason"] = strings.Join(failures, "; ")
		}
		records = append(records, selected)
	}
	if len(records) == 0 {
		return nil, errs
	}
	return records, errs
}

type rawBarV2 struct {
	Timestamp string
	Open      float64
	Close     float64
	High      float64
	Low       float64
	Volume    float64
	Amount    float64
	Estimated bool
}

func makeBarsV2(symbol, timeframe, providerName, provenanceName string, raw []rawBarV2, req Request) ([]any, error) {
	location, _ := time.LoadLocation(req.Timezone)
	asOf, _ := time.Parse(time.RFC3339, req.AsOf)
	result := []any{}
	seen := map[string]bool{}
	previous := time.Time{}
	for _, item := range raw {
		layout := "2006-01-02 15:04"
		stampText := item.Timestamp
		if timeframe == "1d" && len(stampText) == 10 {
			stampText += " 15:00"
		} else if len(stampText) == len("2006-01-02 15:04:05") {
			layout = "2006-01-02 15:04:05"
		}
		stamp, err := time.ParseInLocation(layout, stampText, location)
		if err != nil || seen[stampText] || (!previous.IsZero() && !stamp.After(previous)) {
			return nil, fmt.Errorf("duplicate or unordered bar %q", item.Timestamp)
		}
		seen[stampText] = true
		previous = stamp
		if stamp.After(asOf) {
			continue
		}
		values := []float64{item.Open, item.Close, item.High, item.Low, item.Volume, item.Amount}
		for _, value := range values {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return nil, fmt.Errorf("non-finite bar at %s", item.Timestamp)
			}
		}
		if item.Open <= 0 || item.Close <= 0 || item.High <= 0 || item.Low <= 0 || item.Volume < 0 || item.Amount < 0 || item.High < math.Max(item.Open, item.Close) || item.Low > math.Min(item.Open, item.Close) || item.High < item.Low {
			return nil, fmt.Errorf("invalid OHLCV at %s", item.Timestamp)
		}
		record := map[string]any{
			"symbol": symbol, "timeframe": timeframe, "timestamp": stamp.UTC().Format(time.RFC3339),
			"open": item.Open, "close": item.Close, "high": item.High, "low": item.Low,
			"volume_hands": item.Volume, "amount_cny": item.Amount,
			"provider": providerName, "provenance": provenanceName,
			"unit":             map[string]string{"price": "CNY_per_share", "volume": "hand_100_shares", "amount": "CNY"},
			"source_timestamp": stamp.UTC().Format(time.RFC3339),
		}
		if item.Estimated {
			record["amount_quality"] = "estimated_from_close_and_volume"
		}
		result = append(result, record)
	}
	if len(result) == 0 {
		return nil, errors.New("no bars at or before as_of")
	}
	return result, nil
}

func (c *Client) barsSina(ctx context.Context, req Request, op Operation, symbol string) ([]any, error) {
	limit := op.Limit
	if limit == 0 {
		limit = 240
	}
	scale := "5"
	if op.Timeframe == "1d" {
		scale = "240"
	}
	body, err := c.getBytes(ctx, c.Endpoints.BarsSina, url.Values{"symbol": {tickerSymbol(symbol)}, "scale": {scale}, "ma": {"no"}, "datalen": {strconv.Itoa(limit)}}, map[string]string{"Referer": "https://finance.sina.com.cn/"})
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(string(body))
	start := strings.Index(text, "[")
	end := strings.LastIndex(text, "]")
	if start < 0 || end < start {
		return nil, errors.New("Sina bars JSONP array missing")
	}
	var items []struct {
		Day, Open, High, Low, Close, Volume, Amount string
	}
	if err := json.Unmarshal([]byte(text[start:end+1]), &items); err != nil {
		return nil, fmt.Errorf("decode Sina bars: %w", err)
	}
	raw := make([]rawBarV2, 0, len(items))
	for _, item := range items {
		open, e1 := strconv.ParseFloat(item.Open, 64)
		closeValue, e2 := strconv.ParseFloat(item.Close, 64)
		high, e3 := strconv.ParseFloat(item.High, 64)
		low, e4 := strconv.ParseFloat(item.Low, 64)
		volumeShares, e5 := strconv.ParseFloat(item.Volume, 64)
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil {
			return nil, fmt.Errorf("invalid Sina bar %q", item.Day)
		}
		amount, amountErr := strconv.ParseFloat(item.Amount, 64)
		estimated := amountErr != nil || amount < 0
		if estimated {
			amount = closeValue * volumeShares
		}
		raw = append(raw, rawBarV2{Timestamp: item.Day, Open: open, Close: closeValue, High: high, Low: low, Volume: volumeShares / 100, Amount: amount, Estimated: estimated})
	}
	return makeBarsV2(symbol, op.Timeframe, providerSina, "go-stock:readonly-export-v2/bars/sina", raw, req)
}

func (c *Client) barsEastmoney(ctx context.Context, req Request, op Operation, symbol string, observed time.Time) ([]any, error) {
	single := req
	single.Symbols = []string{symbol}
	result, errs := c.fetchBars(ctx, single, op, observed)
	if len(errs) > 0 {
		return nil, errors.New(errs[0].Message)
	}
	bySymbol, ok := result.(map[string]any)
	if !ok {
		return nil, errors.New("EastMoney bars result invalid")
	}
	records, ok := bySymbol[symbol].([]any)
	if !ok || len(records) == 0 {
		return nil, errors.New("EastMoney bars missing")
	}
	for _, item := range records {
		if record, ok := item.(map[string]any); ok {
			record["provenance"] = "go-stock:readonly-export-v2/bars/eastmoney"
		}
	}
	return records, nil
}

func (c *Client) barsTencent(ctx context.Context, req Request, op Operation, symbol string) ([]any, error) {
	limit := op.Limit
	if limit == 0 {
		limit = 240
	}
	period := "day"
	if op.Timeframe == "5m" {
		period = "m5"
	}
	param := fmt.Sprintf("%s,%s,,,%d,qfq", tickerSymbol(symbol), period, limit)
	body, err := c.getBytes(ctx, c.Endpoints.BarsTencent, url.Values{"param": {param}}, map[string]string{"Referer": "https://gu.qq.com/"})
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}
	data, _ := payload["data"].(map[string]any)
	stock, _ := data[tickerSymbol(symbol)].(map[string]any)
	items, _ := stock[period].([]any)
	if len(items) == 0 && period == "day" {
		items, _ = stock["qfqday"].([]any)
	}
	if len(items) == 0 {
		return nil, errors.New("Tencent bars missing")
	}
	raw := []rawBarV2{}
	for _, value := range items {
		fields, ok := value.([]any)
		if !ok || len(fields) < 6 {
			return nil, errors.New("Tencent bar fields missing")
		}
		asString := func(index int) string { return fmt.Sprint(fields[index]) }
		open, e1 := strconv.ParseFloat(asString(1), 64)
		closeValue, e2 := strconv.ParseFloat(asString(2), 64)
		high, e3 := strconv.ParseFloat(asString(3), 64)
		low, e4 := strconv.ParseFloat(asString(4), 64)
		volumeHands, e5 := strconv.ParseFloat(asString(5), 64)
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil {
			return nil, fmt.Errorf("invalid Tencent bar %q", asString(0))
		}
		stamp := asString(0)
		if len(stamp) == 12 && !strings.Contains(stamp, "-") {
			if parsed, parseErr := time.Parse("200601021504", stamp); parseErr == nil {
				stamp = parsed.Format("2006-01-02 15:04")
			}
		}
		raw = append(raw, rawBarV2{Timestamp: stamp, Open: open, Close: closeValue, High: high, Low: low, Volume: volumeHands, Amount: closeValue * volumeHands * 100, Estimated: true})
	}
	return makeBarsV2(symbol, op.Timeframe, providerTencent, "go-stock:readonly-export-v2/bars/tencent", raw, req)
}

func (c *Client) fetchBarsV2(ctx context.Context, req Request, op Operation, observed time.Time) (any, []PartialError) {
	result := map[string]any{}
	errs := []PartialError{}
	for _, rawSymbol := range req.Symbols {
		symbol, _ := NormalizeSymbol(rawSymbol)
		chain := []providerAttempt{}
		failures := []string{}
		var selected []any
		attempts := []struct {
			name string
			call func() ([]any, error)
		}{
			{providerSina, func() ([]any, error) { return c.barsSina(ctx, req, op, symbol) }},
			{providerEast, func() ([]any, error) { return c.barsEastmoney(ctx, req, op, symbol, observed) }},
			{providerTencent, func() ([]any, error) { return c.barsTencent(ctx, req, op, symbol) }},
		}
		for _, attempt := range attempts {
			records, err := attempt.call()
			recordProvider(ctx, attempt.name, "bars/"+op.Timeframe, err)
			if err != nil {
				chain = append(chain, providerAttempt{Provider: attempt.name, Status: "failed", Reason: err.Error()})
				failures = append(failures, attempt.name+": "+err.Error())
				continue
			}
			chain = append(chain, providerAttempt{Provider: attempt.name, Status: "selected"})
			selected = records
			break
		}
		if selected == nil {
			errs = append(errs, operationError("bars", symbol, "all_providers_failed", errors.New(strings.Join(failures, "; ")), true))
			continue
		}
		for _, item := range selected {
			if record, ok := item.(map[string]any); ok {
				record["provider_chain"] = chain
				if len(failures) > 0 {
					record["fallback_reason"] = strings.Join(failures, "; ")
				}
			}
		}
		result[symbol] = selected
	}
	if len(result) == 0 {
		return nil, errs
	}
	return result, errs
}

// fetchBarsHistoryV3 exposes a point-in-time history window without changing
// the side-effect-free execution boundary. Each returned symbol page uses one
// provider; consumers must reject incomplete coverage rather than splice
// providers together.
func (c *Client) fetchBarsHistoryV3(ctx context.Context, req Request, op Operation, observed time.Time) (any, []PartialError) {
	query := op
	query.Name = "bars"
	if query.Limit == 0 {
		query.Limit = 10000
	}
	result, errs := c.fetchBarsV2(ctx, req, query, observed)
	grouped, ok := result.(map[string]any)
	if !ok {
		return nil, errs
	}
	location, _ := time.LoadLocation(req.Timezone)
	start, _ := time.ParseInLocation("2006-01-02", op.Start, location)
	endDay, _ := time.ParseInLocation("2006-01-02", op.End, location)
	end := endDay.Add(24*time.Hour - time.Nanosecond)
	var cursor time.Time
	if op.Cursor != "" {
		cursor, _ = time.Parse(time.RFC3339, op.Cursor)
	}
	response := map[string]any{}
	for symbol, raw := range grouped {
		records, _ := raw.([]any)
		filtered := make([]any, 0, len(records))
		for _, item := range records {
			record, recordOK := item.(map[string]any)
			if !recordOK {
				continue
			}
			stamp, err := time.Parse(time.RFC3339, fmt.Sprint(record["timestamp"]))
			if err != nil || stamp.Before(start) || stamp.After(end) || (!cursor.IsZero() && !stamp.Before(cursor)) {
				continue
			}
			filtered = append(filtered, record)
		}
		nextCursor := ""
		complete := len(filtered) < query.Limit
		if len(filtered) == query.Limit {
			if first, firstOK := filtered[0].(map[string]any); firstOK {
				nextCursor = fmt.Sprint(first["timestamp"])
			}
		}
		provider := ""
		if len(filtered) > 0 {
			if first, firstOK := filtered[0].(map[string]any); firstOK {
				provider = fmt.Sprint(first["provider"])
			}
		}
		response[symbol] = map[string]any{
			"records":     filtered,
			"next_cursor": nextCursor,
			"complete":    complete,
			"provider":    provider,
			"start":       op.Start,
			"end":         op.End,
		}
	}
	if len(response) == 0 {
		return nil, errs
	}
	return response, errs
}

func compactText(value string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(value, "\u00a0", " ")), "")
}

func parseDateText(value string) string {
	for _, layout := range []string{"2006-01-02", "2006年01月02日", "2006/01/02"} {
		if parsed, err := time.Parse(layout, strings.TrimSpace(value)); err == nil {
			return parsed.Format("2006-01-02")
		}
	}
	return ""
}

func parseAssetsCNY(value string) any {
	re := regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)(亿元|万元|元)`)
	match := re.FindStringSubmatch(compactText(value))
	if len(match) != 3 {
		return nil
	}
	number, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return nil
	}
	switch match[2] {
	case "亿元":
		return number * 1e8
	case "万元":
		return number * 1e4
	default:
		return number
	}
}

func feeRate(text, label string) any {
	re := regexp.MustCompile(label + `[^0-9]{0,12}([0-9]+(?:\.[0-9]+)?)%`)
	match := re.FindStringSubmatch(compactText(text))
	if len(match) != 2 {
		return nil
	}
	number, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return nil
	}
	return number / 100
}

func (c *Client) profileHTML(ctx context.Context, symbol string, observed time.Time) (map[string]any, error) {
	code := strings.Split(symbol, ".")[0]
	endpoint := endpointWithTicker(c.Endpoints.FundProfile, code)
	body, err := c.getBytes(ctx, endpoint, nil, map[string]string{"Accept": "text/html", "Referer": "https://fund.eastmoney.com/"})
	if err != nil {
		return nil, err
	}
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	name := compactText(doc.Find(".merchandiseDetail .fundDetail-tit").First().Text())
	values := map[string]string{}
	doc.Find(".infoOfFund table td").Each(func(_ int, selection *goquery.Selection) {
		text := compactText(selection.Text())
		parts := strings.SplitN(strings.ReplaceAll(text, ":", "："), "：", 2)
		if len(parts) == 2 {
			values[parts[0]] = parts[1]
		}
	})
	lookup := func(labels ...string) string {
		for key, value := range values {
			for _, label := range labels {
				if strings.Contains(key, label) {
					return value
				}
			}
		}
		return ""
	}
	tracking := lookup("跟踪标的", "标的")
	establishment := parseDateText(lookup("成立日期", "成立日"))
	fullText := doc.Text()
	record := map[string]any{
		"symbol": symbol, "name": name, "fund_type": lookup("基金类型", "类型"), "fund_company": lookup("管理人", "基金公司"),
		"tracking_index": nil, "establishment_date": nil, "listing_date": nil,
		"assets_cny":          parseAssetsCNY(lookup("基金规模", "规模")),
		"management_fee_rate": feeRate(fullText, "管理费率"), "custody_fee_rate": feeRate(fullText, "托管费率"),
		"provider": providerEast, "provenance": "go-stock:readonly-export-v2/etf_profile/html",
		"unit": map[string]string{"assets": "CNY", "fee_rate": "fraction"}, "source_timestamp": observed.Format(time.RFC3339),
	}
	if tracking != "" && tracking != "--" {
		record["tracking_index"] = tracking
	}
	if establishment != "" {
		record["establishment_date"] = establishment
	}
	if name == "" && tracking == "" && establishment == "" {
		return nil, errors.New("fund profile fields missing")
	}
	return record, nil
}

func missingProfileFields(record map[string]any) []string {
	fields := []string{"tracking_index", "assets_cny", "management_fee_rate", "custody_fee_rate", "listing_date"}
	missing := []string{}
	for _, field := range fields {
		if value, ok := record[field]; !ok || value == nil || value == "" {
			missing = append(missing, field)
		}
	}
	return missing
}

func (c *Client) fetchProfilesV2(ctx context.Context, req Request, observed time.Time) (any, []PartialError) {
	records := []any{}
	errs := []PartialError{}
	for _, raw := range req.Symbols {
		symbol, _ := NormalizeSymbol(raw)
		chain := []providerAttempt{}
		record, err := c.profileHTML(ctx, symbol, observed)
		recordProvider(ctx, providerEast, "etf_profile/html", err)
		if err == nil {
			chain = append(chain, providerAttempt{Provider: providerEast, Status: "selected"})
		} else {
			chain = append(chain, providerAttempt{Provider: providerEast, Status: "failed", Reason: err.Error()})
			code := strings.Split(symbol, ".")[0]
			funds, fallbackErr := c.suggestFunds(ctx, code)
			recordProvider(ctx, providerEast, "etf_profile/suggest", fallbackErr)
			if fallbackErr != nil {
				errs = append(errs, operationError("etf_profile", symbol, "all_providers_failed", fmt.Errorf("html: %v; suggest: %w", err, fallbackErr), true))
				continue
			}
			for _, fund := range funds {
				if fund.Code == code {
					record = map[string]any{"symbol": symbol, "name": fund.Name, "fund_type": fund.FundBaseInfo["FTYPE"], "fund_company": fund.FundBaseInfo["JJGS"], "tracking_index": nil, "establishment_date": nil, "listing_date": nil, "assets_cny": nil, "management_fee_rate": nil, "custody_fee_rate": nil, "provider": providerEast, "provenance": "go-stock:readonly-export-v2/etf_profile/suggest", "unit": map[string]string{"assets": "CNY", "fee_rate": "fraction"}, "source_timestamp": observed.Format(time.RFC3339)}
					break
				}
			}
			if record == nil {
				errs = append(errs, operationError("etf_profile", symbol, "not_found", errors.New("ETF profile not found"), false))
				continue
			}
			chain = append(chain, providerAttempt{Provider: providerEast, Status: "selected", Reason: "fund suggestion fallback"})
			record["fallback_reason"] = err.Error()
		}
		record["provider_chain"] = chain
		record["missing_fields"] = missingProfileFields(record)
		records = append(records, record)
	}
	if len(records) == 0 {
		return nil, errs
	}
	return records, errs
}

func quoteMaps(result any) []map[string]any {
	values, _ := result.([]any)
	records := make([]map[string]any, 0, len(values))
	for _, value := range values {
		if record, ok := value.(map[string]any); ok {
			records = append(records, record)
		}
	}
	return records
}

func quoteChange(record map[string]any) (float64, bool) {
	price, okPrice := record["price"].(float64)
	previous, okPrevious := record["previous_close"].(float64)
	if !okPrice || !okPrevious || previous <= 0 {
		return 0, false
	}
	return price/previous - 1, true
}

func (c *Client) fetchMarketContextV2(ctx context.Context, req Request, observed time.Time) (any, []PartialError) {
	quotes, errs := c.fetchQuotesV2(ctx, req, observed)
	records := quoteMaps(quotes)
	up, down, flat := 0, 0, 0
	for _, record := range records {
		change, ok := quoteChange(record)
		if !ok || math.Abs(change) < 0.0001 {
			flat++
		} else if change > 0 {
			up++
		} else {
			down++
		}
	}
	recordProvider(ctx, providerDerived, "market_context", nil)
	return map[string]any{"sample_size": len(records), "advance_count": up, "decline_count": down, "flat_or_missing_count": flat, "breadth_ratio": float64(up) / math.Max(1, float64(up+down)), "research_only": true, "provider": providerDerived, "provenance": "go-stock:readonly-export-v2/market_context/quote_breadth", "unit": map[string]string{"breadth_ratio": "fraction"}, "source_timestamp": observed.Format(time.RFC3339)}, errs
}

func (c *Client) fetchFundFlowV2(ctx context.Context, req Request, observed time.Time) (any, []PartialError) {
	quotes, errs := c.fetchQuotesV2(ctx, req, observed)
	records := []any{}
	for _, quote := range quoteMaps(quotes) {
		change, ok := quoteChange(quote)
		amount, amountOK := quote["amount_cny"].(float64)
		value := any(nil)
		if ok && amountOK {
			value = math.Copysign(amount*math.Min(math.Abs(change)*20, 1), change)
		}
		records = append(records, map[string]any{"symbol": quote["symbol"], "signed_turnover_proxy_cny": value, "method": "turnover_times_capped_price_direction", "is_proxy": true, "provider": providerDerived, "provenance": "go-stock:readonly-export-v2/fund_flow/turnover_proxy", "unit": map[string]string{"signed_turnover_proxy": "CNY"}, "source_timestamp": quote["source_timestamp"]})
	}
	recordProvider(ctx, providerDerived, "fund_flow", nil)
	return records, errs
}

func (c *Client) fetchSentimentV2(ctx context.Context, req Request, observed time.Time) (any, []PartialError) {
	quotes, errs := c.fetchQuotesV2(ctx, req, observed)
	records := quoteMaps(quotes)
	changes := []float64{}
	for _, quote := range records {
		if change, ok := quoteChange(quote); ok {
			changes = append(changes, change)
		}
	}
	sort.Float64s(changes)
	median := any(nil)
	if len(changes) > 0 {
		median = changes[len(changes)/2]
	}
	recordProvider(ctx, providerDerived, "sentiment", nil)
	return map[string]any{"sample_size": len(changes), "median_intraday_return": median, "method": "cross_sectional_quote_median", "is_proxy": true, "provider": providerDerived, "provenance": "go-stock:readonly-export-v2/sentiment/quote_proxy", "unit": map[string]string{"median_intraday_return": "fraction"}, "source_timestamp": observed.Format(time.RFC3339)}, errs
}
