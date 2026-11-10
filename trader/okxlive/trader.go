// Package okxlive implements types.Trader against the OKX v5 private REST API.
// It supports both live accounts and the OKX demo-trading environment
// (Config.Demo sets the x-simulated-trading header).
//
// The broker assumes the OKX account is in hedge (long/short) position mode
// and cross margin by default; New() attempts to switch the account into
// hedge mode when needed.
package okxlive

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"nofx/logger"
	"nofx/market"
	"nofx/security"
	"nofx/trader/types"
)

const (
	defaultBaseURL = "https://www.okx.com"

	pathBalance        = "/api/v5/account/balance"
	pathPositions      = "/api/v5/account/positions"
	pathPositionHist   = "/api/v5/account/positions-history"
	pathAccountConfig  = "/api/v5/account/config"
	pathSetPosition    = "/api/v5/account/set-position-mode"
	pathSetLeverage    = "/api/v5/account/set-leverage"
	pathOrder          = "/api/v5/trade/order"
	pathOrderDetail    = "/api/v5/trade/order"
	pathClosePosition  = "/api/v5/trade/close-position"
	pathAlgoOrder      = "/api/v5/trade/order-algo"
	pathCancelAlgos    = "/api/v5/trade/cancel-algos"
	pathAlgoPending    = "/api/v5/trade/orders-algo-pending"
	pathAlgoHistory    = "/api/v5/trade/orders-algo-history"
	pathTicker         = "/api/v5/market/ticker"
	pathInstruments    = "/api/v5/public/instruments"
)

// Config configures the OKX live broker.
type Config struct {
	APIKey       string
	SecretKey    string
	Passphrase   string
	BaseURL      string // defaults to https://www.okx.com
	Demo         bool   // OKX demo-trading environment (x-simulated-trading: 1)
	CrossMargin  bool   // default true
	HTTPTimeout  time.Duration
}

// Broker trades USDT perpetual swaps on OKX.
// Compile-time check: implements the unified Trader interface.
var _ types.Trader = (*Broker)(nil)

type Broker struct {
	apiKey       string
	secretKey    string
	passphrase   string
	baseURL      string
	demo         bool
	crossMargin  bool
	client       *http.Client

	instMu       sync.Mutex
	instruments  map[string]*instrument

	posHistSince int64 // unix seconds: only report positions closed after startup / last check

	closeMu       sync.Mutex
	initiatedLive map[string]int64 // "instId|posSide" -> unix ms of broker-initiated close
}

