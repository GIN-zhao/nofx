package trader

import (
	"fmt"
	"math"
	"nofx/kernel"
	"nofx/logger"
	"nofx/market"
	"nofx/trader/types"
	"sort"
	"strings"
	"time"
)

// Safe mode: after this many consecutive failed cycles the loop pauses and
// retries periodically instead of hammering a broken AI/market setup.
const (
	safeModeFailureThreshold = 3
	safeModeRetryDelay       = 10 * time.Minute
	maxRecentOrdersInPrompt  = 5
)

// Run is the main trading loop: one AI decision cycle per scan interval.
func (at *AutoTrader) Run() {
	at.runCycleSafely() // first cycle runs immediately

	ticker := time.NewTicker(at.scanInterval)
	defer ticker.Stop()

	var nextRetry time.Time
	for {
		select {
		case <-at.stopCh:
			return
		case <-ticker.C:
			if safe, _ := at.safeModeState(); safe {
				if time.Now().Before(nextRetry) {
					continue
				}
				nextRetry = time.Now().Add(safeModeRetryDelay)
			}
			at.runCycleSafely()
		}
	}
}

// runCycleSafely wraps a cycle with safe-mode bookkeeping.
func (at *AutoTrader) runCycleSafely() {
	err := at.runCycle()
	at.runtimeHealthMu.Lock()
	if err != nil {
		at.consecutiveFailures++
		if at.consecutiveFailures >= safeModeFailureThreshold && !at.safeMode {
			at.safeMode = true
			at.safeModeReason = err.Error()
			at.runtimeHealthMu.Unlock()
			logger.Errorf("🛑 [Trader %s] Entering safe mode after %d consecutive failures: %v",
				at.id, at.consecutiveFailures, err)
			at.notify(fmt.Sprintf("🛑 *进入安全模式*\n\n连续 %d 次周期失败:\n`%s`\n\n每 %s 重试一次",
				at.consecutiveFailures, err.Error(), safeModeRetryDelay))
			return
		}
		at.runtimeHealthMu.Unlock()
		logger.Warnf("⚠️ [Trader %s] Cycle failed: %v", at.id, err)
		return
	}
	wasSafeMode := at.safeMode
	at.consecutiveFailures = 0
	at.safeMode = false
	at.safeModeReason = ""
	at.runtimeHealthMu.Unlock()
	if wasSafeMode {
		logger.Infof("✅ [Trader %s] Cycle recovered, leaving safe mode", at.id)
		at.notify("✅ *交易周期恢复正常，退出安全模式*")
	}
}

// runCycle runs one full AI decision cycle.
func (at *AutoTrader) runCycle() error {
	cycleStart := time.Now()
	at.callCount++

	logger.Info("\n" + strings.Repeat("=", 70))
	logger.Infof("⏰ %s - AI decision cycle #%d", time.Now().Format("2006-01-02 15:04:05"), at.callCount)
	logger.Info(strings.Repeat("=", 70))

	// 1. SL/TP/liquidation check before anything else.
	at.checkStops()

	// 2. Build the trading context from the live ledger + market data.
	ctx, err := at.buildTradingContext()
	if err != nil {
		return fmt.Errorf("failed to build trading context: %w", err)
	}

	// 3. Ask the AI for decisions.
	full, err := kernel.GetFullDecisionWithStrategy(ctx, at.aiClient, at.currentEngine(), "")
	if err != nil {
		at.saveFailedDecision(ctx, err)
		return fmt.Errorf("AI decision failed: %w", err)
	}

	// 4. Persist the decision + an equity snapshot.
	cycleNumber := at.nextCycleNumber()
	at.saveDecision(cycleNumber, full, ctx)
	at.saveEquitySnapshot()

	// 5. Execute the decisions (closes first, then opens).
	decisions := filterDecisionsToUniverse(full.Decisions, ctx)
	sortDecisionsByPriority(decisions)
	for i := range decisions {
		decision := decisions[i]
		if decision.Action == "hold" || decision.Action == "wait" {
			continue
		}
		if reason := at.tradeThrottleReason(decision, ctx); reason != "" {
			logger.Infof("🚦 [Trader %s] %s %s skipped: %s", at.id, decision.Action, decision.Symbol, reason)
			continue
		}
		if err := at.executeDecisionWithRecord(&decision); err != nil {
			logger.Errorf("❌ [Trader %s] Execute %s %s failed: %v", at.id, decision.Action, decision.Symbol, err)
		}
	}

	logger.Infof("✅ Cycle #%d completed in %s", at.callCount, time.Since(cycleStart).Round(time.Second))
	return nil
}

