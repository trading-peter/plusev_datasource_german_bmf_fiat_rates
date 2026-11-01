package main

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	ex "github.com/plusev-terminal/go-plugin-common/v2/datasrc/exchange"
	"github.com/plusev-terminal/go-plugin-common/v2/logging"
	m "github.com/plusev-terminal/go-plugin-common/v2/meta"
	"github.com/plusev-terminal/go-plugin-common/v2/plugin"
	"github.com/plusev-terminal/go-plugin-common/v2/requester"
	rt "github.com/plusev-terminal/go-plugin-common/v2/requester/types"
	"github.com/plusev-terminal/go-plugin-common/v2/trading"
	"github.com/plusev-terminal/go-plugin-common/v2/wasmutils"
)

// ============================================================================
// Main - Register the plugin
// ============================================================================

func init() {
	p := &BMFRatesPlugin{}
	plugin.RegisterPlugin(p)
}

func main() {}

// ============================================================================
// Plugin Implementation
// ============================================================================

var log = logging.NewLogger("bmf-fiat-rates")

// pricePattern matches values like "1,6422 AUD" or "17.049,43 IDR"
var pricePattern = regexp.MustCompile(`^([0-9][0-9.,]*) ([A-Z]+)$`)

const firstYear = 2015
const urlTpl = "https://www.bundesfinanzministerium.de/Datenportal/Daten/offene-daten/steuern-zoelle/umsatzsteuer-umrechnungskurse/datensaetze/uu-kurse-%d-csv.csv?__blob=publicationFile&v=4"
const dataDir = "/data"

// rates stores parsed rates: currency -> year -> month -> rate (as "1 EUR = rate CURRENCY")
type rateStore map[string]map[int]map[time.Month]string

type BMFRatesPlugin struct {
	config *plugin.ConfigStore
	req    *requester.Requester
	rates  rateStore
	loaded bool
}

func (p *BMFRatesPlugin) GetMeta() m.Meta {
	return m.Meta{
		PluginID:    "german-bmf-fiat-rates",
		Name:        "German BMF Official Fiat Rates",
		AppID:       "datasrc",
		Category:    "file",
		Description: "Official monthly Umsatzsteuer-Umrechnungskurse (VAT exchange rates) published by the German Bundesfinanzministerium. These are the legally binding rates for German tax calculations.",
		Author:      "trading_peter",
		Version: "v1.1.0",
		Repository:  "https://github.com/trading-peter/plusev_datasource_german_bmf_fiat_rates",
		Tags:        []string{"german", "bmf", "fiat", "rates", "tax", "official", "eur"},
		Contacts: []m.AuthorContact{
			{Kind: "x.com", Value: "https://x.com/trading_peter"},
		},
		Resources: m.ResourceAccess{
			AllowedNetworkTargets: []m.NetworkTargetRule{
				{
					Pattern: "https://www.bundesfinanzministerium.de/*",
					// BMF publishes no rate limits for its Datenportal CSV
					// downloads (checked 2026-10-31). This is a conservative
					// courtesy rule, not a documented limit.
					// Source dataset page:
					// https://www.bundesfinanzministerium.de/Datenportal/Daten/offene-daten/steuern-zoelle/umsatzsteuer-umrechnungskurse
					Limits: []m.RateLimitRule{
						m.NewRateLimitRule("total", 1, time.Second).WithBurst(5),
					},
				},
			},
			FsWriteAccess: map[string]string{
				"data": "/data",
			},
		},
		Features: []string{},
	}
}

func (p *BMFRatesPlugin) GetConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{}
}

func (p *BMFRatesPlugin) OnInit(config *plugin.ConfigStore) error {
	p.config = config
	p.req = requester.NewRequester()
	p.rates = make(rateStore)
	return nil
}

func (p *BMFRatesPlugin) OnShutdown() error {
	return nil
}

func (p *BMFRatesPlugin) GetRateLimits() []plugin.RateLimit {
	// Empty on purpose: limits are declared on the manifest network target above;
	// an explicit empty array opts out of the legacy per-command limiter.
	return []plugin.RateLimit{}
}

func (p *BMFRatesPlugin) RegisterCommands(router *plugin.CommandRouter) {
	router.Register(ex.CMD_GET_MARKETS, p.handleGetMarkets)
	router.Register(ex.CMD_GET_TIMEFRAMES, p.handleGetTimeframes)
	router.Register(ex.CMD_GET_OHLCV, p.handleGetOHLCV)
}

