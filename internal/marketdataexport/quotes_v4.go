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
	"strings"
	"time"
)

const (
	// quoteBatchSizeV4 bounds one provider URL; a watchlist of a few dozen
	// symbols is a single request.
	quoteBatchSizeV4      = 50
	quoteRequestTimeoutV4 = 8 * time.Second
	// A quote whose provider timestamp is this close to the local clock is
	// evidently live, whatever the feed is called.
	measuredRealtimeLag       = 60 * time.Second
	measuredRealtimeLagMinute = 120 * time.Second
	futureTimestampTolerance  = 5 * time.Minute

	timestampProvider       = "provider"
	timestampProviderMinute = "provider_minute"
	timestampFallback       = "observed_at_fallback"

	zoneShanghai = "Asia/Shanghai"
	zoneHongKong = "Asia/Hong_Kong"
	zoneNewYork  = "America/New_York"
)

// delayV4 states how late a quote feed runs. Level is realtime, delayed or
// unknown. Basis says where the level comes from: measured_lag (this
// record's own timestamp is within the realtime threshold),
// provider_declared (the provider names or flags the feed), or none.
type delayV4 struct {
	Level   string `json:"level"`
	Minutes *int   `json:"minutes"`
	Basis   string `json:"basis"`
}

var (
	delayUnknown          = delayV4{Level: "unknown", Basis: "none"}
	delayDeclaredRealtime = delayV4{Level: "realtime", Basis: "provider_declared"}
	delayDeclaredDelayed  = delayV4{Level: "delayed", Basis: "provider_declared"}
	delayMeasuredRealtime = delayV4{Level: "realtime", Basis: "measured_lag"}
)

func marketZone(market string) string {
	switch market {
	case marketHK:
		return zoneHongKong
	case marketUS:
		return zoneNewYork
	}
	return zoneShanghai
}