// checkStops lets the broker trigger SL/TP/liquidation closes and notifies.
func (at *AutoTrader) checkStops() {
	checker, ok := at.trader.(interface {
		CheckStops() ([]types.ClosedPnLRecord, error)
	})
	if !ok {
		return
	}
	events, err := checker.CheckStops()
	if err != nil {
		logger.Warnf("⚠️ [Trader %s] Stop check failed: %v", at.id, err)
		return
	}
	for _, record := range events {
		logger.Infof("🔔 [Trader %s] %s triggered on %s: PnL %.2f", at.id, record.CloseType, record.Symbol, record.RealizedPnL)
		at.notify(formatCloseMessage(record.Symbol, record.Side, record.ExitPrice, record.RealizedPnL,
			record.Leverage, record.CloseType, ""))
	}
}

// buildTradingContext assembles everything the AI sees.
func (at *AutoTrader) buildTradingContext() (*kernel.Context, error) {
	balance, err := at.trader.GetBalance()
	if err != nil {
		return nil, fmt.Errorf("failed to get balance: %w", err)
	}
	positions, err := at.trader.GetPositions()
	if err != nil {
		return nil, fmt.Errorf("failed to get positions: %w", err)
	}

	equity := safeFloat(balance, "totalEquity")
	available := safeFloat(balance, "availableBalance")
	unrealized := safeFloat(balance, "totalUnrealizedProfit")
	wallet := safeFloat(balance, "totalWalletBalance")

	initial := at.initialBalance()
	marginUsed := 0.0
	now := time.Now().UTC().UnixMilli()

	ctx := &kernel.Context{
		CurrentTime:    time.Now().UTC().Format("2006-01-02 15:04:05 MST"),
		RuntimeMinutes: int(time.Since(at.startTime).Minutes()),
		CallCount:      at.callCount,
	}

	for _, pos := range positions {
		symbol, _ := pos["symbol"].(string)
		side, _ := pos["side"].(string)
		entryPrice := safeFloat(pos, "entryPrice")
		markPrice := safeFloat(pos, "markPrice")
		quantity := safeFloat(pos, "positionAmt")
		pnl := safeFloat(pos, "unRealizedProfit")
		leverage := int(safeFloat(pos, "leverage"))
		if leverage <= 0 {
			leverage = 1
		}
		margin := entryPrice * math.Abs(quantity) / float64(leverage)
		marginUsed += margin

		pnlPct := 0.0
		if margin > 0 {
			pnlPct = pnl / margin * 100
		}

		ctx.Positions = append(ctx.Positions, kernel.PositionInfo{
			Symbol:           market.Normalize(symbol),
			Side:             side,
			EntryPrice:       entryPrice,
			MarkPrice:        markPrice,
			Quantity:         quantity,
			Leverage:         leverage,
			UnrealizedPnL:    pnl,
			UnrealizedPnLPct: pnlPct,
			LiquidationPrice: safeFloat(pos, "liquidationPrice"),
			MarginUsed:       margin,
			UpdateTime:       now,
		})
	}

	totalPnL := equity - initial
	totalPnLPct := 0.0
	if initial > 0 {
		totalPnLPct = totalPnL / initial * 100
	}
	marginUsedPct := 0.0
	if equity > 0 {
		marginUsedPct = marginUsed / equity * 100
	}
	ctx.Account = kernel.AccountInfo{
		TotalEquity:      equity,
		AvailableBalance: available,
		UnrealizedPnL:    unrealized,
		TotalPnL:         totalPnL,
		TotalPnLPct:      totalPnLPct,
		MarginUsed:       marginUsed,
		MarginUsedPct:    marginUsedPct,
		PositionCount:    len(positions),
	}
	_ = wallet

	// Candidate coins come from the strategy's static list.
	for _, coin := range at.staticCoins() {
		ctx.CandidateCoins = append(ctx.CandidateCoins, kernel.CandidateCoin{
			Symbol:  market.Normalize(coin),
			Sources: []string{"static"},
		})
	}

	ctx.TradingStats = at.computeTradingStats()
	ctx.RecentOrders = at.computeRecentOrders()

	return ctx, nil
}

// initialBalance returns the configured starting balance for this trader.
func (at *AutoTrader) initialBalance() float64 {
	trader, err := at.store.Trader().GetByID(at.id)
	if err != nil || trader == nil {
		return 0
	}
	return trader.InitialBalance
}

