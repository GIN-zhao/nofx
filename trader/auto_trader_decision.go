package trader

import (
	"encoding/json"
	"fmt"
	"math"
	"nofx/kernel"
	"nofx/logger"
	"nofx/market"
	"nofx/store"
	"strings"
	"time"
)

// ============================================================================
// Decision execution
// ============================================================================

const (
	// marginOverheadFactor and takerFeeRate approximate the total funds an
	// exchange reserves when opening a position:
	// totalRequired ≈ positionSize/leverage + positionSize*takerFeeRate + margin*1%
	marginOverheadFactor = 1.01
	takerFeeRate         = 0.001

	// positionSizeSafetyFactor leaves a buffer below the maximum affordable
	// position size so a price move between sizing and execution cannot
	// trigger an insufficient-margin rejection.
	positionSizeSafetyFactor = 0.98
)

// executeDecisionWithRecord executes an AI decision against the broker.
func (at *AutoTrader) executeDecisionWithRecord(decision *kernel.Decision) error {
	switch decision.Action {
	case "open_long":
		return at.executeOpen(decision, "long")
	case "open_short":
		return at.executeOpen(decision, "short")
	case "close_long":
		return at.executeClose(decision, "long")
	case "close_short":
		return at.executeClose(decision, "short")
	default:
		return fmt.Errorf("unknown action: %s", decision.Action)
	}
}

func (at *AutoTrader) executeOpen(decision *kernel.Decision, side string) error {
	logger.Infof("  📈 Open %s: %s", side, decision.Symbol)

	if at.OpensPaused() {
		return fmt.Errorf("opening new positions is paused (/resume to re-enable)")
	}

	positions, err := at.trader.GetPositions()
	if err != nil {
		return fmt.Errorf("failed to get positions: %w", err)
	}
	if err := at.enforceMaxPositions(len(positions)); err != nil {
		return err
	}
	for _, pos := range positions {
		if pos["symbol"] == decision.Symbol && pos["side"] == side {
			return fmt.Errorf("%s already has an open %s position, close it first", decision.Symbol, side)
		}
	}

	price, err := at.trader.GetMarketPrice(decision.Symbol)
	if err != nil {
		return fmt.Errorf("failed to get market price for %s: %w", decision.Symbol, err)
	}
	if err := validateProtectionPrices(decision.Action, price, decision.StopLoss, decision.TakeProfit); err != nil {
		return err
	}

	balance, err := at.trader.GetBalance()
	if err != nil {
		return fmt.Errorf("failed to get account balance: %w", err)
	}
	equity := safeFloat(balance, "totalEquity")
	available := safeFloat(balance, "availableBalance")

	// [CODE ENFORCED] Position value ratio + affordability caps.
	positionSize := decision.PositionSizeUSD
	if capped, wasCapped := at.enforcePositionValueRatio(positionSize, equity, decision.Symbol); wasCapped {
		positionSize = capped
	}
	marginFactor := marginOverheadFactor/float64(decision.Leverage) + takerFeeRate
	if maxAffordable := available / marginFactor * positionSizeSafetyFactor; positionSize > maxAffordable {
		logger.Infof("  ⚠️ Position size %.2f exceeds max affordable %.2f, auto-reducing", positionSize, maxAffordable)
		positionSize = maxAffordable
	}
	if err := at.enforceMinPositionSize(positionSize); err != nil {
		return err
	}
	decision.PositionSizeUSD = positionSize

	quantity := positionSize / price
	margin := positionSize / float64(decision.Leverage)

	var order map[string]interface{}
	if side == "long" {
		order, err = at.trader.OpenLong(decision.Symbol, quantity, decision.Leverage)
	} else {
		order, err = at.trader.OpenShort(decision.Symbol, quantity, decision.Leverage)
	}
	if err != nil {
		return fmt.Errorf("failed to open %s position for %s: %w", side, decision.Symbol, err)
	}

	fillPrice := safeFloat(order, "price")
	if fillPrice <= 0 {
		fillPrice = price
	}

	// Protective SL/TP are mandatory — a position without a stop is closed immediately.
	if side == "long" {
		err = at.trader.SetStopLoss(decision.Symbol, "LONG", quantity, decision.StopLoss)
	} else {
		err = at.trader.SetStopLoss(decision.Symbol, "SHORT", quantity, decision.StopLoss)
	}
	if err != nil {
		return at.closeUnprotectedPosition(decision.Symbol, side, fmt.Errorf("failed to set stop loss: %w", err))
	}
	if side == "long" {
		err = at.trader.SetTakeProfit(decision.Symbol, "LONG", quantity, decision.TakeProfit)
	} else {
		err = at.trader.SetTakeProfit(decision.Symbol, "SHORT", quantity, decision.TakeProfit)
	}
	if err != nil {
		return at.closeUnprotectedPosition(decision.Symbol, side, fmt.Errorf("failed to set take profit: %w", err))
	}

	logger.Infof("  ✓ Position opened: %s %s %.8f @ %.2f (%.2f USDT margin, %dx)",
		decision.Symbol, side, quantity, fillPrice, margin, decision.Leverage)
	at.notify(formatOpenMessage(decision, side, fillPrice, margin, quantity))
	return nil
}

