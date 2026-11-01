package trader

import (
	"encoding/json"
	"fmt"
	"nofx/logger"
	"nofx/market"
	"nofx/mcp"
	"nofx/store"
	"strings"
)

// ============================================================================
// Telegram agentic chat: the AI can call nofx tools on demand to fetch live
// market data, account state, and to modify the trading strategy.
// ============================================================================

const maxToolRounds = 5 // hard cap on tool-calling round trips per question

// telegramTools returns the tool definitions offered to the chat AI.
func telegramTools() []mcp.Tool {
	objectSchema := map[string]any{"type": "object", "properties": map[string]any{}}
	return []mcp.Tool{
		{Type: "function", Function: mcp.FunctionDef{
			Name: "get_market_data",
			Description: "获取币种的 OKX 实时行情：现价、1h/4h 涨跌、持仓量、资金费率，以及各周期的 EMA/MACD/RSI/BOLL/ATR 指标。回答行情相关问题时必须先调用本工具。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"symbol": map[string]any{"type": "string", "description": "币种，如 BTC、ETH、SOL"},
					"timeframes": map[string]any{
						"type": "array", "items": map[string]any{"type": "string"},
						"description": "可选，K线周期列表，默认 [15m, 1h, 4h]，可选 1m/3m/5m/15m/30m/1h/2h/4h/6h/8h/12h/1d/3d/1w",
					},
				},
				"required": []string{"symbol"},
			},
		}},
		{Type: "function", Function: mcp.FunctionDef{
			Name:        "get_latest_price",
			Description: "获取币种当前最新价格（轻量，只要价格时用这个）。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"symbol": map[string]any{"type": "string", "description": "币种，如 BTC"},
				},
				"required": []string{"symbol"},
			},
		}},
		{Type: "function", Function: mcp.FunctionDef{
			Name:        "get_positions",
			Description: "获取当前模拟账户的所有持仓明细（方向、杠杆、开仓价、现价、浮动盈亏、止损止盈）。",
			Parameters:  objectSchema,
		}},
		{Type: "function", Function: mcp.FunctionDef{
			Name:        "get_account_status",
			Description: "获取账户总览：权益、可用余额、浮动盈亏、历史胜率、盈亏比、最大回撤。",
			Parameters:  objectSchema,
		}},
		{Type: "function", Function: mcp.FunctionDef{
			Name:        "get_recent_trades",
			Description: "获取最近的平仓交易记录。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"limit": map[string]any{"type": "integer", "description": "条数，默认 10，最大 30"},
				},
			},
		}},
		{Type: "function", Function: mcp.FunctionDef{
			Name:        "get_strategy",
			Description: "获取当前生效的交易策略配置（完整 JSON）。",
			Parameters:  objectSchema,
		}},
		{Type: "function", Function: mcp.FunctionDef{
			Name: "close_position",
			Description: "立即以市价平掉指定币种的全部持仓（绕过最小持仓时间限制）。用户明确要求平仓/提前止损/认错离场时使用。执行成功后会自动推送平仓记录。" +
				"这是真实执行操作，仅在用户明确表达平仓意图时调用，不要自作主张。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"symbol": map[string]any{"type": "string", "description": "持仓币种，如 BTCUSDT（以 get_positions 返回的 symbol 为准）"},
					"side":   map[string]any{"type": "string", "enum": []string{"long", "short"}, "description": "可选，方向；不填默认该币种的持仓"},
					"reason": map[string]any{"type": "string", "description": "可选，平仓原因（记入推送消息）"},
				},
				"required": []string{"symbol"},
			},
		}},
		{Type: "function", Function: mcp.FunctionDef{
			Name: "adjust_protection",
			Description: "修改指定持仓的止损/止盈价格（覆盖旧值）。用户要求收紧止损、上移止损、保本止损、调整止盈时使用。" +
				"多头止损必须低于现价、止盈高于现价；空头相反。至少提供 stop_loss 或 take_profit 之一。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"symbol":     map[string]any{"type": "string", "description": "持仓币种，如 BTCUSDT"},
					"side":       map[string]any{"type": "string", "enum": []string{"long", "short"}, "description": "可选，方向；不填默认该币种的持仓"},
					"stop_loss":  map[string]any{"type": "number", "description": "新止损价（0 表示不修改）"},
					"take_profit": map[string]any{"type": "number", "description": "新止盈价（0 表示不修改）"},
				},
				"required": []string{"symbol"},
			},
		}},
		{Type: "function", Function: mcp.FunctionDef{
			Name: "update_strategy",
			Description: "修改交易策略配置（立即持久化并热更新到下一个决策周期）。patch 是部分 JSON 对象，与 get_strategy 返回的结构相同，深度合并、未提到的字段保持不变。" +
				"常用路径: ai_config.coin_source.static_coins (候选币种数组), ai_config.indicators.klines.selected_timeframes (周期数组), " +
				"ai_config.indicators.klines.primary_timeframe, ai_config.risk_control.max_positions / min_confidence / min_position_size / " +
				"btceth_max_leverage / altcoin_max_leverage / max_margin_usage / min_risk_reward_ratio / custom_prompt。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"patch": map[string]any{
						"type":       "object",
						"description": `部分配置，例如 {"ai_config":{"risk_control":{"min_confidence":70}}} 把最低信心度改为 70`,
					},
				},
				"required": []string{"patch"},
			},
		}},
	}
}

