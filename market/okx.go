package market

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// okxBaseURL is the OKX public REST base URL (overridable via OKX_BASE_URL).
var okxBaseURL = func() string {
	if v := os.Getenv("OKX_BASE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://www.okx.com"
}()

var okxHTTPClient = &http.Client{Timeout: 15 * time.Second}

// okxInstrumentInfo holds contract metadata used for position sizing.
type okxInstrumentInfo struct {
	CtVal  float64 // contract value (base asset per contract)
	LotSz  float64 // lot size (contracts)
	TickSz float64 // price tick size
}

var (
	okxInstrumentCache   = map[string]okxInstrumentInfo{}
	okxInstrumentCacheMu sync.Mutex
)

// ToOKXInstID converts an internal symbol (BTCUSDT) to an OKX SWAP instrument id (BTC-USDT-SWAP).
// Symbols already in OKX form are returned unchanged.
func ToOKXInstID(symbol string) string {
	s := strings.ToUpper(strings.TrimSpace(symbol))
	if strings.HasSuffix(s, "-SWAP") {
		return s
	}
	s = strings.TrimSuffix(s, "USDT")
	if strings.Contains(s, "-") {
		// Already hyphenated (e.g. BTC-USDT) — just append -SWAP
		return s + "-SWAP"
	}
	return s + "-USDT-SWAP"
}

// FromOKXInstID converts an OKX instrument id back to internal symbol form (BTCUSDT).
func FromOKXInstID(instID string) string {
	s := strings.ToUpper(strings.TrimSpace(instID))
	s = strings.TrimSuffix(s, "-SWAP")
	s = strings.ReplaceAll(s, "-", "")
	if !strings.HasSuffix(s, "USDT") {
		s += "USDT"
	}
	return s
}

// okxBar maps an internal timeframe (3m, 4h, 1d) to an OKX bar value (3m, 4H, 1D).
func okxBar(timeframe string) string {
	tf := strings.ToLower(strings.TrimSpace(timeframe))
	switch {
	case strings.HasSuffix(tf, "m"):
		return tf
	case strings.HasSuffix(tf, "h"):
		return strings.ToUpper(tf)
	case strings.HasSuffix(tf, "d"):
		return strings.ToUpper(tf)
	case strings.HasSuffix(tf, "w"):
		return strings.ToUpper(tf)
	default:
		return tf
	}
}

// okxResponse is the common OKX v5 response envelope.
type okxResponse struct {
	Code string          `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// okxGet performs a GET request against the OKX public REST API and decodes data into out.
func okxGet(path string, query url.Values, out interface{}) error {
	u := okxBaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	resp, err := okxHTTPClient.Get(u)
	if err != nil {
		return fmt.Errorf("okx request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("okx api returned HTTP %d: %s", resp.StatusCode, truncateBody(body))
	}

	var envelope okxResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("okx api decode failed: %w", err)
	}
	if envelope.Code != "0" {
		return fmt.Errorf("okx api error %s: %s", envelope.Code, envelope.Msg)
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("okx api data decode failed: %w", err)
	}
	return nil
}

func truncateBody(body []byte) string {
	s := string(body)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// okxGetKlines fetches OHLCV klines for an instrument (ascending by time).
// OKX returns newest-first; the result is reversed to oldest-first.
func okxGetKlines(instID, bar string, limit int) ([]Kline, error) {
	if limit > 300 {
		limit = 300 // OKX candles max is 300 (100 default)
	}
	q := url.Values{}
	q.Set("instId", instID)
	q.Set("bar", bar)
	q.Set("limit", strconv.Itoa(limit))

	var rows [][]interface{}
	if err := okxGet("/api/v5/market/candles", q, &rows); err != nil {
		return nil, err
	}

	klines := make([]Kline, 0, len(rows))
	for _, row := range rows {
		if len(row) < 6 {
			continue
		}
		ts, _ := strconv.ParseInt(fmt.Sprint(row[0]), 10, 64)
		open, _ := parseFloat(row[1])
		high, _ := parseFloat(row[2])
		low, _ := parseFloat(row[3])
		close, _ := parseFloat(row[4])
		volume, _ := parseFloat(row[5])

		// Bar interval in ms for CloseTime estimation
		klines = append(klines, Kline{
			OpenTime:  ts,
			Open:      open,
			High:      high,
			Low:       low,
			Close:     close,
			Volume:    volume,
			CloseTime: ts + barMillis(bar) - 1,
		})
	}

	// OKX returns newest-first; reverse to oldest-first
	for i, j := 0, len(klines)-1; i < j; i, j = i+1, j-1 {
		klines[i], klines[j] = klines[j], klines[i]
	}
	return klines, nil
}

// barMillis approximates the interval of a bar in milliseconds.
func barMillis(bar string) int64 {
	bar = strings.ToLower(bar)
	if len(bar) < 2 {
		return 180000
	}
	n, err := strconv.Atoi(bar[:len(bar)-1])
	if err != nil || n <= 0 {
		return 180000
	}
	switch bar[len(bar)-1] {
	case 'm':
		return int64(n) * 60 * 1000
	case 'h':
		return int64(n) * 3600 * 1000
	case 'd':
		return int64(n) * 86400 * 1000
	case 'w':
		return int64(n) * 7 * 86400 * 1000
	default:
		return 180000
	}
}

// okxGetTicker fetches the latest price for an instrument.
func okxGetTicker(instID string) (float64, error) {
	q := url.Values{}
	q.Set("instId", instID)

	var tickers []struct {
		Last string `json:"last"`
	}
	if err := okxGet("/api/v5/market/ticker", q, &tickers); err != nil {
		return 0, err
	}
	if len(tickers) == 0 {
		return 0, fmt.Errorf("okx ticker empty for %s", instID)
	}
	price, err := strconv.ParseFloat(tickers[0].Last, 64)
	if err != nil || math.IsNaN(price) || price <= 0 {
		return 0, fmt.Errorf("okx ticker invalid price for %s", instID)
	}
	return price, nil
}

// okxGetFundingRate fetches the current funding rate for a SWAP instrument.
func okxGetFundingRate(instID string) (float64, error) {
	q := url.Values{}
	q.Set("instId", instID)

	var rates []struct {
		InstId      string `json:"instId"`
		FundingRate string `json:"fundingRate"`
	}
	if err := okxGet("/api/v5/public/funding-rate", q, &rates); err != nil {
		return 0, err
	}
	if len(rates) == 0 {
		return 0, fmt.Errorf("okx funding rate empty for %s", instID)
	}
	rate, err := strconv.ParseFloat(rates[0].FundingRate, 64)
	if err != nil {
		return 0, fmt.Errorf("okx funding rate invalid for %s", instID)
	}
	return rate, nil
}

// okxGetOpenInterest fetches open interest for a SWAP instrument, in base-asset units (oiCcy).
func okxGetOpenInterest(instID string) (float64, error) {
	q := url.Values{}
	q.Set("instType", "SWAP")
	q.Set("instId", instID)

	var ois []struct {
		InstId  string `json:"instId"`
		Oi      string `json:"oi"`
		OiCcy   string `json:"oiCcy"`
		OiUsd   string `json:"oiUsd"`
		CtVal   string `json:"ctVal"`
	}
	if err := okxGet("/api/v5/public/open-interest", q, &ois); err != nil {
		return 0, err
	}
	if len(ois) == 0 {
		return 0, fmt.Errorf("okx open interest empty for %s", instID)
	}
	if v, err := strconv.ParseFloat(ois[0].OiCcy, 64); err == nil && v > 0 {
		return v, nil
	}
	// Fallback: contracts * contract value
	contracts, err1 := strconv.ParseFloat(ois[0].Oi, 64)
	ctVal, err2 := strconv.ParseFloat(ois[0].CtVal, 64)
	if err1 != nil || err2 != nil || contracts <= 0 || ctVal <= 0 {
		return 0, fmt.Errorf("okx open interest invalid for %s", instID)
	}
	return contracts * ctVal, nil
}

// GetInstrumentInfo returns OKX contract metadata (ctVal/lotSz/tickSz), cached.
func GetInstrumentInfo(symbol string) (okxInstrumentInfo, error) {
	instID := ToOKXInstID(symbol)

	okxInstrumentCacheMu.Lock()
	if info, ok := okxInstrumentCache[instID]; ok {
		okxInstrumentCacheMu.Unlock()
		return info, nil
	}
	okxInstrumentCacheMu.Unlock()

	q := url.Values{}
	q.Set("instType", "SWAP")
	q.Set("instId", instID)

	var instruments []struct {
		InstId string `json:"instId"`
		CtVal  string `json:"ctVal"`
		LotSz  string `json:"lotSz"`
		TickSz string `json:"tickSz"`
		CtType string `json:"ctType"`
	}
	if err := okxGet("/api/v5/public/instruments", q, &instruments); err != nil {
		return okxInstrumentInfo{}, err
	}
	if len(instruments) == 0 {
		return okxInstrumentInfo{}, fmt.Errorf("okx instrument not found: %s", instID)
	}

	ctVal, _ := strconv.ParseFloat(instruments[0].CtVal, 64)
	lotSz, _ := strconv.ParseFloat(instruments[0].LotSz, 64)
	tickSz, _ := strconv.ParseFloat(instruments[0].TickSz, 64)

	info := okxInstrumentInfo{CtVal: ctVal, LotSz: lotSz, TickSz: tickSz}
	if info.CtVal <= 0 || info.LotSz <= 0 {
		return okxInstrumentInfo{}, fmt.Errorf("okx instrument invalid metadata for %s", instID)
	}

	okxInstrumentCacheMu.Lock()
	okxInstrumentCache[instID] = info
	okxInstrumentCacheMu.Unlock()
	return info, nil
}