// New validates the account and returns a broker. It requires a hedge
// (long/short) position mode and switches the account when possible.
func New(cfg Config) (*Broker, error) {
	if cfg.APIKey == "" || cfg.SecretKey == "" || cfg.Passphrase == "" {
		return nil, fmt.Errorf("OKX_API_KEY / OKX_SECRET_KEY / OKX_PASSPHRASE 均为必填")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	if cfg.HTTPTimeout <= 0 {
		cfg.HTTPTimeout = 30 * time.Second
	}
	b := &Broker{
		apiKey:        cfg.APIKey,
		secretKey:     cfg.SecretKey,
		passphrase:    cfg.Passphrase,
		baseURL:       strings.TrimRight(cfg.BaseURL, "/"),
		demo:          cfg.Demo,
		crossMargin:   cfg.CrossMargin,
		client:        security.SafeHTTPClient(cfg.HTTPTimeout),
		instruments:   make(map[string]*instrument),
		posHistSince:  time.Now().Unix(),
		initiatedLive: make(map[string]int64),
	}

	if err := b.ensureHedgeMode(); err != nil {
		return nil, err
	}
	mode := "实盘"
	if b.demo {
		mode = "模拟盘 (demo)"
	}
	logger.Infof("✅ [OKX Live] broker ready: %s, margin=%s, base=%s", mode, b.marginMode(), b.baseURL)
	return b, nil
}

func (b *Broker) marginMode() string {
	if b.crossMargin {
		return "cross"
	}
	return "isolated"
}

// ensureHedgeMode reads the account position mode and switches to
// long_short_mode when the account is in net mode and has no open positions.
func (b *Broker) ensureHedgeMode() error {
	var configs []struct {
		PosMode string `json:"posMode"`
	}
	if err := b.get(pathAccountConfig, &configs); err != nil {
		return fmt.Errorf("读取 OKX 账户配置失败: %w", err)
	}
	if len(configs) == 0 {
		return fmt.Errorf("OKX 账户配置为空")
	}
	if configs[0].PosMode == "long_short_mode" {
		return nil
	}

	body := map[string]string{"posMode": "long_short_mode"}
	err := b.post(pathSetPosition, body)
	if err != nil && !strings.Contains(err.Error(), "already") &&
		!strings.Contains(err.Error(), "not modified") {
		return fmt.Errorf("账户当前为 %s 模式，切换到双向持仓失败: %v（若账户有持仓/挂单请先平掉，或手动在 OKX 网页切换后重试）",
			configs[0].PosMode, err)
	}
	logger.Infof("  ✓ [OKX Live] 已切换为双向持仓模式 (long_short_mode)")
	return nil
}

// ============================================================================
// Signed HTTP plumbing
// ============================================================================

func (b *Broker) sign(timestamp, method, requestPath, body string) string {
	preHash := timestamp + method + requestPath + body
	h := hmac.New(sha256.New, []byte(b.secretKey))
	h.Write([]byte(preHash))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func (b *Broker) do(method, path string, body any) ([]byte, error) {
	var bodyBytes []byte
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("serialize request: %w", err)
		}
		bodyBytes = buf
	}

	timestamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	signature := b.sign(timestamp, method, path, string(bodyBytes))

	req, err := http.NewRequest(method, b.baseURL+path, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("OK-ACCESS-KEY", b.apiKey)
	req.Header.Set("OK-ACCESS-SIGN", signature)
	req.Header.Set("OK-ACCESS-TIMESTAMP", timestamp)
	req.Header.Set("OK-ACCESS-PASSPHRASE", b.passphrase)
	req.Header.Set("Content-Type", "application/json")
	if b.demo {
		req.Header.Set("x-simulated-trading", "1")
	}

	// Signed credentials must never be forwarded to a redirect destination.
	client := *b.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s failed: %w", path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read response %s: %w", path, err)
	}

	var envelope struct {
		Code string          `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, fmt.Errorf("decode response %s: %w (%s)", path, err, truncateForLog(respBody))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || envelope.Code != "0" {
		return nil, fmt.Errorf("OKX %s (code %s): %s", path, envelope.Code, envelope.Msg)
	}
	return envelope.Data, nil
}

func (b *Broker) get(path string, out any) error {
	data, err := b.do(http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func (b *Broker) post(path string, body any, out ...any) error {
	data, err := b.do(http.MethodPost, path, body)
	if err != nil {
		return err
	}
	if len(out) > 0 && out[0] != nil {
		return json.Unmarshal(data, out[0])
	}
	return nil
}

func truncateForLog(b []byte) string {
	s := string(b)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// ============================================================================
// Instruments & sizing
// ============================================================================

type instrument struct {
	InstID   string  `json:"instId"`
	CtVal    float64 `json:"-"` // contract value (base asset per contract)
	LotSz    float64 `json:"-"` // contract quantity step
	MinSz    float64 `json:"-"` // minimum contracts
	MaxMktSz float64 `json:"-"` // max market order contracts
}

func (b *Broker) getInstrument(symbol string) (*instrument, error) {
	instID := market.ToOKXInstID(symbol)

	b.instMu.Lock()
	if inst, ok := b.instruments[instID]; ok {
		b.instMu.Unlock()
		return inst, nil
	}
	b.instMu.Unlock()

	var rows []struct {
		InstID   string `json:"instId"`
		CtVal    string `json:"ctVal"`
		LotSz    string `json:"lotSz"`
		MinSz    string `json:"minSz"`
		MaxMktSz string `json:"maxMktSz"`
		State    string `json:"state"`
	}
	path := fmt.Sprintf("%s?instType=SWAP&instId=%s", pathInstruments, instID)
	if err := b.get(path, &rows); err != nil {
		return nil, fmt.Errorf("获取合约信息失败: %w", err)
	}
	if len(rows) != 1 {
		return nil, fmt.Errorf("合约不存在: %s", instID)
	}
	if rows[0].State != "live" {
		return nil, fmt.Errorf("合约未在交易状态: %s (%s)", instID, rows[0].State)
	}

	inst := &instrument{
		InstID:    rows[0].InstID,
		CtVal:     parseFloat(rows[0].CtVal),
		LotSz:     parseFloat(rows[0].LotSz),
		MinSz:     parseFloat(rows[0].MinSz),
		MaxMktSz:  parseFloat(rows[0].MaxMktSz),
	}
	if inst.CtVal <= 0 || inst.LotSz <= 0 || inst.MinSz <= 0 {
		return nil, fmt.Errorf("合约 %s 参数异常: ctVal=%s lotSz=%s minSz=%s", instID, rows[0].CtVal, rows[0].LotSz, rows[0].MinSz)
	}

	b.instMu.Lock()
	b.instruments[instID] = inst
	b.instMu.Unlock()
	return inst, nil
}

// contractsForQuantity converts a base-asset quantity into OKX contract count,
// floored to the lot size. Returns an error when below the minimum.
func contractsForQuantity(quantity float64, inst *instrument) (float64, error) {
	contracts := quantity / inst.CtVal
	if inst.LotSz > 0 {
		contracts = math.Floor(contracts/inst.LotSz) * inst.LotSz
	}
	if contracts < inst.MinSz {
		return 0, fmt.Errorf("仓位 %.8f 不足最小下单量 (%d 张 × ctVal %g, 需 ≥ %.8f)",
			quantity, int64(inst.MinSz), inst.CtVal, inst.MinSz*inst.CtVal)
	}
	return contracts, nil
}

// formatContracts renders the contract count with lot-size precision.
func formatContracts(contracts float64, inst *instrument) string {
	if inst.LotSz >= 1 {
		return fmt.Sprintf("%.0f", contracts)
	}
	lotStr := strings.TrimRight(fmt.Sprintf("%f", inst.LotSz), "0")
	dot := strings.Index(lotStr, ".")
	precision := 0
	if dot >= 0 {
		precision = len(lotStr) - dot - 1
	}
	return fmt.Sprintf("%.*f", precision, contracts)
}

func parseFloat(s string) float64 {
	var f float64
	fmt.Sscanf(strings.TrimSpace(s), "%g", &f)
	return f
}