// telegramAgentChat runs an agentic tool-calling conversation and returns the
// final answer text.
func (at *AutoTrader) telegramAgentChat(question string) string {
	messages := []mcp.Message{
		mcp.NewSystemMessage(at.telegramAgentSystemPrompt()),
		mcp.NewUserMessage(question),
	}

	for round := 0; round < maxToolRounds; round++ {
		req := &mcp.Request{Messages: messages, Tools: telegramTools(), ToolChoice: "auto"}
		if round == maxToolRounds-1 {
			// Last round: drop tools to force a textual final answer.
			req.Tools = nil
			req.ToolChoice = ""
		}

		resp, err := at.aiClient.CallWithRequestFull(req)
		if err != nil {
			logger.Warnf("⚠️ [Trader %s] telegram agent chat failed: %v", at.id, err)
			return fmt.Sprintf("AI 暂时无法回答: %v", err)
		}

		if len(resp.ToolCalls) == 0 {
			answer := strings.TrimSpace(resp.Content)
			if answer == "" {
				return "AI 没有返回内容，请稍后再试"
			}
			return truncateTelegram(answer)
		}

		// Echo the assistant tool-call message back, then append tool results.
		messages = append(messages, mcp.Message{
			Role:             "assistant",
			Content:          resp.Content,
			ReasoningContent: resp.ReasoningContent,
			ToolCalls:        resp.ToolCalls,
		})
		for _, tc := range resp.ToolCalls {
			result := at.executeTelegramTool(tc.Function.Name, tc.Function.Arguments)
			logger.Infof("🛠 [Trader %s] telegram tool %s(%s) -> %d chars", at.id, tc.Function.Name, tc.Function.Arguments, len(result))
			messages = append(messages, mcp.Message{Role: "tool", ToolCallID: tc.ID, Content: result})
		}
	}

	return "AI 工具调用次数已达上限，请缩小问题范围后重试。"
}

