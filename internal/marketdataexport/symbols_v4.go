package marketdataexport

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	marketCN = "CN"
	marketHK = "HK"
	marketUS = "US"

	kindSecurity = "security"
	kindIndex    = "index"
)

// instrument is the v4 multi-market identity. Symbol is the canonical form
// returned to consumers; the remaining fields drive provider ticker mapping.
type instrument struct {
	Symbol   string // 600519.SH, 920799.BJ, 00700.HK, HSI.HK, AAPL.US, .IXIC.US
	Market   string // CN, HK, US
	Exchange string // SH, SZ, BJ, HK, US
	Code     string // 600519, 00700, HSI, AAPL, IXIC (no leading dot)
	Kind     string // security, index
}

var (
	cnPrefixedPattern = regexp.MustCompile(`^(SH|SZ|BJ)([0-9]{6})$`)
	hkPrefixedPattern = regexp.MustCompile(`^HK([0-9]{1,5})$`)
	sixDigitPattern   = regexp.MustCompile(`^[0-9]{6}$`)
	hkCodePattern     = regexp.MustCompile(`^[0-9]{1,5}$`)
	hkIndexPattern    = regexp.MustCompile(`^[A-Z]{2,10}$`)
	usTickerPattern   = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,7}(\.[A-Z0-9]{1,3})?$`)
)

// NormalizeSymbolV4 returns the canonical MarketDataRequest/v4 symbol.
//
// Accepted forms:
//   - Mainland: 600519.SH, 000001.SZ, 920799.BJ, 600519.SS, sh600519, and a bare
//     six-digit code whose exchange follows from its prefix. Bare codes always
//     resolve to a tradable security, so an index must carry its suffix
//     (000001.SH is the SSE Composite, bare 000001 is 000001.SZ).
//   - Hong Kong: 00700.HK (shorter codes are zero-padded), hk00700, and
//     alphabetic index codes such as HSI.HK.
//   - United States: AAPL.US, BRK.B.US, and indices with a leading dot such
//     as .IXIC.US.
func NormalizeSymbolV4(raw string) (string, error) {
	item, err := parseInstrument(raw)
	if err != nil {
		return "", err
	}
	return item.Symbol, nil
}

func parseInstrument(raw string) (instrument, error) {
	value := strings.ToUpper(strings.TrimSpace(raw))
	invalid := func(reason string) (instrument, error) {
		return instrument{}, fmt.Errorf("invalid symbol %q: %s", raw, reason)
	}
	switch {
	case value == "":
		return invalid("empty")
	case strings.HasSuffix(value, ".US"):
		ticker := strings.ReplaceAll(strings.TrimSuffix(value, ".US"), "-", ".")
		kind := kindSecurity
		if strings.HasPrefix(ticker, ".") {
			kind = kindIndex
			ticker = strings.TrimPrefix(ticker, ".")
		}
		if !usTickerPattern.MatchString(ticker) {
			return invalid("expected a US ticker such as AAPL.US or .IXIC.US")
		}
		symbol := ticker + ".US"
		if kind == kindIndex {
			symbol = "." + symbol
		}
		return instrument{Symbol: symbol, Market: marketUS, Exchange: "US", Code: ticker, Kind: kind}, nil
	case strings.HasSuffix(value, ".HK"):
		return hkInstrument(strings.TrimSuffix(value, ".HK"), raw)
	case strings.HasSuffix(value, ".SH"), strings.HasSuffix(value, ".SS"), strings.HasSuffix(value, ".SZ"), strings.HasSuffix(value, ".BJ"):
		code, exchange := value[:len(value)-3], value[len(value)-2:]
		if exchange == "SS" {
			exchange = "SH"
		}
		return cnInstrument(code, exchange, raw)
	}
	if match := cnPrefixedPattern.FindStringSubmatch(value); match != nil {
		return cnInstrument(match[2], match[1], raw)
	}
	if match := hkPrefixedPattern.FindStringSubmatch(value); match != nil {
		return hkInstrument(match[1], raw)
	}
	if sixDigitPattern.MatchString(value) {
		exchange, ok := inferMainlandExchange(value)
		if !ok {
			return invalid("cannot infer the exchange; add .SH, .SZ or .BJ")
		}
		return cnInstrument(value, exchange, raw)
	}
	return invalid("expected NNNNNN.SH/.SZ/.BJ, NNNNN.HK or TICKER.US")
}

func inferMainlandExchange(code string) (string, bool) {
	switch {
	case strings.HasPrefix(code, "92"), code[0] == '4', code[0] == '8':
		return "BJ", true
	case code[0] == '6', code[0] == '5', code[0] == '9':
		return "SH", true
	case code[0] == '0', code[0] == '1', code[0] == '2', code[0] == '3':
		return "SZ", true
	}
	return "", false
}

func cnInstrument(code, exchange, raw string) (instrument, error) {
	if !sixDigitPattern.MatchString(code) {
		return instrument{}, fmt.Errorf("invalid symbol %q: mainland codes have six digits", raw)
	}
	kind := kindSecurity
	switch {
	case exchange == "SH" && strings.HasPrefix(code, "000"),
		exchange == "SZ" && strings.HasPrefix(code, "399"),
		exchange == "BJ" && strings.HasPrefix(code, "899"):
		kind = kindIndex
	}
	return instrument{Symbol: code + "." + exchange, Market: marketCN, Exchange: exchange, Code: code, Kind: kind}, nil
}

func hkInstrument(code, raw string) (instrument, error) {
	switch {
	case hkCodePattern.MatchString(code):
		padded := strings.Repeat("0", 5-len(code)) + code
		if padded == "00000" {
			break
		}
		return instrument{Symbol: padded + ".HK", Market: marketHK, Exchange: "HK", Code: padded, Kind: kindSecurity}, nil
	case hkIndexPattern.MatchString(code):
		return instrument{Symbol: code + ".HK", Market: marketHK, Exchange: "HK", Code: code, Kind: kindIndex}, nil
	}
	return instrument{}, fmt.Errorf("invalid symbol %q: expected a Hong Kong code such as 00700.HK or HSI.HK", raw)
}

// instrumentsV4 normalizes and de-duplicates request symbols, keeping the
// request order. Validation has already rejected malformed symbols.
func instrumentsV4(symbols []string) []instrument {
	seen := map[string]bool{}
	result := make([]instrument, 0, len(symbols))
	for _, raw := range symbols {
		item, err := parseInstrument(raw)
		if err != nil || seen[item.Symbol] {
			continue
		}
		seen[item.Symbol] = true
		result = append(result, item)
	}
	return result
}

func (i instrument) tencentQuoteTicker() string {
	switch i.Market {
	case marketHK:
		return "r_hk" + i.Code
	case marketUS:
		if i.Kind == kindIndex {
			return "us." + i.Code
		}
		return "us" + i.Code
	}
	return strings.ToLower(i.Exchange) + i.Code
}

// sinaReservedUSIndexCodes are index codes that Sina serves under the same
// gb_ prefix as stock tickers; a stock request with one of these tickers
// would silently receive index data.
var sinaReservedUSIndexCodes = map[string]bool{"IXIC": true, "NDX": true, "DJI": true, "INX": true}

func (i instrument) sinaQuoteTicker() (string, string) {
	switch i.Market {
	case marketHK:
		return "rt_hk" + i.Code, ""
	case marketUS:
		if i.Kind == kindSecurity && sinaReservedUSIndexCodes[i.Code] {
			return "", "ticker collides with a Sina index code"
		}
		return "gb_" + strings.ToLower(i.Code), ""
	}
	return strings.ToLower(i.Exchange) + i.Code, ""
}

// tencentBarTicker needs the exchange-qualified code (AAPL.OQ) for US
// securities; it comes from the Tencent quote payload.
func (i instrument) tencentBarTicker(usProviderCode string) (string, string) {
	switch i.Market {
	case marketHK:
		return "hk" + i.Code, ""
	case marketUS:
		if i.Kind == kindIndex {
			return "us." + i.Code, ""
		}
		if usProviderCode == "" {
			return "", "exchange-qualified US code unavailable"
		}
		return "us" + usProviderCode, ""
	}
	return strings.ToLower(i.Exchange) + i.Code, ""
}

var (
	eastmoneyHKIndexSecIDs = map[string]string{"HSI": "100.HSI", "HSTECH": "124.HSTECH"}
	eastmoneyUSIndexSecIDs = map[string]string{"IXIC": "100.NDX", "NDX": "100.NDX100", "DJI": "100.DJIA", "INX": "100.SPX"}
	eastmoneyUSMarkets     = map[string]string{"OQ": "105", "N": "106", "AM": "107"}
)

// eastmoneySecIDs returns candidate secids in probe order. US securities
// need a market number that only the Tencent exchange suffix reveals, so
// without it the three US markets are probed in turn.
func (i instrument) eastmoneySecIDs(usProviderCode string) []string {
	switch i.Market {
	case marketCN:
		if i.Exchange == "SH" {
			return []string{"1." + i.Code}
		}
		return []string{"0." + i.Code}
	case marketHK:
		if i.Kind == kindIndex {
			if secID, ok := eastmoneyHKIndexSecIDs[i.Code]; ok {
				return []string{secID}
			}
			return nil
		}
		return []string{"116." + i.Code}
	case marketUS:
		if i.Kind == kindIndex {
			if secID, ok := eastmoneyUSIndexSecIDs[i.Code]; ok {
				return []string{secID}
			}
			return nil
		}
		if index := strings.LastIndex(usProviderCode, "."); index >= 0 {
			if market, ok := eastmoneyUSMarkets[usProviderCode[index+1:]]; ok {
				return []string{market + "." + i.Code}
			}
		}
		return []string{"105." + i.Code, "106." + i.Code, "107." + i.Code}
	}
	return nil
}

// ruleCurrency is used only when a provider does not state the currency and
// the listing venue leaves no ambiguity. Mainland B shares and Hong Kong
// multi-currency counters stay unknown.
func ruleCurrency(i instrument) string {
	switch i.Market {
	case marketCN:
		if (i.Exchange == "SH" && strings.HasPrefix(i.Code, "90")) || (i.Exchange == "SZ" && strings.HasPrefix(i.Code, "20")) {
			return ""
		}
		return "CNY"
	case marketUS:
		return "USD"
	}
	return ""
}

// ruleLotSize returns a board lot only where an exchange-wide rule fixes it:
// 100 shares for Shanghai/Shenzhen main board, ChiNext and exchange-traded
// funds, and single shares in the US. STAR Market, Beijing, B shares and
// anything unclassified stay unknown; Hong Kong lots are per security and
// come from the provider.
func ruleLotSize(i instrument) (*float64, string) {
	if i.Kind != kindSecurity {
		return nil, ""
	}
	value := 0.0
	switch i.Market {
	case marketUS:
		value = 1
	case marketCN:
		prefix := i.Code[:2]
		switch {
		case i.Exchange == "SH" && (prefix == "60" || i.Code[0] == '5'),
			i.Exchange == "SZ" && (prefix == "00" || prefix == "30" || prefix == "15" || prefix == "16" || prefix == "18"):
			value = 100
		}
	}
	if value == 0 {
		return nil, ""
	}
	return &value, "market_rule"
}

func validCurrency(value string) bool {
	if len(value) != 3 {
		return false
	}
	for _, char := range value {
		if char < 'A' || char > 'Z' {
			return false
		}
	}
	return true
}