func (at *AutoTrader) executeClose(decision *kernel.Decision, side string) error {
	logger.Infof("  🔄 Close %s: %s", side, decision.Symbol)

	var order map[string]interface{}
	var err error
	if side == "long" {
		order, err = at.trader.CloseLong(decision.Symbol, 0)
	} else {
		order, err = at.trader.CloseShort(decision.Symbol, 0)
	}
	if err != nil {
		return fmt.Errorf("failed to close %s position for %s: %w", side, decision.Symbol, err)
	}

	exitPrice := safeFloat(order, "price")
	pnl := safeFloat(order, "realizedPnl")

	// Pull leverage from the just-closed ledger row for the message.
	leverage := 1
	if closed, cerr := at.store.Position().GetClosedPositions(at.id, 1); cerr == nil && len(closed) > 0 &&
		strings.EqualFold(closed[0].Symbol, decision.Symbol) {
		leverage = closed[0].Leverage
	}

	logger.Infof("  ✓ Position closed: %s %s @ %.2f (PnL %.2f)", decision.Symbol, side, exitPrice, pnl)
	at.notify(formatCloseMessage(decision.Symbol, side, exitPrice, pnl, leverage, "ai", decision.Reasoning))
	return nil
}

// closeUnprotectedPosition closes a position whose protection failed to arm.
func (at *AutoTrader) closeUnprotectedPosition(symbol, side string, cause error) error {
	logger.Errorf("  🚨 Closing unprotected %s position on %s: %v", side, symbol, cause)
	var err error
	if side == "long" {
		_, err = at.trader.CloseLong(symbol, 0)
	} else {
		_, err = at.trader.CloseShort(symbol, 0)
	}
	if err != nil {
		return fmt.Errorf("%w (and close also failed: %v)", cause, err)
	}
	return cause
}

// validateProtectionPrices rejects SL/TP that are on the wrong side of the
// current market price (the entry would immediately trigger them).
func validateProtectionPrices(action string, currentPrice, stopLoss, takeProfit float64) error {
	switch action {
	case "open_long":
		if stopLoss >= currentPrice {
			return fmt.Errorf("stop loss %.2f must be below current price %.2f for a long entry", stopLoss, currentPrice)
		}
		if takeProfit <= currentPrice {
			return fmt.Errorf("take profit %.2f must be above current price %.2f for a long entry", takeProfit, currentPrice)
		}
	case "open_short":
		if stopLoss <= currentPrice {
			return fmt.Errorf("stop loss %.2f must be above current price %.2f for a short entry", stopLoss, currentPrice)
		}
		if takeProfit >= currentPrice {
			return fmt.Errorf("take profit %.2f must be below current price %.2f for a short entry", takeProfit, currentPrice)
		}
	}
	return nil
}