// ============================================================================
// Command Handlers
// ============================================================================

func (p *BMFRatesPlugin) handleGetMarkets(params map[string]any) plugin.Response {
	if err := p.ensureRates(); err != nil {
		return plugin.ErrorResponse(err)
	}

	currencies := make([]string, 0, len(p.rates))
	for currency := range p.rates {
		currencies = append(currencies, currency)
	}
	sort.Strings(currencies)

	markets := make([]trading.Market, 0, len(currencies))
	for _, currency := range currencies {
		// Each rate pair is FOREIGN/EUR — "how many EUR for 1 unit of FOREIGN"
		markets = append(markets, trading.Market{
			Label:             currency + "/EUR",
			AssetType:         trading.AssetTypeSpot,
			QuantityPrecision: 4,
			PricePrecision:    4,
			Symbol:            currency + "/EUR",
			Base:              currency,
			Quote:             "EUR",
			PriceTick:         "0.0001",
			QuantityTick:      "0.0001",
		})
	}

	return plugin.SuccessResponse(markets, time.Hour)
}

func (p *BMFRatesPlugin) handleGetTimeframes(params map[string]any) plugin.Response {
	return plugin.SuccessResponse([]string{"1M"}, time.Hour*24)
}

func (p *BMFRatesPlugin) handleGetOHLCV(params map[string]any) plugin.Response {
	ohlcvParams := ex.GetOHLCVParamsFromMap(params)

	if ohlcvParams.Market.Symbol == "" && ohlcvParams.Market.Base == "" {
		return plugin.ErrorResponseMsg("market.symbol or market.base is required")
	}

	if err := p.ensureRates(); err != nil {
		return plugin.ErrorResponse(err)
	}

	// Determine the currency from symbol or base
	currency := ohlcvParams.Market.Base
	if currency == "" {
		parts := strings.Split(ohlcvParams.Market.Symbol, "/")
		if len(parts) == 2 {
			currency = parts[0]
		} else {
			currency = ohlcvParams.Market.Symbol
		}
	}

	currencyRates, ok := p.rates[currency]
	if !ok {
		return plugin.ErrorResponseMsg(fmt.Sprintf("no rates available for currency %s", currency))
	}

	// Collect all year/month combinations, sorted
	type yearMonth struct {
		year  int
		month time.Month
	}
	var periods []yearMonth

	for year, months := range currencyRates {
		for month := range months {
			periods = append(periods, yearMonth{year, month})
		}
	}
	sort.Slice(periods, func(i, j int) bool {
		if periods[i].year != periods[j].year {
			return periods[i].year < periods[j].year
		}
		return periods[i].month < periods[j].month
	})

	var records []trading.OHLCVRecord

	for _, ym := range periods {
		// Monthly candle timestamp = first day of the month at 00:00 UTC
		t := time.Date(ym.year, ym.month, 1, 0, 0, 0, 0, time.UTC)

		if ohlcvParams.StartTime != nil && t.Before(*ohlcvParams.StartTime) {
			continue
		}
		if ohlcvParams.EndTime != nil && t.After(*ohlcvParams.EndTime) {
			continue
		}

		rawRate := currencyRates[ym.year][ym.month]

		// The raw rate is "1 EUR = rawRate CURRENCY".
		// We want CURRENCY/EUR, so we invert: 1/rawRate.
		invertedRate, err := invertRate(rawRate)
		if err != nil {
			log.WarnWithData("Failed to invert rate", map[string]any{
				"currency": currency,
				"year":     ym.year,
				"month":    int(ym.month),
				"rawRate":  rawRate,
				"error":    err.Error(),
			})
			continue
		}

		records = append(records, trading.OHLCVRecord{
			OpenTime: t.Unix(),
			Open:     invertedRate,
			High:     invertedRate,
			Low:      invertedRate,
			Close:    invertedRate,
			Volume:   "0",
			IsClosed: true,
		})

		if ohlcvParams.Limit > 0 && len(records) >= ohlcvParams.Limit {
			break
		}
	}

	return plugin.SuccessResponse(records)
}

// ============================================================================
// Rate loading
// ============================================================================