// executeTelegramTool dispatches a single tool call and always returns a
// string result (errors become "ERROR: ..." so the model can react).
func (at *AutoTrader) executeTelegramTool(name, argsJSON string) string {
	var args map[string]any
	if strings.TrimSpace(argsJSON) != "" {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "ERROR: 参数不是合法 JSON: " + err.Error()
		}
	}

	str := func(key string) string {
		v, _ := args[key].(string)
		return strings.TrimSpace(v)
	}

	switch name {
	case "get_market_data":
		symbol := str("symbol")
		if symbol == "" {
			return "ERROR: symbol 不能为空"
		}
		timeframes := []string{"15m", "1h", "4h"}
		if raw, ok := args["timeframes"].([]any); ok && len(raw) > 0 {
			timeframes = timeframes[:0]
			for _, v := range raw {
				if s, ok := v.(string); ok && s != "" {
					timeframes = append(timeframes, s)
				}
			}
		}
		data, err := market.GetWithTimeframes(symbol, timeframes, timeframes[0], 30)
		if err != nil {
			return "ERROR: 获取行情失败: " + err.Error()
		}
		return formatMarketForChat(data)

	case "get_latest_price":
		symbol := str("symbol")
		if symbol == "" {
			return "ERROR: symbol 不能为空"
		}
		price, err := market.GetLatestPrice(symbol)
		if err != nil {
			return "ERROR: 获取价格失败: " + err.Error()
		}
		return fmt.Sprintf("%s 最新价: %.4g USDT", market.Normalize(symbol), price)

	case "get_positions":
		return at.telegramPositions()

	case "get_account_status":
		return at.telegramStatus()

	case "get_recent_trades":
		limit := 10
		if v, ok := args["limit"].(float64); ok && v >= 1 {
			limit = int(v)
		}
		if limit > 30 {
			limit = 30
		}
		closed, err := at.store.Position().GetClosedPositions(at.id, limit)
		if err != nil {
			return "ERROR: 查询历史失败: " + err.Error()
		}
		if len(closed) == 0 {
			return "暂无平仓记录"
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "最近 %d 笔平仓:\n", len(closed))
		for _, pos := range closed {
			fmt.Fprintf(&sb, "- %s %s %dx %.4g→%.4g 盈亏 %.2f USDT\n",
				pos.Symbol, strings.ToUpper(pos.Side), pos.Leverage, pos.EntryPrice, pos.ExitPrice, pos.RealizedPnL)
		}
		return sb.String()

	case "get_strategy":
		cfgJSON, err := json.Marshal(at.currentEngine().GetConfig())
		if err != nil {
			return "ERROR: 序列化策略失败: " + err.Error()
		}
		return "当前策略配置:\n" + string(cfgJSON)

	case "update_strategy":
		patch, ok := args["patch"].(map[string]any)
		if !ok {
			return "ERROR: 缺少 patch 参数（必须是 JSON 对象）"
		}
		summary, err := at.toolUpdateStrategy(patch)
		if err != nil {
			logger.Warnf("⚠️ [Trader %s] telegram update_strategy failed: %v", at.id, err)
			return "ERROR: " + err.Error()
		}
		return summary

	case "close_position":
		symbol := str("symbol")
		if symbol == "" {
			return "ERROR: symbol 不能为空"
		}
		reason, _ := args["reason"].(string)
		return at.toolClosePosition(symbol, strings.ToLower(str("side")), reason)

	case "adjust_protection":
		symbol := str("symbol")
		if symbol == "" {
			return "ERROR: symbol 不能为空"
		}
		var sl, tp float64
		if v, ok := args["stop_loss"].(float64); ok {
			sl = v
		}
		if v, ok := args["take_profit"].(float64); ok {
			tp = v
		}
		return at.toolAdjustProtection(symbol, strings.ToLower(str("side")), sl, tp)

	default:
		return "ERROR: 未知工具 " + name
	}
}

// ============================================================================
// Trade execution tools (invoked only from the configured Telegram chat)
// ============================================================================