// ============================================================================
// Persistence
// ============================================================================

// saveEquitySnapshot persists an equity curve point.
func (at *AutoTrader) saveEquitySnapshot() {
	balance, err := at.trader.GetBalance()
	if err != nil {
		return
	}
	equity := safeFloat(balance, "totalEquity")
	unrealized := safeFloat(balance, "totalUnrealizedProfit")
	available := safeFloat(balance, "availableBalance")

	positions, err := at.trader.GetPositions()
	if err != nil {
		return
	}
	marginUsed := 0.0
	for _, pos := range positions {
		leverage := safeFloat(pos, "leverage")
		if leverage < 1 {
			leverage = 1
		}
		marginUsed += safeFloat(pos, "entryPrice") * math.Abs(safeFloat(pos, "positionAmt")) / leverage
	}
	marginUsedPct := 0.0
	if equity > 0 {
		marginUsedPct = marginUsed / equity * 100
	}

	if err := at.store.Equity().Save(&store.EquitySnapshot{
		TraderID:         at.id,
		Timestamp:        time.Now().UTC(),
		TotalEquity:      equity,
		Balance:          equity - unrealized,
		AvailableBalance: available,
		UnrealizedPnL:    unrealized,
		PositionCount:    len(positions),
		MarginUsedPct:    marginUsedPct,
	}); err != nil {
		logger.Warnf("⚠️ [Trader %s] Failed to save equity snapshot: %v", at.id, err)
	}
}

// saveDecision persists the raw AI decision for audit/debug.
func (at *AutoTrader) saveDecision(cycleNumber int, full *kernel.FullDecision, ctx *kernel.Context) {
	if full == nil {
		return
	}
	decisionJSON, _ := json.Marshal(full.Decisions)
	coins := make([]string, 0, len(ctx.CandidateCoins))
	for _, coin := range ctx.CandidateCoins {
		coins = append(coins, coin.Symbol)
	}

	record := &store.DecisionRecord{
		TraderID:            at.id,
		CycleNumber:         cycleNumber,
		Timestamp:           full.Timestamp,
		SystemPrompt:        full.SystemPrompt,
		InputPrompt:         full.UserPrompt,
		CoTTrace:            full.CoTTrace,
		DecisionJSON:        string(decisionJSON),
		RawResponse:         full.RawResponse,
		CandidateCoins:      coins,
		ExecutionLog:        []string{},
		Success:             true,
		AIRequestDurationMs: full.AIRequestDurationMs,
		AccountState: store.AccountSnapshot{
			TotalBalance:          ctx.Account.TotalEquity,
			AvailableBalance:      ctx.Account.AvailableBalance,
			TotalUnrealizedProfit: ctx.Account.UnrealizedPnL,
			PositionCount:         ctx.Account.PositionCount,
			MarginUsedPct:         ctx.Account.MarginUsedPct,
		},
	}
	for _, pos := range ctx.Positions {
		record.Positions = append(record.Positions, store.PositionSnapshot{
			Symbol:           pos.Symbol,
			Side:             pos.Side,
			PositionAmt:      pos.Quantity,
			EntryPrice:       pos.EntryPrice,
			MarkPrice:        pos.MarkPrice,
			UnrealizedProfit: pos.UnrealizedPnL,
			Leverage:         float64(pos.Leverage),
			LiquidationPrice: pos.LiquidationPrice,
		})
	}
	if err := at.store.Decision().LogDecision(record); err != nil {
		logger.Warnf("⚠️ [Trader %s] Failed to save decision record: %v", at.id, err)
	}
}