func (p *BMFRatesPlugin) ensureRates() error {
	if p.loaded {
		return nil
	}

	os.MkdirAll(dataDir, 0755)

	now, err := wasmutils.Now()
	if err != nil {
		return fmt.Errorf("failed to get current time: %w", err)
	}

	maxYear := now.Year()

	for year := firstYear; year <= maxYear; year++ {
		if err := p.loadYear(year); err != nil {
			log.WarnWithData("Failed to load rates for year", map[string]any{
				"year":  year,
				"error": err.Error(),
			})
		}
	}

	p.loaded = true
	return nil
}

func (p *BMFRatesPlugin) loadYear(year int) error {
	filename := fmt.Sprintf("bmf_rates_%d.csv", year)
	filePath := filepath.Join(dataDir, filename)

	// Try cached file first
	csvData, err := os.ReadFile(filePath)
	if err != nil || len(csvData) == 0 {
		// Download from BMF
		csvData, err = p.downloadYear(year)
		if err != nil {
			return fmt.Errorf("failed to download rates for %d: %w", year, err)
		}

		// Cache to disk
		if writeErr := os.WriteFile(filePath, csvData, 0644); writeErr != nil {
			log.WarnWithData("Failed to cache CSV file", map[string]any{
				"path":  filePath,
				"error": writeErr.Error(),
			})
		}
	}

	return p.parseCSV(csvData, year)
}

func (p *BMFRatesPlugin) downloadYear(year int) ([]byte, error) {
	url := fmt.Sprintf(urlTpl, year)

	resp, err := p.req.Send(&rt.Request{
		URL:    url,
		Method: "GET",
		Headers: map[string]string{
			"Accept":     "text/csv,text/plain,*/*",
			"User-Agent": "Mozilla/5.0 (compatible; PlusEV Terminal)",
		},
	}, nil)

	if err != nil {
		return nil, err
	}

	if resp.Status != 200 {
		return nil, fmt.Errorf("HTTP %d from BMF for year %d", resp.Status, year)
	}

	return resp.Body, nil
}

func (p *BMFRatesPlugin) parseCSV(data []byte, year int) error {
	// Remove \r (BMF CSVs have Windows line endings)
	data = bytes.ReplaceAll(data, []byte("\r"), []byte(""))

	reader := csv.NewReader(bytes.NewReader(data))
	reader.Comma = ';'
	reader.LazyQuotes = true
	reader.FieldsPerRecord = -1

	records, err := reader.ReadAll()
	if err != nil {
		return fmt.Errorf("CSV parse error: %w", err)
	}

	for i, row := range records {
		// First rows are headers (title row + column headers)
		if i < 2 {
			continue
		}

		month := 1

		for c, txt := range row {
			// First two columns are country name and currency description
			if c < 2 {
				continue
			}

			txt = strings.ReplaceAll(txt, "\n", " ")
			txt = strings.TrimSpace(txt)

			if txt == "" {
				month++
				continue
			}

			matches := pricePattern.FindStringSubmatch(txt)
			if matches == nil {
				month++
				continue
			}

			// matches[1] = rate with German formatting (e.g. "1,6422" or "17.049,43")
			// matches[2] = currency code (e.g. "AUD", "IDR")
			rateStr := matches[1]
			currency := matches[2]

			// Convert German number format to standard:
			// "17.049,43" -> remove dots (thousand separators), comma -> dot
			rateStr = strings.ReplaceAll(rateStr, ".", "")
			rateStr = strings.ReplaceAll(rateStr, ",", ".")

			if _, ok := p.rates[currency]; !ok {
				p.rates[currency] = make(map[int]map[time.Month]string)
			}
			if _, ok := p.rates[currency][year]; !ok {
				p.rates[currency][year] = make(map[time.Month]string)
			}

			p.rates[currency][year][time.Month(month)] = rateStr
			month++
		}
	}

	return nil
}

// ============================================================================
// Helpers
// ============================================================================

// invertRate computes 1/rate as a string with high precision.
// rate is a decimal string like "1.6422".
func invertRate(rate string) (string, error) {
	f, err := strconv.ParseFloat(rate, 64)
	if err != nil {
		return "", fmt.Errorf("invalid rate %q: %w", rate, err)
	}
	if f == 0 {
		return "", fmt.Errorf("cannot invert zero rate")
	}
	inverted := 1.0 / f
	return strconv.FormatFloat(inverted, 'f', 8, 64), nil
}
