package trader

import (
	"fmt"
	"nofx/kernel"
	"nofx/logger"
	"nofx/mcp"
	"nofx/store"
	"nofx/trader/types"
	"strings"
	"sync"
	"time"
)

// Notifier delivers trade notifications (Telegram).
type Notifier interface {
	Send(message string) error
}

// SetOpensPaused toggles opening new positions (existing positions keep being
// managed). Wired to the Telegram /pause and /resume commands.
func (at *AutoTrader) SetOpensPaused(paused bool) {
	at.opensPausedMu.Lock()
	at.opensPaused = paused
	at.opensPausedMu.Unlock()
	logger.Infof("⏸ [Trader %s] opens paused: %v", at.id, paused)
}

// OpensPaused reports whether opening new positions is paused.
func (at *AutoTrader) OpensPaused() bool {
	at.opensPausedMu.RLock()
	defer at.opensPausedMu.RUnlock()
	return at.opensPaused
}

// Config auto trader configuration.
type Config struct {
	TraderID     string
	Store        *store.Store
	Strategy     *store.StrategyConfig
	ScanInterval time.Duration
	Notifier     Notifier
	AIClient     mcp.AIClient
}

// AutoTrader runs the AI decision loop against a broker (paper).
type AutoTrader struct {
	id           string
	store        *store.Store
	trader       types.Trader
	aiClient     mcp.AIClient
	engineMu     sync.RWMutex // guards engine (hot-swapped by Telegram /strategy updates)
	engine       *kernel.StrategyEngine
	notifier     Notifier
	scanInterval time.Duration

	startTime time.Time
	callCount int
	isRunning bool

	opensPausedMu sync.RWMutex
	opensPaused   bool // set via Telegram /pause; existing positions still managed

	runtimeHealthMu     sync.RWMutex
	safeMode            bool
	safeModeReason      string
	consecutiveFailures int

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewAutoTrader creates an auto trader.
func NewAutoTrader(cfg Config, trader types.Trader) *AutoTrader {
	if cfg.ScanInterval <= 0 {
		cfg.ScanInterval = 5 * time.Minute
	}
	engine := kernel.NewStrategyEngine(cfg.Strategy)
	return &AutoTrader{
		id:           cfg.TraderID,
		store:        cfg.Store,
		trader:       trader,
		aiClient:     cfg.AIClient,
		engine:       engine,
		notifier:     cfg.Notifier,
		scanInterval: cfg.ScanInterval,
		stopCh:       make(chan struct{}),
	}
}

// Start launches the trading loop.
func (at *AutoTrader) Start() error {
	if at.isRunning {
		return fmt.Errorf("auto trader is already running")
	}
	at.isRunning = true
	at.startTime = time.Now()

	at.wg.Add(1)
	go func() {
		defer at.wg.Done()
		logger.Infof("🤖 [Trader %s] AI trading loop started (interval: %s)", at.id, at.scanInterval)
		at.notify(fmt.Sprintf("🤖 *NOFX 模拟交易已启动*\n\n扫描周期: %s\n候选币种: %s",
			at.scanInterval, strings.Join(at.staticCoins(), ", ")))
		at.Run()
	}()
	return nil
}

// Stop gracefully stops the trading loop.
func (at *AutoTrader) Stop() {
	if !at.isRunning {
		return
	}
	logger.Infof("⏹ [Trader %s] Stopping AI trading loop...", at.id)
	close(at.stopCh)
	at.wg.Wait()
	at.isRunning = false
	at.notify("📴 *NOFX 模拟交易已停止*")
	logger.Infof("✅ [Trader %s] AI trading loop stopped", at.id)
}

// GetStatus returns trader runtime status.
func (at *AutoTrader) GetStatus() map[string]interface{} {
	at.runtimeHealthMu.RLock()
	safeMode := at.safeMode
	safeModeReason := at.safeModeReason
	failures := at.consecutiveFailures
	at.runtimeHealthMu.RUnlock()

	status := map[string]interface{}{
		"id":                   at.id,
		"is_running":           at.isRunning,
		"start_time":           at.startTime,
		"call_count":           at.callCount,
		"scan_interval":        at.scanInterval.String(),
		"safe_mode":            safeMode,
		"consecutive_failures": failures,
	}
	if safeMode {
		status["safe_mode_reason"] = safeModeReason
	}
	return status
}

// GetAccountInfo returns the current account snapshot.
func (at *AutoTrader) GetAccountInfo() (map[string]interface{}, error) {
	return at.trader.GetBalance()
}

// GetPositions returns all open positions.
func (at *AutoTrader) GetPositions() ([]map[string]interface{}, error) {
	return at.trader.GetPositions()
}

// currentEngine returns the active strategy engine (hot-swappable).
func (at *AutoTrader) currentEngine() *kernel.StrategyEngine {
	at.engineMu.RLock()
	defer at.engineMu.RUnlock()
	return at.engine
}

// UpdateStrategy hot-swaps the strategy engine with a new configuration.
// The next decision cycle picks it up; running cycles finish on the old one.
func (at *AutoTrader) UpdateStrategy(cfg *store.StrategyConfig) {
	if cfg == nil {
		return
	}
	cfg.ClampLimits()
	at.engineMu.Lock()
	at.engine = kernel.NewStrategyEngine(cfg)
	at.engineMu.Unlock()
	k := cfg
	logger.Infof("🔄 [Trader %s] strategy updated: coins=%v timeframes=%v maxPositions=%d minConfidence=%d minPositionSize=%.1f",
		at.id, k.CoinSource.StaticCoins, k.Indicators.Klines.SelectedTimeframes,
		k.RiskControl.MaxPositions, k.RiskControl.MinConfidence, k.RiskControl.MinPositionSize)
}

// staticCoins returns the configured static candidate coins.
func (at *AutoTrader) staticCoins() []string {
	coins := make([]string, 0)
	for _, c := range at.currentEngine().GetConfig().CoinSource.StaticCoins {
		s := strings.ToUpper(strings.TrimSpace(c))
		if s != "" && s != "USDT" {
			coins = append(coins, s)
		}
	}
	return coins
}

// notify sends a message through the notifier, logging send failures.
func (at *AutoTrader) notify(message string) {
	if at.notifier == nil {
		return
	}
	if err := at.notifier.Send(message); err != nil {
		logger.Warnf("⚠️ [Trader %s] notification failed: %v", at.id, err)
	}
}