// saveFailedDecision persists a failed AI call for debugging.
func (at *AutoTrader) saveFailedDecision(ctx *kernel.Context, cycleErr error) {
	record := &store.DecisionRecord{
		TraderID:     at.id,
		CycleNumber:  at.callCount,
		Timestamp:    time.Now().UTC(),
		CandidateCoins: []string{},
		ExecutionLog:   []string{},
		Success:        false,
		ErrorMessage:   cycleErr.Error(),
	}
	if ctx != nil {
		for _, coin := range ctx.CandidateCoins {
			record.CandidateCoins = append(record.CandidateCoins, coin.Symbol)
		}
	}
	if err := at.store.Decision().LogDecision(record); err != nil {
		logger.Warnf("⚠️ [Trader %s] Failed to save failed-decision record: %v", at.id, err)
	}
}

// ============================================================================
// Telegram messages
// ============================================================================

// formatOpenMessage builds the Telegram message for a position open.
func formatOpenMessage(d *kernel.Decision, side string, fillPrice, margin, quantity float64) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s *开仓 %s*\n\n", directionEmoji(d.Action), d.Symbol))
	sb.WriteString(fmt.Sprintf("方向: *%s*\n", strings.ToUpper(side)))
	sb.WriteString(fmt.Sprintf("策略: AI 多周期趋势决策\n"))
	trend1h, trend4h := marketTrend(d.Symbol)
	sb.WriteString(fmt.Sprintf("现价: %.2f (1h %.2f%% / 4h %.2f%%)\n", fillPrice, trend1h, trend4h))
	sb.WriteString(fmt.Sprintf("本金: %.2f USDT\n", margin))
	sb.WriteString(fmt.Sprintf("杠杆: %dx\n", d.Leverage))
	sb.WriteString(fmt.Sprintf("仓位价值: %.2f USDT (%.4f)\n", margin*float64(d.Leverage), quantity))
	sb.WriteString(fmt.Sprintf("止损: %.2f\n", d.StopLoss))
	sb.WriteString(fmt.Sprintf("止盈: %.2f\n", d.TakeProfit))
	sb.WriteString(fmt.Sprintf("信心: %d%%\n", d.Confidence))
	if d.Reasoning != "" {
		sb.WriteString(fmt.Sprintf("\n🧠 AI 分析:\n%s\n", truncate(d.Reasoning, 700)))
	}
	return sb.String()
}

// formatCloseMessage builds the Telegram message for a position close.
func formatCloseMessage(symbol, side string, exitPrice, pnl float64, leverage int, closeType, reasoning string) string {
	var sb strings.Builder
	icon := "✅"
	if pnl < 0 {
		icon = "❌"
	}
	sb.WriteString(fmt.Sprintf("%s *平仓 %s*\n\n", icon, symbol))
	sb.WriteString(fmt.Sprintf("方向: *%s*\n", strings.ToUpper(side)))
	sb.WriteString(fmt.Sprintf("退出价: %.2f\n", exitPrice))
	sb.WriteString(fmt.Sprintf("盈亏: *%.2f USDT* (%.2fx杠杆)\n", pnl, float64(leverage)))
	switch closeType {
	case "stop_loss":
		sb.WriteString("原因: 触发止损 🛑\n")
	case "take_profit":
		sb.WriteString("原因: 触发止盈 🎯\n")
	case "liquidation":
		sb.WriteString("原因: 爆仓 💥\n")
	}
	if reasoning != "" {
		sb.WriteString(fmt.Sprintf("\n🧠 AI 分析:\n%s\n", truncate(reasoning, 700)))
	}
	return sb.String()
}

// marketTrend returns the 1h and 4h price change percentages for a symbol,
// used to describe the current market direction in notifications.
func marketTrend(symbol string) (float64, float64) {
	data, err := market.Get(symbol)
	if err != nil || data == nil {
		return 0, 0
	}
	return data.PriceChange1h, data.PriceChange4h
}

func directionEmoji(action string) string {
	switch action {
	case "open_long":
		return "📈"
	case "open_short":
		return "📉"
	default:
		return "🔄"
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