// computeTradingStats summarizes closed positions for the AI prompt.
func (at *AutoTrader) computeTradingStats() *kernel.TradingStats {
	closed, err := at.store.Position().GetClosedPositions(at.id, 200)
	if err != nil || len(closed) == 0 {
		return nil
	}

	stats := &kernel.TradingStats{TotalTrades: len(closed)}
	wins, losses := 0, 0
	grossWin, grossLoss := 0.0, 0.0
	for _, pos := range closed {
		pnl := pos.RealizedPnL
		stats.TotalPnL += pnl
		if pnl >= 0 {
			wins++
			grossWin += pnl
		} else {
			losses++
			grossLoss += -pnl
		}
	}
	if stats.TotalTrades > 0 {
		stats.WinRate = float64(wins) / float64(stats.TotalTrades) * 100
	}
	if wins > 0 {
		stats.AvgWin = grossWin / float64(wins)
	}
	if losses > 0 {
		stats.AvgLoss = grossLoss / float64(losses)
	}
	if grossLoss > 0 {
		stats.ProfitFactor = grossWin / grossLoss
	}

	// Max drawdown from recent equity snapshots.
	snapshots, err := at.store.Equity().GetLatest(at.id, 500)
	if err == nil && len(snapshots) > 1 {
		peak := snapshots[0].TotalEquity
		for _, snap := range snapshots {
			if snap.TotalEquity > peak {
				peak = snap.TotalEquity
			}
			if peak > 0 {
				dd := (peak - snap.TotalEquity) / peak * 100
				if dd > stats.MaxDrawdownPct {
					stats.MaxDrawdownPct = dd
				}
			}
		}
	}
	return stats
}

// computeRecentOrders converts recently closed positions into prompt history.
func (at *AutoTrader) computeRecentOrders() []kernel.RecentOrder {
	closed, err := at.store.Position().GetClosedPositions(at.id, maxRecentOrdersInPrompt)
	if err != nil {
		return nil
	}
	orders := make([]kernel.RecentOrder, 0, len(closed))
	for _, pos := range closed {
		side := "long"
		if pos.Side == "SHORT" {
			side = "short"
		}
		leverage := float64(pos.Leverage)
		if leverage < 1 {
			leverage = 1
		}
		margin := pos.EntryPrice * pos.EntryQuantity / leverage
		pnlPct := 0.0
		if margin > 0 {
			pnlPct = pos.RealizedPnL / margin * 100
		}
		orders = append(orders, kernel.RecentOrder{
			Symbol:       pos.Symbol,
			Side:         side,
			EntryPrice:   pos.EntryPrice,
			ExitPrice:    pos.ExitPrice,
			RealizedPnL:  pos.RealizedPnL,
			PnLPct:       pnlPct,
			EntryTime:    time.UnixMilli(pos.EntryTime).Format("01-02 15:04"),
			ExitTime:     time.UnixMilli(pos.ExitTime).Format("01-02 15:04"),
			HoldDuration: formatHoldDuration(pos.ExitTime - pos.EntryTime),
		})
	}
	return orders
}

func (at *AutoTrader) nextCycleNumber() int {
	n, err := at.store.Decision().GetLastCycleNumber(at.id)
	if err != nil {
		return at.callCount
	}
	return n + 1
}

// filterDecisionsToUniverse keeps decisions only for coins we actually track
// (candidates or open positions) and dedupes per symbol.
func filterDecisionsToUniverse(decisions []kernel.Decision, ctx *kernel.Context) []kernel.Decision {
	universe := make(map[string]bool)
	for _, coin := range ctx.CandidateCoins {
		universe[market.Normalize(coin.Symbol)] = true
	}
	for _, pos := range ctx.Positions {
		universe[market.Normalize(pos.Symbol)] = true
	}

	seen := make(map[string]bool)
	kept := make([]kernel.Decision, 0, len(decisions))
	for _, d := range decisions {
		if d.Action == "wait" && strings.EqualFold(strings.TrimSpace(d.Symbol), "ALL") {
			kept = append(kept, d)
			continue
		}
		symbol := normalizedDecisionSymbol(d.Symbol)
		if symbol == "" || !universe[symbol] || seen[symbol] {
			logger.Infof("⚠️ Dropping decision for %s (%s): outside strategy universe or duplicated", d.Symbol, d.Action)
			continue
		}
		seen[symbol] = true
		d.Symbol = symbol
		kept = append(kept, d)
	}
	return kept
}

// sortDecisionsByPriority orders closes before opens, higher confidence first.
func sortDecisionsByPriority(decisions []kernel.Decision) {
	rank := func(action string) int {
		switch action {
		case "close_long", "close_short":
			return 0
		case "open_long", "open_short":
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(decisions, func(i, j int) bool {
		ri, rj := rank(decisions[i].Action), rank(decisions[j].Action)
		if ri != rj {
			return ri < rj
		}
		return decisions[i].Confidence > decisions[j].Confidence
	})
}

func formatHoldDuration(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	if d < 0 {
		d = 0
	}
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	if hours > 0 {
		return fmt.Sprintf("%dh%dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}

// safeFloat extracts a float64 from a broker map, tolerating missing keys.
func safeFloat(data map[string]interface{}, key string) float64 {
	if data == nil {
		return 0
	}
	switch v := data[key].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case string:
		var f float64
		_, err := fmt.Sscanf(v, "%g", &f)
		if err != nil {
			return 0
		}
		return f
	default:
		return 0
	}
}