// findOpenPosition locates an open position by symbol (and optional side).
// Accepts the ledger symbol form ("BTCUSDT") and the bare base coin ("BTC").
func (at *AutoTrader) findOpenPosition(symbol, side string) (map[string]interface{}, string, error) {
	positions, err := at.trader.GetPositions()
	if err != nil {
		return nil, "", fmt.Errorf("查询持仓失败: %w", err)
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	for _, pos := range positions {
		posSymbol, _ := pos["symbol"].(string)
		posSide, _ := pos["side"].(string)
		posSide = strings.ToUpper(posSide)
		if posSymbol != symbol && posSymbol != symbol+"USDT" {
			continue
		}
		if side != "" && posSide != strings.ToUpper(side) {
			continue
		}
		return pos, posSymbol, nil
	}
	return nil, "", fmt.Errorf("没有找到 %s 的持仓", symbol)
}

// toolClosePosition market-closes the full position and pushes the standard
// close notification. Bypasses the AI-cycle throttle by design: chat closes
// exist so the user can cut a losing trade early.
func (at *AutoTrader) toolClosePosition(symbol, side, reason string) string {
	pos, posSymbol, err := at.findOpenPosition(symbol, side)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	posSide, _ := pos["side"].(string)
	leverage := int(safeFloat(pos, "leverage"))
	if leverage < 1 {
		leverage = 1
	}

	var order map[string]interface{}
	if strings.EqualFold(posSide, "LONG") {
		order, err = at.trader.CloseLong(posSymbol, 0)
	} else {
		order, err = at.trader.CloseShort(posSymbol, 0)
	}
	if err != nil {
		logger.Warnf("⚠️ [Trader %s] telegram close_position %s failed: %v", at.id, posSymbol, err)
		return "ERROR: 平仓失败: " + err.Error()
	}

	exitPrice := safeFloat(order, "price")
	pnl := safeFloat(order, "realizedPnl")
	logger.Infof("  ✓ Telegram manual close: %s %s @ %.2f (PnL %.2f)", posSymbol, posSide, exitPrice, pnl)

	closeReason := "Telegram 手动平仓"
	if reason != "" {
		closeReason += ": " + reason
	}
	at.notify(formatCloseMessage(posSymbol, posSide, exitPrice, pnl, leverage, "manual", closeReason))
	return fmt.Sprintf("✅ 已市价平仓 %s %s @ %.4g，盈亏 %.2f USDT（已推送交易记录）", posSymbol, posSide, exitPrice, pnl)
}

// toolAdjustProtection overwrites SL/TP on an open position after validating
// the new prices sit on the correct side of the current market price.
func (at *AutoTrader) toolAdjustProtection(symbol, side string, stopLoss, takeProfit float64) string {
	pos, posSymbol, err := at.findOpenPosition(symbol, side)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	posSide, _ := pos["side"].(string)
	if stopLoss <= 0 && takeProfit <= 0 {
		return "ERROR: stop_loss 和 take_profit 至少提供一个（>0 表示修改）"
	}

	price, err := at.trader.GetMarketPrice(posSymbol)
	if err != nil {
		return "ERROR: 获取现价失败: " + err.Error()
	}
	isLong := strings.EqualFold(posSide, "LONG")
	if stopLoss > 0 {
		if isLong && stopLoss >= price {
			return fmt.Sprintf("ERROR: 多头止损 %.4g 必须低于现价 %.4g", stopLoss, price)
		}
		if !isLong && stopLoss <= price {
			return fmt.Sprintf("ERROR: 空头止损 %.4g 必须高于现价 %.4g", stopLoss, price)
		}
	}
	if takeProfit > 0 {
		if isLong && takeProfit <= price {
			return fmt.Sprintf("ERROR: 多头止盈 %.4g 必须高于现价 %.4g", takeProfit, price)
		}
		if !isLong && takeProfit >= price {
			return fmt.Sprintf("ERROR: 空头止盈 %.4g 必须低于现价 %.4g", takeProfit, price)
		}
	}

	if stopLoss > 0 {
		if err := at.trader.SetStopLoss(posSymbol, strings.ToUpper(posSide), 0, stopLoss); err != nil {
			return "ERROR: 设置止损失败: " + err.Error()
		}
	}
	if takeProfit > 0 {
		if err := at.trader.SetTakeProfit(posSymbol, strings.ToUpper(posSide), 0, takeProfit); err != nil {
			return "ERROR: 设置止盈失败: " + err.Error()
		}
	}

	logger.Infof("  ✓ Telegram protection update: %s %s SL %.4g TP %.4g (price %.4g)", posSymbol, posSide, stopLoss, takeProfit, price)
	var sb strings.Builder
	fmt.Fprintf(&sb, "✅ 已更新 %s %s 保护单（现价 %.4g）", posSymbol, posSide, price)
	if stopLoss > 0 {
		fmt.Fprintf(&sb, "\n新止损: %.4g", stopLoss)
	}
	if takeProfit > 0 {
		fmt.Fprintf(&sb, "\n新止盈: %.4g", takeProfit)
	}
	return sb.String()
}

// ============================================================================
// Strategy modification
// ============================================================================

// toolUpdateStrategy deep-merges the patch into the persisted strategy,
// persists it, and hot-swaps the engine. Returns a human-readable summary.
func (at *AutoTrader) toolUpdateStrategy(patch map[string]any) (string, error) {
	if len(patch) == 0 {
		return "", fmt.Errorf("patch 为空")
	}

	current, row, err := at.loadEditableStrategy()
	if err != nil {
		return "", err
	}
	before := current

	merged, err := store.MergeStrategyConfig(current, patch)
	if err != nil {
		return "", fmt.Errorf("合并配置失败: %w", err)
	}
	merged.ClampLimits()

	if row != nil {
		if err := row.SetConfig(&merged); err != nil {
			return "", err
		}
		if err := at.store.Strategy().Update(row); err != nil {
			return "", fmt.Errorf("保存策略失败: %w", err)
		}
	} else {
		newRow := &store.Strategy{ID: "default", Name: "默认策略", IsDefault: true}
		if err := newRow.SetConfig(&merged); err != nil {
			return "", err
		}
		if err := at.store.Strategy().Create(newRow); err != nil {
			return "", fmt.Errorf("保存策略失败: %w", err)
		}
	}

	at.UpdateStrategy(&merged)

	changes := strategyDiff(before, merged)
	var sb strings.Builder
	sb.WriteString("✅ 策略已更新并在下一个决策周期生效\n")
	if len(changes) == 0 {
		sb.WriteString("（配置值未发生变化）")
	} else {
		sb.WriteString("变更:\n")
		for _, c := range changes {
			sb.WriteString("- " + c + "\n")
		}
	}
	for _, w := range store.StrategyClampWarnings(before, merged, "zh") {
		sb.WriteString("⚠️ " + w + "\n")
	}
	return sb.String(), nil
}

// loadEditableStrategy loads the persisted default strategy (or the built-in
// default when none exists) together with its store row (nil if unsaved).
func (at *AutoTrader) loadEditableStrategy() (store.StrategyConfig, *store.Strategy, error) {
	if row, err := at.store.Strategy().GetDefault(); err == nil && row != nil {
		if parsed, perr := row.ParseConfig(); perr == nil && parsed != nil {
			return *parsed, row, nil
		}
	}
	return store.GetDefaultStrategyConfig("zh"), nil, nil
}

// strategyDiff renders the changed strategy fields as readable lines.
func strategyDiff(before, after store.StrategyConfig) []string {
	changes := make([]string, 0, 12)
	add := func(format string, args ...any) {
		changes = append(changes, fmt.Sprintf(format, args...))
	}

	b, a := before.CoinSource.StaticCoins, after.CoinSource.StaticCoins
	if !equalStringSlices(b, a) {
		add("候选币种: [%s] → [%s]", strings.Join(b, ","), strings.Join(a, ","))
	}

	bk, ak := before.Indicators.Klines, after.Indicators.Klines
	if bk.PrimaryTimeframe != ak.PrimaryTimeframe {
		add("主周期: %s → %s", bk.PrimaryTimeframe, ak.PrimaryTimeframe)
	}
	if bk.LongerTimeframe != ak.LongerTimeframe {
		add("长周期: %s → %s", bk.LongerTimeframe, ak.LongerTimeframe)
	}
	if !equalStringSlices(bk.SelectedTimeframes, ak.SelectedTimeframes) {
		add("分析周期: [%s] → [%s]", strings.Join(bk.SelectedTimeframes, ","), strings.Join(ak.SelectedTimeframes, ","))
	}
	if bk.PrimaryCount != ak.PrimaryCount {
		add("K线数量: %d → %d", bk.PrimaryCount, ak.PrimaryCount)
	}

	brc, arc := before.RiskControl, after.RiskControl
	if brc.MaxPositions != arc.MaxPositions {
		add("最大持仓数: %d → %d", brc.MaxPositions, arc.MaxPositions)
	}
	if brc.MinConfidence != arc.MinConfidence {
		add("最低信心度: %d → %d", brc.MinConfidence, arc.MinConfidence)
	}
	if brc.MinPositionSize != arc.MinPositionSize {
		add("最小仓位: %.1f → %.1f USDT", brc.MinPositionSize, arc.MinPositionSize)
	}
	if brc.BTCETHMaxLeverage != arc.BTCETHMaxLeverage {
		add("BTC/ETH 最大杠杆: %d → %d", brc.BTCETHMaxLeverage, arc.BTCETHMaxLeverage)
	}
	if brc.AltcoinMaxLeverage != arc.AltcoinMaxLeverage {
		add("山寨币最大杠杆: %d → %d", brc.AltcoinMaxLeverage, arc.AltcoinMaxLeverage)
	}
	if brc.BTCETHMaxPositionValueRatio != arc.BTCETHMaxPositionValueRatio {
		add("BTC/ETH 仓位占比上限: %.1f → %.1f", brc.BTCETHMaxPositionValueRatio, arc.BTCETHMaxPositionValueRatio)
	}
	if brc.AltcoinMaxPositionValueRatio != arc.AltcoinMaxPositionValueRatio {
		add("山寨币仓位占比上限: %.1f → %.1f", brc.AltcoinMaxPositionValueRatio, arc.AltcoinMaxPositionValueRatio)
	}
	if brc.MaxMarginUsage != arc.MaxMarginUsage {
		add("最大保证金占用: %.0f%% → %.0f%%", brc.MaxMarginUsage*100, arc.MaxMarginUsage*100)
	}
	if brc.MinRiskRewardRatio != arc.MinRiskRewardRatio {
		add("最小盈亏比: %.1f → %.1f", brc.MinRiskRewardRatio, arc.MinRiskRewardRatio)
	}
	if before.CustomPrompt != after.CustomPrompt {
		add("自定义提示词已更新")
	}
	return changes
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ============================================================================
// Market data formatting
// ============================================================================

// formatMarketForChat renders a compact market snapshot for the chat AI.
func formatMarketForChat(data *market.Data) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s 现价 %.4g USDT (1h %+.2f%% / 4h %+.2f%%)\n",
		data.Symbol, data.CurrentPrice, data.PriceChange1h, data.PriceChange4h)
	if data.OpenInterest != nil {
		fmt.Fprintf(&sb, "持仓量: %.4g\n", data.OpenInterest.Latest)
	}
	fmt.Fprintf(&sb, "资金费率: %.4f%%\n", data.FundingRate*100)

	timeframes := make([]string, 0, len(data.TimeframeData))
	for tf := range data.TimeframeData {
		timeframes = append(timeframes, tf)
	}
	sortStrings(timeframes)
	for _, tf := range timeframes {
		series := data.TimeframeData[tf]
		if series == nil {
			continue
		}
		fmt.Fprintf(&sb, "\n[%s] ", tf)
		if n := len(series.Klines); n > 0 {
			last := series.Klines[n-1]
			fmt.Fprintf(&sb, "收盘 %.4g H %.4g L %.4g | ", last.Close, last.High, last.Low)
		}
		if n := len(series.EMA20Values); n > 0 {
			fmt.Fprintf(&sb, "EMA20 %.4g ", series.EMA20Values[n-1])
		}
		if n := len(series.EMA50Values); n > 0 {
			fmt.Fprintf(&sb, "EMA50 %.4g ", series.EMA50Values[n-1])
		}
		if n := len(series.MACDValues); n > 0 {
			fmt.Fprintf(&sb, "| MACD %.4g ", series.MACDValues[n-1])
		}
		if n := len(series.RSI14Values); n > 0 {
			fmt.Fprintf(&sb, "| RSI14 %.1f ", series.RSI14Values[n-1])
		} else if n := len(series.RSI7Values); n > 0 {
			fmt.Fprintf(&sb, "| RSI7 %.1f ", series.RSI7Values[n-1])
		}
		if series.ATR14 > 0 {
			fmt.Fprintf(&sb, "| ATR14 %.4g", series.ATR14)
		}
		if n := len(series.BOLLUpper); n > 0 {
			fmt.Fprintf(&sb, "\nBOLL 上 %.4g / 中 %.4g / 下 %.4g",
				series.BOLLUpper[n-1], series.BOLLMiddle[n-1], series.BOLLLower[n-1])
		}
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// telegramAgentSystemPrompt builds the system prompt for agentic chat.
func (at *AutoTrader) telegramAgentSystemPrompt() string {
	var sb strings.Builder
	sb.WriteString(at.telegramChatContext())
	sb.WriteString("\n你可以调用工具。规则:\n")
	sb.WriteString("- 回答涉及价格、行情、指标时，必须先调用 get_market_data 或 get_latest_price 查询最新数据，不要编造价格。\n")
	sb.WriteString("- 涉及账户/持仓状态时用 get_account_status / get_positions。\n")
	sb.WriteString("- 用户要求平仓、提前止损、认错离场时：先 get_positions 确认持仓存在，再用 close_position 执行（市价全平，不受最小持仓时间限制），然后把成交价和盈亏告诉用户。\n")
	sb.WriteString("- 用户要求收紧/移动止损、调整止盈时用 adjust_protection（多头止损须低于现价，空头止损须高于现价）。\n")
	sb.WriteString("- close_position 和 adjust_protection 是真实执行操作：只在用户明确表达该意图时调用；拿不准就先和用户确认，绝不自作主张开仓或平仓。\n")
	sb.WriteString("- 用户要求修改策略时，先用 get_strategy 查看当前配置，再用 update_strategy 提交部分 patch（未提到的字段保持不变），修改成功后用中文向用户报告具体变更。\n")
	sb.WriteString("- 工具返回 ERROR 时如实告知用户原因。\n")
	sb.WriteString("- 回答用中文，控制在 800 字以内，纯文本。")
	return sb.String()
}