func parseInZone(layout, text, zone string) (time.Time, bool) {
	location, err := time.LoadLocation(zone)
	if err != nil {
		return time.Time{}, false
	}
	parsed, err := time.ParseInLocation(layout, strings.TrimSpace(text), location)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

func floatAt(fields []string, index int) (float64, bool) {
	value, err := numberAt(fields, index)
	return value, err == nil
}

func positiveAt(fields []string, index int, multiplier float64) *float64 {
	value, ok := floatAt(fields, index)
	if !ok || value <= 0 {
		return nil
	}
	value = scaled(value, multiplier)
	return &value
}

func nonNegativeAt(fields []string, index int, multiplier float64) *float64 {
	value, ok := floatAt(fields, index)
	if !ok || value < 0 {
		return nil
	}
	value = scaled(value, multiplier)
	return &value
}

// scaled converts a provider unit and trims the float noise the
// multiplication introduces.
func scaled(value, multiplier float64) float64 {
	if multiplier == 1 {
		return value
	}
	return roundTo(value*multiplier, 4)
}

// tencentMainlandVolumeMultiplier converts Tencent's total volume to shares.
// Tencent reports lots of 100 shares except on the STAR Market (688/689),
// where the same field already holds shares.
func tencentMainlandVolumeMultiplier(item instrument) float64 {
	if item.Exchange == "SH" && (strings.HasPrefix(item.Code, "688") || strings.HasPrefix(item.Code, "689")) {
		return 1
	}
	return 100
}

// tencentMainlandVolume resolves the lots-or-shares ambiguity from the
// record itself: the multiplier must make turnover/volume land near the
// traded price. Without turnover the board rule decides; when turnover
// contradicts both readings the volume is left unknown.
func tencentMainlandVolume(item instrument, fields []string, price float64, amount *float64) *float64 {
	raw, ok := floatAt(fields, 36)
	if !ok || raw < 0 {
		return nil
	}
	multiplier := tencentMainlandVolumeMultiplier(item)
	if amount != nil && *amount > 0 && raw > 0 && price > 0 {
		plausible := func(candidate float64) bool {
			implied := *amount / (raw * candidate)
			return implied >= price*0.5 && implied <= price*2
		}
		switch lots, shares := plausible(100), plausible(1); {
		case lots && !shares:
			multiplier = 100
		case shares && !lots:
			multiplier = 1
		case !lots && !shares:
			return nil
		}
	}
	value := scaled(raw, multiplier)
	return &value
}

func fieldAt(fields []string, index int) string {
	if index < 0 || index >= len(fields) {
		return ""
	}
	return strings.TrimSpace(fields[index])
}

// getRawV4 requests rawURL exactly as written. Sina rejects percent-encoded
// commas in a batch list, so batch quote URLs must not be re-encoded. The
// final host is returned because EastMoney redirects some networks to a
// delayed-quote host.
func (c *Client) getRawV4(ctx context.Context, rawURL string, headers map[string]string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "go-stock-market-data-export/4")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	host := req.URL.Host
	if resp.Request != nil && resp.Request.URL != nil {
		host = resp.Request.URL.Host
	}
	if resp.StatusCode != http.StatusOK {
		return nil, host, fmt.Errorf("provider returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, host, err
	}
	if len(data) == 0 {
		return nil, host, errors.New("empty provider response")
	}
	return data, host, nil
}

// quoteFieldsV4 is the provider-neutral quote before unit, lag and delay
// evidence are attached. Nil pointers become JSON null: a field the provider
// does not supply is never filled in.
type quoteFieldsV4 struct {
	Name          string
	Price         float64
	Open          *float64
	High          *float64
	Low           *float64
	PreviousClose *float64
	Volume        *float64
	Amount        *float64
	Bid1Price     *float64
	Bid1Volume    *float64
	Ask1Price     *float64
	Ask1Volume    *float64
	Currency      string
	LotSize       *float64
	LotSource     string
	SourceTime    time.Time
	Quality       string
	Provider      string
	Feed          string
	Declared      delayV4
	// ProviderCode is the provider's own instrument code (AAPL.OQ); it is
	// used for US bar lookups and is not exported.
	ProviderCode string
}

// unitV4 describes one record's units. An index is quoted in points; its
// turnover unit is stated only when a turnover value is present.
func unitV4(item instrument, currency string, lot *float64, lotSource string, hasAmount bool) map[string]any {
	unit := map[string]any{"currency": nil, "price": nil, "volume": nil, "amount": nil, "lot_size": nil, "lot_size_source": nil}
	if item.Kind == kindIndex {
		unit["price"] = "index_point"
		if currency != "" && hasAmount {
			unit["amount"] = currency
		}
		return unit
	}
	unit["volume"] = "share"
	if currency != "" {
		unit["currency"] = currency
		unit["price"] = currency + "_per_share"
		unit["amount"] = currency
	}
	if lot != nil {
		unit["lot_size"] = *lot
		unit["lot_size_source"] = lotSource
	}
	return unit
}

func roundTo(value float64, digits int) float64 {
	scale := math.Pow(10, float64(digits))
	return math.Round(value*scale) / scale
}

func buildQuoteRecordV4(item instrument, quote quoteFieldsV4, observed time.Time) (map[string]any, error) {
	if !(quote.Price > 0) || math.IsInf(quote.Price, 0) {
		return nil, errors.New("quote price invalid")
	}
	sourceTime := observed
	quality := timestampFallback
	var lag any
	delay := quote.Declared
	if !quote.SourceTime.IsZero() {
		if quote.SourceTime.After(observed.Add(futureTimestampTolerance)) {
			return nil, fmt.Errorf("source timestamp %s is in the future", quote.SourceTime.UTC().Format(time.RFC3339))
		}
		sourceTime = quote.SourceTime.UTC()
		quality = quote.Quality
		elapsed := observed.Sub(sourceTime)
		if elapsed < 0 {
			elapsed = 0
		}
		lag = math.Round(elapsed.Seconds())
		threshold := measuredRealtimeLag
		if quality == timestampProviderMinute {
			threshold = measuredRealtimeLagMinute
		}
		if elapsed <= threshold {
			delay = delayMeasuredRealtime
		}
	}
	currency := quote.Currency
	if !validCurrency(currency) {
		currency = ruleCurrency(item)
	}
	lot, lotSource := quote.LotSize, quote.LotSource
	if lot == nil {
		lot, lotSource = ruleLotSize(item)
	}
	var change, changeRatio any
	if quote.PreviousClose != nil {
		change = roundTo(quote.Price-*quote.PreviousClose, 6)
		changeRatio = roundTo(quote.Price / *quote.PreviousClose - 1, 6)
	}
	return map[string]any{
		"symbol": item.Symbol, "market": item.Market, "exchange": item.Exchange, "instrument_type": item.Kind,
		"name": quote.Name, "price": quote.Price,
		"open": quote.Open, "high": quote.High, "low": quote.Low, "previous_close": quote.PreviousClose,
		"change": change, "change_ratio": changeRatio,
		"volume": quote.Volume, "amount": quote.Amount,
		"bid1_price": quote.Bid1Price, "bid1_volume": quote.Bid1Volume, "ask1_price": quote.Ask1Price, "ask1_volume": quote.Ask1Volume,
		"unit":             unitV4(item, currency, lot, lotSource, quote.Amount != nil),
		"source_timestamp": sourceTime.Format(time.RFC3339), "source_timezone": marketZone(item.Market),
		"timestamp_quality": quality, "lag_seconds": lag, "delay": delay,
		"provider": quote.Provider, "provider_feed": quote.Feed,
		"provenance": "go-stock:readonly-export-v4/quotes/" + strings.TrimSuffix(quote.Provider, "_public_web"),
	}, nil
}

var (
	tencentStatementPattern = regexp.MustCompile(`v_([^=\s]+)="([^"]*)"`)
	sinaStatementPattern    = regexp.MustCompile(`hq_str_([^=\s]+)="([^"]*)"`)
)

func splitStatements(pattern *regexp.Regexp, body string) map[string]string {
	result := map[string]string{}
	for _, match := range pattern.FindAllStringSubmatch(body, -1) {
		result[match[1]] = match[2]
	}
	return result
}

// Tencent tags each quote with an instrument class (GP stock, ETF, ZS index).
// The tag guards against a ticker that the provider resolves to a different
// kind of instrument, for example usIXIC returning the Nasdaq Composite.
func tencentKindMismatch(item instrument, marker string) bool {
	if marker == "" {
		return false
	}
	return (marker == "ZS") != (item.Kind == kindIndex)
}

func parseTencentQuoteV4(item instrument, payload string) (quoteFieldsV4, error) {
	fields := strings.Split(payload, "~")
	if len(fields) < 38 {
		return quoteFieldsV4{}, fmt.Errorf("Tencent quote payload has %d fields", len(fields))
	}
	price, ok := floatAt(fields, 3)
	if !ok || price <= 0 {
		return quoteFieldsV4{}, errors.New("Tencent quote price invalid")
	}
	quote := quoteFieldsV4{
		Name: fieldAt(fields, 1), Price: price,
		PreviousClose: positiveAt(fields, 4, 1), Open: positiveAt(fields, 5, 1),
		High: positiveAt(fields, 33, 1), Low: positiveAt(fields, 34, 1),
		Provider: providerTencent, ProviderCode: fieldAt(fields, 2), Declared: delayDeclaredRealtime,
	}
	code := fieldAt(fields, 2)
	isIndex := item.Kind == kindIndex
	switch item.Market {
	case marketCN:
		if code != item.Code || tencentKindMismatch(item, fieldAt(fields, 61)) {
			return quoteFieldsV4{}, fmt.Errorf("Tencent returned a different instrument (%s %s)", code, fieldAt(fields, 61))
		}
		quote.Feed = "tencent:" + strings.ToLower(item.Exchange)
		quote.Currency = fieldAt(fields, 82)
		// Field 35 is price/volume/amount with the exact turnover; field 37
		// is the same turnover rounded to 10k.
		if parts := strings.Split(fieldAt(fields, 35), "/"); len(parts) == 3 {
			quote.Amount = nonNegativeAt(parts, 2, 1)
		}
		if quote.Amount == nil {
			quote.Amount = nonNegativeAt(fields, 37, 10000)
		}
		if !isIndex {
			quote.Volume = tencentMainlandVolume(item, fields, price, quote.Amount)
			// Order-book sizes are in lots on every board.
			quote.Bid1Price, quote.Bid1Volume = positiveAt(fields, 9, 1), nonNegativeAt(fields, 10, 100)
			quote.Ask1Price, quote.Ask1Volume = positiveAt(fields, 19, 1), nonNegativeAt(fields, 20, 100)
			if quote.Bid1Price == nil {
				quote.Bid1Volume = nil
			}
			if quote.Ask1Price == nil {
				quote.Ask1Volume = nil
			}
		}
		if stamp, parsed := parseInZone("20060102150405", fieldAt(fields, 30), zoneShanghai); parsed {
			quote.SourceTime, quote.Quality = stamp, timestampProvider
		}
	case marketHK:
		if code != item.Code || tencentKindMismatch(item, fieldAt(fields, 63)) {
			return quoteFieldsV4{}, fmt.Errorf("Tencent returned a different instrument (%s %s)", code, fieldAt(fields, 63))
		}
		quote.Feed = "tencent:r_hk"
		quote.Currency = fieldAt(fields, 75)
		if isIndex {
			// Index turnover is reported in units of 10k.
			quote.Amount = positiveAt(fields, 37, 10000)
		} else {
			quote.Volume = nonNegativeAt(fields, 36, 1)
			quote.Amount = nonNegativeAt(fields, 37, 1)
			if lot := positiveAt(fields, 60, 1); lot != nil {
				quote.LotSize, quote.LotSource = lot, "provider"
			}
		}
		if stamp, parsed := parseInZone("2006/01/02 15:04:05", fieldAt(fields, 30), zoneHongKong); parsed {
			quote.SourceTime, quote.Quality = stamp, timestampProvider
		}
	case marketUS:
		expected := item.Code + "."
		if isIndex {
			expected = "." + item.Code
		}
		matches := strings.HasPrefix(code, expected)
		if isIndex {
			matches = code == expected
		}
		if !matches || tencentKindMismatch(item, fieldAt(fields, 56)) {
			return quoteFieldsV4{}, fmt.Errorf("Tencent returned a different instrument (%s %s)", code, fieldAt(fields, 56))
		}
		quote.Feed = "tencent:us"
		quote.Currency = fieldAt(fields, 35)
		if !isIndex {
			quote.Volume = nonNegativeAt(fields, 36, 1)
			quote.Amount = nonNegativeAt(fields, 37, 1)
			// Tencent flags its web quote for US securities as "delay" in
			// the K-line payload's embedded quote; indices carry no flag.
			quote.Declared = delayDeclaredDelayed
		}
		if stamp, parsed := parseInZone("2006-01-02 15:04:05", fieldAt(fields, 30), zoneNewYork); parsed {
			quote.SourceTime, quote.Quality = stamp, timestampProvider
		}
	}
	return quote, nil
}

// parseSinaUSTradeTime reads "Nov 13 04:00PM EST". The year is absent, so
// the most recent year that does not put the trade in the future is used.
func parseSinaUSTradeTime(text string, observed time.Time) (time.Time, bool) {
	parts := strings.Fields(text)
	if len(parts) < 3 {
		return time.Time{}, false
	}
	location, err := time.LoadLocation(zoneNewYork)
	if err != nil {
		return time.Time{}, false
	}
	stamp, err := time.Parse("Jan 2 03:04PM", strings.Join(parts[:3], " "))
	if err != nil {
		return time.Time{}, false
	}
	year := observed.In(location).Year()
	for _, candidateYear := range []int{year, year - 1} {
		candidate := time.Date(candidateYear, stamp.Month(), stamp.Day(), stamp.Hour(), stamp.Minute(), 0, 0, location)
		if !candidate.After(observed.Add(24 * time.Hour)) {
			return candidate, true
		}
	}
	return time.Time{}, false
}

func parseSinaQuoteV4(item instrument, payload string, observed time.Time) (quoteFieldsV4, error) {
	fields := strings.Split(payload, ",")
	quote := quoteFieldsV4{Provider: providerSina, Declared: delayUnknown}
	isIndex := item.Kind == kindIndex
	var price float64
	var ok bool
	switch item.Market {
	case marketCN:
		if len(fields) < 32 {
			return quoteFieldsV4{}, fmt.Errorf("Sina quote payload has %d fields", len(fields))
		}
		price, ok = floatAt(fields, 3)
		quote.Name, quote.Feed = fieldAt(fields, 0), "sina:"+strings.ToLower(item.Exchange)
		quote.Open, quote.PreviousClose = positiveAt(fields, 1, 1), positiveAt(fields, 2, 1)
		quote.High, quote.Low = positiveAt(fields, 4, 1), positiveAt(fields, 5, 1)
		quote.Amount = nonNegativeAt(fields, 9, 1)
		if !isIndex {
			// Sina reports index volume in lots for Shanghai and shares for
			// Shenzhen, so index volume is left unknown.
			quote.Volume = nonNegativeAt(fields, 8, 1)
			quote.Bid1Price, quote.Bid1Volume = positiveAt(fields, 11, 1), nonNegativeAt(fields, 10, 1)
			quote.Ask1Price, quote.Ask1Volume = positiveAt(fields, 21, 1), nonNegativeAt(fields, 20, 1)
			if quote.Bid1Price == nil {
				quote.Bid1Volume = nil
			}
			if quote.Ask1Price == nil {
				quote.Ask1Volume = nil
			}
		}
		if stamp, parsed := parseInZone("2006-01-02 15:04:05", fieldAt(fields, 30)+" "+fieldAt(fields, 31), zoneShanghai); parsed {
			quote.SourceTime, quote.Quality = stamp, timestampProvider
		}
	case marketHK:
		if len(fields) < 19 {
			return quoteFieldsV4{}, fmt.Errorf("Sina quote payload has %d fields", len(fields))
		}
		price, ok = floatAt(fields, 6)
		quote.Name, quote.Feed = fieldAt(fields, 1), "sina:rt_hk"
		if quote.Name == "" {
			quote.Name = fieldAt(fields, 0)
		}
		// rt_hk is Sina's realtime-named Hong Kong feed; the plain hk feed
		// is the delayed one.
		quote.Declared = delayDeclaredRealtime
		quote.Open, quote.PreviousClose = positiveAt(fields, 2, 1), positiveAt(fields, 3, 1)
		quote.High, quote.Low = positiveAt(fields, 4, 1), positiveAt(fields, 5, 1)
		if isIndex {
			// Index turnover is reported in thousands.
			quote.Amount = positiveAt(fields, 11, 1000)
		} else {
			quote.Bid1Price, quote.Ask1Price = positiveAt(fields, 9, 1), positiveAt(fields, 10, 1)
			quote.Amount = nonNegativeAt(fields, 11, 1)
			quote.Volume = nonNegativeAt(fields, 12, 1)
		}
		if stamp, parsed := parseInZone("2006/01/02 15:04:05", fieldAt(fields, 17)+" "+fieldAt(fields, 18), zoneHongKong); parsed {
			quote.SourceTime, quote.Quality = stamp, timestampProvider
		}
	case marketUS:
		if len(fields) < 27 {
			return quoteFieldsV4{}, fmt.Errorf("Sina quote payload has %d fields", len(fields))
		}
		price, ok = floatAt(fields, 1)
		quote.Name, quote.Feed = fieldAt(fields, 0), "sina:gb"
		quote.Open, quote.High, quote.Low = positiveAt(fields, 5, 1), positiveAt(fields, 6, 1), positiveAt(fields, 7, 1)
		quote.PreviousClose = positiveAt(fields, 26, 1)
		if !isIndex {
			quote.Volume = nonNegativeAt(fields, 10, 1)
			quote.Amount = positiveAt(fields, 30, 1)
		}
		// Field 25 is the regular-session trade time at minute resolution.
		// Field 3 has seconds but keeps moving with extended-hours prints
		// that do not change the regular price, so it is trusted only when
		// it agrees with field 25.
		if minuteStamp, parsed := parseSinaUSTradeTime(fieldAt(fields, 25), observed); parsed {
			quote.SourceTime, quote.Quality = minuteStamp, timestampProviderMinute
			if secondStamp, secondParsed := parseInZone("2006-01-02 15:04:05", fieldAt(fields, 3), zoneShanghai); secondParsed {
				if gap := secondStamp.Sub(minuteStamp); gap >= 0 && gap < 90*time.Second {
					quote.SourceTime, quote.Quality = secondStamp, timestampProvider
				}
			}
		}
	}
	if !ok || price <= 0 {
		return quoteFieldsV4{}, errors.New("Sina quote price invalid")
	}
	quote.Price = price
	return quote, nil
}

// quoteEastmoneyV4 is the per-symbol third fallback for mainland symbols.
// The boolean reports whether the provider answered with a decodable
// payload, as opposed to a transport failure.
func (c *Client) quoteEastmoneyV4(ctx context.Context, item instrument) (quoteFieldsV4, bool, error) {
	values := url.Values{"secid": {item.eastmoneySecIDs("")[0]}, "fltt": {"2"}, "invt": {"2"}, "fields": {"f57,f58,f43,f44,f45,f46,f47,f48,f60,f19,f20,f39,f40,f86,f124"}}
	separator := "?"
	if strings.Contains(c.Endpoints.Quote, "?") {
		separator = "&"
	}
	body, host, err := c.getRawV4(ctx, c.Endpoints.Quote+separator+values.Encode(), map[string]string{"Accept": "application/json"})
	if err != nil {
		return quoteFieldsV4{}, false, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload struct {
		RC   int            `json:"rc"`
		Data map[string]any `json:"data"`
	}
	if err := decoder.Decode(&payload); err != nil {
		return quoteFieldsV4{}, false, fmt.Errorf("decode EastMoney quote: %w", err)
	}
	if payload.RC != 0 || payload.Data == nil {
		return quoteFieldsV4{}, true, errSymbolMissing
	}
	if code, _ := payload.Data["f57"].(string); code != item.Code {
		return quoteFieldsV4{}, true, fmt.Errorf("EastMoney returned a different instrument (%s)", code)
	}
	number := func(field string, multiplier float64, allowZero bool) *float64 {
		value, err := finiteFloat(payload.Data[field], field)
		if err != nil || value < 0 || (!allowZero && value == 0) {
			return nil
		}
		value = scaled(value, multiplier)
		return &value
	}
	price := number("f43", 1, false)
	if price == nil {
		return quoteFieldsV4{}, true, errors.New("EastMoney quote price invalid")
	}
	name, _ := payload.Data["f58"].(string)
	quote := quoteFieldsV4{
		Name: name, Price: *price,
		Open: number("f46", 1, false), High: number("f44", 1, false), Low: number("f45", 1, false), PreviousClose: number("f60", 1, false),
		Amount:   number("f48", 1, true),
		Provider: providerEast, Feed: "eastmoney:push2", Declared: delayUnknown,
	}
	if item.Kind != kindIndex {
		quote.Volume = number("f47", 100, true)
		quote.Bid1Price, quote.Bid1Volume = number("f19", 1, false), number("f20", 100, true)
		quote.Ask1Price, quote.Ask1Volume = number("f39", 1, false), number("f40", 100, true)
		if quote.Bid1Price == nil {
			quote.Bid1Volume = nil
		}
		if quote.Ask1Price == nil {
			quote.Ask1Volume = nil
		}
	}
	// The delayed host leaves f124 at zero and keeps the trade time in f86.
	for _, field := range []string{"f86", "f124"} {
		if seconds := number(field, 1, false); seconds != nil {
			quote.SourceTime, quote.Quality = time.Unix(int64(*seconds), 0).UTC(), timestampProvider
			break
		}
	}
	if strings.Contains(host, "delay") {
		quote.Feed, quote.Declared = "eastmoney:push2delay", delayDeclaredDelayed
	}
	return quote, true, nil
}

var errSymbolMissing = errors.New("symbol missing from provider response")

type quoteStateV4 struct {
	chain    []providerAttempt
	failures []string
	// attempts counts providers that were asked; missing counts those that
	// answered without this symbol.
	attempts int
	missing  int
	record   map[string]any
	quote    quoteFieldsV4
}

func (s *quoteStateV4) fail(providerName string, err error) {
	s.chain = append(s.chain, providerAttempt{Provider: providerName, Status: "failed", Reason: err.Error()})
	s.failures = append(s.failures, providerName+": "+err.Error())
	s.attempts++
	if errors.Is(err, errSymbolMissing) {
		s.missing++
	}
}

// unknownEverywhere reports that every provider asked so far answered and
// none of them has the symbol. A provider that failed to answer leaves the
// question open.
func (s *quoteStateV4) unknownEverywhere() bool {
	return s.attempts > 0 && s.missing == s.attempts
}

func (s *quoteStateV4) skip(providerName, reason string) {
	s.chain = append(s.chain, providerAttempt{Provider: providerName, Status: "skipped", Reason: reason})
}

type quoteBatchProviderV4 struct {
	name     string
	endpoint string
	headers  map[string]string
	ticker   func(instrument) (string, string)
	pattern  *regexp.Regexp
	parse    func(item instrument, payload string, observed time.Time) (quoteFieldsV4, error)
}

func (c *Client) quoteBatchProvidersV4() []quoteBatchProviderV4 {
	return []quoteBatchProviderV4{
		{
			name: providerTencent, endpoint: c.Endpoints.QuoteTencent, headers: map[string]string{"Referer": "https://gu.qq.com/"},
			ticker:  func(item instrument) (string, string) { return item.tencentQuoteTicker(), "" },
			pattern: tencentStatementPattern,
			parse: func(item instrument, payload string, _ time.Time) (quoteFieldsV4, error) {
				return parseTencentQuoteV4(item, payload)
			},
		},
		{
			name: providerSina, endpoint: c.Endpoints.QuoteSina, headers: map[string]string{"Referer": "https://finance.sina.com.cn/"},
			ticker:  func(item instrument) (string, string) { return item.sinaQuoteTicker() },
			pattern: sinaStatementPattern,
			parse:   parseSinaQuoteV4,
		},
	}
}

// quoteRoundV4 asks one provider for every pending instrument, one HTTP
// request per batch, and returns the instruments that still have no quote.
func (c *Client) quoteRoundV4(ctx context.Context, source quoteBatchProviderV4, pending []instrument, states map[string]*quoteStateV4, observed time.Time) []instrument {
	remaining := []instrument{}
	type entry struct {
		item   instrument
		ticker string
	}
	batch := []entry{}
	flush := func() {
		if len(batch) == 0 {
			return
		}
		tickers := make([]string, len(batch))
		for index, current := range batch {
			tickers[index] = current.ticker
		}
		requestCtx, cancel := context.WithTimeout(ctx, quoteRequestTimeoutV4)
		body, _, err := c.getRawV4(requestCtx, endpointWithTicker(source.endpoint, strings.Join(tickers, ",")), source.headers)
		cancel()
		text := ""
		if err == nil {
			text, err = decodeGB18030(body)
		}
		recordProvider(ctx, source.name, "quotes", err)
		if err != nil {
			for _, current := range batch {
				states[current.item.Symbol].fail(source.name, err)
				remaining = append(remaining, current.item)
			}
			batch = batch[:0]
			return
		}
		payloads := splitStatements(source.pattern, text)
		for _, current := range batch {
			state := states[current.item.Symbol]
			payload := payloads[current.ticker]
			if payload == "" {
				state.fail(source.name, errSymbolMissing)
				remaining = append(remaining, current.item)
				continue
			}
			quote, parseErr := source.parse(current.item, payload, observed)
			var record map[string]any
			if parseErr == nil {
				record, parseErr = buildQuoteRecordV4(current.item, quote, observed)
			}
			if parseErr != nil {
				state.fail(source.name, parseErr)
				remaining = append(remaining, current.item)
				continue
			}
			state.chain = append(state.chain, providerAttempt{Provider: source.name, Status: "selected"})
			state.record, state.quote = record, quote
		}
		batch = batch[:0]
	}
	for _, item := range pending {
		ticker, reason := source.ticker(item)
		if ticker == "" {
			states[item.Symbol].skip(source.name, reason)
			remaining = append(remaining, item)
			continue
		}
		batch = append(batch, entry{item, ticker})
		if len(batch) == quoteBatchSizeV4 {
			flush()
		}
	}
	flush()
	return remaining
}

// resolveQuotesV4 runs the bounded fallback chain: Tencent batch, Sina batch,
// then per-symbol EastMoney for mainland symbols a batch provider failed on.
func (c *Client) resolveQuotesV4(ctx context.Context, instruments []instrument, observed time.Time) map[string]*quoteStateV4 {
	states := make(map[string]*quoteStateV4, len(instruments))
	for _, item := range instruments {
		states[item.Symbol] = &quoteStateV4{}
	}
	pending := instruments
	for _, source := range c.quoteBatchProvidersV4() {
		if len(pending) == 0 {
			break
		}
		pending = c.quoteRoundV4(ctx, source, pending, states, observed)
	}
	consecutiveTransportFailures := 0
	for _, item := range pending {
		state := states[item.Symbol]
		switch {
		case item.Market != marketCN:
			state.skip(providerEast, "quote fallback covers mainland symbols only")
			continue
		case state.unknownEverywhere():
			// Every batch provider answered and none knows the symbol; a
			// third lookup on each poll would only slow the batch down.
			state.skip(providerEast, "symbol unknown to the batch providers")
			continue
		case consecutiveTransportFailures >= 2:
			state.skip(providerEast, "fallback stopped after consecutive failures")
			continue
		}
		requestCtx, cancel := context.WithTimeout(ctx, quoteRequestTimeoutV4)
		quote, answered, err := c.quoteEastmoneyV4(requestCtx, item)
		cancel()
		if answered {
			recordProvider(ctx, providerEast, "quotes", nil)
			consecutiveTransportFailures = 0
		} else {
			recordProvider(ctx, providerEast, "quotes", err)
			consecutiveTransportFailures++
		}
		var record map[string]any
		if err == nil {
			record, err = buildQuoteRecordV4(item, quote, observed)
		}
		if err != nil {
			state.fail(providerEast, err)
			continue
		}
		state.chain = append(state.chain, providerAttempt{Provider: providerEast, Status: "selected"})
		state.record, state.quote = record, quote
	}
	return states
}

func (c *Client) fetchQuotesV4(ctx context.Context, req Request, observed time.Time) (any, []PartialError) {
	instruments := instrumentsV4(req.Symbols)
	states := c.resolveQuotesV4(ctx, instruments, observed)
	records := []any{}
	errs := []PartialError{}
	for _, item := range instruments {
		state := states[item.Symbol]
		if state.record == nil {
			reason := errors.New(strings.Join(state.failures, "; "))
			if state.unknownEverywhere() {
				errs = append(errs, operationError("quotes", item.Symbol, "symbol_not_found", reason, false))
			} else {
				errs = append(errs, operationError("quotes", item.Symbol, "all_providers_failed", reason, true))
			}
			continue
		}
		state.record["provider_chain"] = state.chain
		if len(state.failures) > 0 {
			state.record["fallback_reason"] = strings.Join(state.failures, "; ")
		}
		records = append(records, state.record)
	}
	if len(records) == 0 && len(errs) > 0 {
		return nil, errs
	}
	return records, errs
}
