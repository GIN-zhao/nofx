package trader

import (
	"fmt"
	"nofx/logger"
	"nofx/notify"
	"strings"
	"time"
)

// HandleTelegramMessage responds to an incoming Telegram message. Only the
// configured chat is served; commands reply with account data and any free
// text is forwarded to the AI with the current account context.
func (at *AutoTrader) HandleTelegramMessage(chatID int64, from, text string) {
	tg, ok := at.notifier.(*notify.Telegram)
	if !ok || !tg.AllowsChat(chatID) {
		return // ignore strangers
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	// Strip an @BotName suffix from commands: "/status@My_bot" -> "/status"
	if strings.HasPrefix(text, "/") {
		if i := strings.IndexAny(text, "@ "); i > 0 {
			text = text[:i]
		}
	}

	var reply string
	switch strings.ToLower(text) {
	case "/help", "/start":
		reply = telegramHelpText()
	case "/status":
		reply = at.telegramStatus()
	case "/positions":
		reply = at.telegramPositions()
	case "/trades":
		reply = at.telegramTrades()
	case "/pause":
		at.SetOpensPaused(true)
		reply = "⏸ 已暂停开仓（已有持仓继续正常管理，发送 /resume 恢复）"
	case "/resume":
		at.SetOpensPaused(false)
		reply = "▶️ 已恢复开仓"
	default:
		if strings.HasPrefix(text, "/") {
			reply = fmt.Sprintf("未知指令 %s\n\n%s", text, telegramHelpText())
		} else {
			reply = at.telegramAIChat(from, text)
		}
	}

	if err := tg.SendPlain(reply); err != nil {
		logger.Warnf("⚠️ [Trader %s] telegram reply failed: %v", at.id, err)
	}
}

func telegramHelpText() string {
	return "🤖 NOFX 模拟交易指令\n\n" +
		"/status — 账户总览（权益/盈亏/胜率）\n" +
		"/positions — 当前持仓明细\n" +
		"/trades — 最近平仓记录\n" +
		"/pause — 暂停开新仓\n" +
		"/resume — 恢复开仓\n\n" +
		"直接输入任意问题，AI 可以调用工具：查实时行情（任意币种/周期）、账户持仓，" +
		"修改交易策略（例如「把最低信心度改成70」「加一个 DOGE 候选」），以及执行交易——" +
		"「平掉 BTC」「把 ETH 止损上移到 2300」都会真实执行并推送记录。"
}

// telegramStatus renders the account overview.
func (at *AutoTrader) telegramStatus() string {
	balance, err := at.trader.GetBalance()
	if err != nil {
		return fmt.Sprintf("查询余额失败: %v", err)
	}
	equity := safeFloat(balance, "totalEquity")
	available := safeFloat(balance, "availableBalance")
	unrealized := safeFloat(balance, "totalUnrealizedProfit")

	positions, _ := at.trader.GetPositions()
	initial := at.initialBalance()

	var sb strings.Builder
	sb.WriteString("📊 账户总览\n\n")
	sb.WriteString(fmt.Sprintf("初始本金: %.2f USDT\n", initial))
	sb.WriteString(fmt.Sprintf("当前权益: %.2f USDT\n", equity))
	sb.WriteString(fmt.Sprintf("可用余额: %.2f USDT\n", available))
	sb.WriteString(fmt.Sprintf("浮动盈亏: %.2f USDT\n", unrealized))
	totalPnL := equity - initial
	if initial > 0 {
		sb.WriteString(fmt.Sprintf("总盈亏: %.2f USDT (%.2f%%)\n", totalPnL, totalPnL/initial*100))
	}
	sb.WriteString(fmt.Sprintf("持仓数量: %d\n", len(positions)))

	if stats := at.computeTradingStats(); stats != nil {
		sb.WriteString(fmt.Sprintf("\n历史交易: %d 笔 | 胜率 %.1f%%\n", stats.TotalTrades, stats.WinRate))
		if stats.ProfitFactor > 0 {
			sb.WriteString(fmt.Sprintf("盈亏比: %.2f | 最大回撤 %.1f%%\n", stats.ProfitFactor, stats.MaxDrawdownPct))
		}
	}

	if at.OpensPaused() {
		sb.WriteString("\n⏸ 开仓已暂停 (/resume 恢复)")
	}
	if safe, reason := at.safeModeState(); safe {
		sb.WriteString(fmt.Sprintf("\n🛑 安全模式: %s", reason))
	}
	return sb.String()
}

// telegramPositions renders open position details.
func (at *AutoTrader) telegramPositions() string {
	positions, err := at.trader.GetPositions()
	if err != nil {
		return fmt.Sprintf("查询持仓失败: %v", err)
	}
	if len(positions) == 0 {
		return "📭 当前无持仓"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📌 持仓明细（%d 个）\n\n", len(positions)))
	for _, pos := range positions {
		symbol, _ := pos["symbol"].(string)
		side, _ := pos["side"].(string)
		entry := safeFloat(pos, "entryPrice")
		mark := safeFloat(pos, "markPrice")
		qty := safeFloat(pos, "positionAmt")
		pnl := safeFloat(pos, "unRealizedProfit")
		leverage := int(safeFloat(pos, "leverage"))
		liq := safeFloat(pos, "liquidationPrice")
		stopLoss := safeFloat(pos, "stopLoss")
		takeProfit := safeFloat(pos, "takeProfit")
		margin := entry * abs(qty) / float64(leverage)
		pnlPct := 0.0
		if margin > 0 {
			pnlPct = pnl / margin * 100
		}
		sb.WriteString(fmt.Sprintf("%s %s %dx\n", strings.ToUpper(symbol), strings.ToUpper(side), leverage))
		sb.WriteString(fmt.Sprintf("开仓 %.4g → 现价 %.4g\n", entry, mark))
		sb.WriteString(fmt.Sprintf("本金 %.2f | 浮动 %.2f (%.1f%%)\n", margin, pnl, pnlPct))
		if stopLoss > 0 {
			sb.WriteString(fmt.Sprintf("止损 %.4g ", stopLoss))
		}
		if takeProfit > 0 {
			sb.WriteString(fmt.Sprintf("止盈 %.4g", takeProfit))
		}
		if stopLoss > 0 || takeProfit > 0 {
			sb.WriteString("\n")
		}
		if liq > 0 {
			sb.WriteString(fmt.Sprintf("爆仓价 %.4g\n", liq))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// telegramTrades renders recent closed positions.
func (at *AutoTrader) telegramTrades() string {
	closed, err := at.store.Position().GetClosedPositions(at.id, 10)
	if err != nil {
		return fmt.Sprintf("查询历史失败: %v", err)
	}
	if len(closed) == 0 {
		return "📭 暂无平仓记录"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📜 最近平仓（%d 笔）\n\n", len(closed)))
	for _, pos := range closed {
		icon := "✅"
		if pos.RealizedPnL < 0 {
			icon = "❌"
		}
		sb.WriteString(fmt.Sprintf("%s %s %s %dx\n", icon, pos.Symbol, strings.ToUpper(pos.Side), pos.Leverage))
		sb.WriteString(fmt.Sprintf("%.4g → %.4g | 盈亏 %.2f USDT\n", pos.EntryPrice, pos.ExitPrice, pos.RealizedPnL))
		sb.WriteString(fmt.Sprintf("%s ~ %s (%s)\n\n",
			time.UnixMilli(pos.EntryTime).Format("01-02 15:04"),
			time.UnixMilli(pos.ExitTime).Format("01-02 15:04"),
			formatHoldDuration(pos.ExitTime-pos.EntryTime)))
	}
	return sb.String()
}

// telegramAIChat forwards a free-form question to the AI agent (with tool
// calling: live market data, account state, strategy modification).
func (at *AutoTrader) telegramAIChat(from, question string) string {
	return at.telegramAgentChat(question)
}

// telegramChatContext builds the system prompt describing the current state.
func (at *AutoTrader) telegramChatContext() string {
	var sb strings.Builder
	sb.WriteString("你是一个模拟盘合约交易助手（OKX 行情 + AI 决策），用简洁的中文回答用户关于交易状态、市场和策略的问题。")
	sb.WriteString("模拟盘不涉及真实资金。以下是最新的账户状态：\n\n")

	balance, err := at.trader.GetBalance()
	if err == nil {
		sb.WriteString(fmt.Sprintf("权益 %.2f USDT，可用 %.2f，浮动盈亏 %.2f，初始本金 %.2f\n",
			safeFloat(balance, "totalEquity"), safeFloat(balance, "availableBalance"),
			safeFloat(balance, "totalUnrealizedProfit"), at.initialBalance()))
	}

	if positions, err := at.trader.GetPositions(); err == nil && len(positions) > 0 {
		sb.WriteString("当前持仓:\n")
		for _, pos := range positions {
			symbol, _ := pos["symbol"].(string)
			side, _ := pos["side"].(string)
			sb.WriteString(fmt.Sprintf("- %s %s %dx, 开仓价 %.4g, 现价 %.4g, 浮动 %.2f USDT\n",
				symbol, side, int(safeFloat(pos, "leverage")),
				safeFloat(pos, "entryPrice"), safeFloat(pos, "markPrice"), safeFloat(pos, "unRealizedProfit")))
		}
	} else {
		sb.WriteString("当前无持仓。\n")
	}

	if stats := at.computeTradingStats(); stats != nil {
		sb.WriteString(fmt.Sprintf("历史: %d 笔, 胜率 %.0f%%, 总盈亏 %.2f USDT\n",
			stats.TotalTrades, stats.WinRate, stats.TotalPnL))
	}

	if orders := at.computeRecentOrders(); len(orders) > 0 {
		sb.WriteString("最近平仓:\n")
		for i, o := range orders {
			if i >= 5 {
				break
			}
			sb.WriteString(fmt.Sprintf("- %s %s 盈亏 %.2f USDT (%s)\n", o.Symbol, o.Side, o.RealizedPnL, o.HoldDuration))
		}
	}

	sb.WriteString("\n候选币种: ")
	for _, coin := range at.staticCoins() {
		sb.WriteString(coin + " ")
	}
	sb.WriteString("\n回答控制在 800 字以内，不用 Markdown 之外的格式。")
	return sb.String()
}

// truncateTelegram keeps replies inside the Telegram message limit.
func truncateTelegram(s string) string {
	const maxLen = 3900
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "\n\n…(内容过长已截断)"
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
