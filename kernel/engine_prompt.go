package kernel

import (
	"fmt"
	"nofx/market"
	"nofx/store"
	"strings"
	"time"
)

// ============================================================================
// Prompt Building - System Prompt
// ============================================================================

// BuildSystemPrompt builds System Prompt according to strategy configuration.
// System prompts are intentionally English-only; the model contract stays
// language-stable for reproducible trading behavior.
func (e *StrategyEngine) BuildSystemPrompt(accountEquity float64, variant string) string {
	var sb strings.Builder
	riskControl := e.config.RiskControl
	promptSections := e.config.PromptSections

	// Configs created in the Chinese-UI era carry legacy stored prompt sections
	// written for a different contract; ignore them wholesale and fall back to
	// the canonical built-in English sections.
	legacyZhConfig := strings.EqualFold(strings.TrimSpace(e.config.Language), "zh")
	if legacyZhConfig {
		promptSections = store.PromptSectionsConfig{}
	}

	// 0. Data Dictionary & Schema (ensure AI understands all fields)
	sb.WriteString(GetSchemaPrompt(LangEnglish))
	sb.WriteString("\n\n")
	sb.WriteString("---\n\n")

	// 1. Role definition
	roleDefinition := englishOnlyPromptSection(promptSections.RoleDefinition)
	if roleDefinition != "" {
		sb.WriteString(roleDefinition)
		sb.WriteString("\n\n")
	} else {
		sb.WriteString("# You are a professional OKX USDT perpetual multi-asset trading AI\n\n")
		sb.WriteString("Your task is to make trading decisions based on the provided market data.\n\n")
	}

	// 2. Trading mode variant
	writeModeVariant(&sb, variant)

	// 3. Hard constraints (risk control).
	btcEthPosValueRatio := riskControl.BTCETHMaxPositionValueRatio
	if btcEthPosValueRatio <= 0 {
		btcEthPosValueRatio = 5.0
	}
	altcoinPosValueRatio := riskControl.AltcoinMaxPositionValueRatio
	if altcoinPosValueRatio <= 0 {
		altcoinPosValueRatio = 1.0
	}

	singleSymbol, primarySymbol := e.singleSymbolInfo()
	writeHardConstraints(&sb, accountEquity, riskControl, btcEthPosValueRatio, altcoinPosValueRatio, singleSymbol, primarySymbol)

	// 4. Trading frequency
	tradingFrequency := englishOnlyPromptSection(promptSections.TradingFrequency)
	if tradingFrequency != "" {
		sb.WriteString(tradingFrequency)
		sb.WriteString("\n\n")
	} else {
		sb.WriteString("# ⏱️ Trading Frequency Awareness\n\n")
		sb.WriteString("- Excellent traders: 2-4 trades/day ≈ 0.1-0.2 trades/hour\n")
		sb.WriteString("- >2 trades/hour = overtrading\n")
		sb.WriteString("- Single position hold time ≥ 45-90 minutes\n")
		sb.WriteString("If you find yourself trading every cycle → standards too low; if closing positions < 45 minutes → too impulsive.\n\n")
	}

	// 5. Entry standards
	entryStandards := englishOnlyPromptSection(promptSections.EntryStandards)
	if entryStandards != "" {
		sb.WriteString(entryStandards)
		sb.WriteString("\n\nYou have the following indicator data:\n")
		e.writeAvailableIndicators(&sb)
		sb.WriteString(fmt.Sprintf("\n**Confidence ≥ %d** required to open positions.\n\n", riskControl.MinConfidence))
	} else {
		sb.WriteString("# 🎯 Entry Standards (Strict)\n\n")
		sb.WriteString("Only open positions when multiple signals resonate. You have:\n")
		e.writeAvailableIndicators(&sb)
		sb.WriteString(fmt.Sprintf("\nFeel free to use any effective analysis method, but **confidence ≥ %d** is required to open positions; avoid low-quality behaviors such as single-indicator entries, contradictory signals, sideways chop, or re-entering immediately after a close.\n\n", riskControl.MinConfidence))
	}

	// 6. Decision process
	decisionProcess := englishOnlyPromptSection(promptSections.DecisionProcess)
	if decisionProcess != "" {
		sb.WriteString(decisionProcess)
		sb.WriteString("\n\n")
	} else {
		sb.WriteString("# 📋 Decision Process\n\n")
		sb.WriteString("1. Check positions → take profit / stop loss?\n")
		sb.WriteString("2. Scan candidates + multi-timeframe → are there strong signals?\n")
		sb.WriteString("3. Write chain of thought first, then output structured JSON\n\n")
	}

	// 7. Output format — schema spec stays in English (parser contract).
	writeOutputFormat(&sb, accountEquity, btcEthPosValueRatio, riskControl, singleSymbol, primarySymbol)

	// 8. Custom Prompt
	customPrompt := englishOnlyPromptSection(e.config.CustomPrompt)
	if legacyZhConfig {
		customPrompt = ""
	}
	if customPrompt != "" {
		sb.WriteString("# 📌 Personalized Trading Strategy\n\n")
		sb.WriteString(customPrompt)
		sb.WriteString("\n\n")
		sb.WriteString("Note: the above personalized strategy supplements the basic rules and may not violate the core risk controls.\n")
	}

	return sb.String()
}

func englishOnlyPromptSection(section string) string {
	trimmed := strings.TrimSpace(section)
	if trimmed == "" {
		return ""
	}
	if detectLanguage(trimmed) == LangChinese {
		return ""
	}
	return trimmed
}

// singleSymbolInfo returns (true, "BTCUSDT") for static-coin strategies that
// trade exactly one instrument. Multi-symbol strategies return (false, "").
func (e *StrategyEngine) singleSymbolInfo() (bool, string) {
	coinSource := e.config.CoinSource
	if len(coinSource.StaticCoins) == 1 {
		return true, market.Normalize(coinSource.StaticCoins[0])
	}
	return false, ""
}

func writeModeVariant(sb *strings.Builder, variant string) {
	switch strings.ToLower(strings.TrimSpace(variant)) {
	case "aggressive":
		sb.WriteString("## Mode: Aggressive\n- Prioritize capturing trend breakouts; may scale in when confidence ≥ 70\n- Allow larger positions, but must strictly set stop-loss and explain the risk-reward ratio\n\n")
	case "conservative":
		sb.WriteString("## Mode: Conservative\n- Open positions only when multiple signals resonate\n- Prioritize capital preservation; pause for multiple periods after consecutive losses\n\n")
	case "scalping":
		sb.WriteString("## Mode: Scalping\n- Focus on short-term momentum, smaller profit targets but require quick action\n- If price doesn't move as expected within two bars, immediately reduce position or stop-loss\n\n")
	}
}

func writeHardConstraints(sb *strings.Builder, accountEquity float64, riskControl store.RiskControlConfig, btcEthPosValueRatio, altcoinPosValueRatio float64, singleSymbol bool, primarySymbol string) {
	sb.WriteString("# Hard Constraints (Risk Control)\n\n")
	sb.WriteString("## CODE ENFORCED (backend validation, cannot be bypassed):\n")
	sb.WriteString(fmt.Sprintf("- Max Positions: %d instruments simultaneously\n", riskControl.MaxPositions))

	if singleSymbol {
		// One symbol — pick the higher of the two configured ratios so the
		// limit isn't accidentally clamped to the altcoin cap.
		ratio := altcoinPosValueRatio
		if btcEthPosValueRatio > ratio {
			ratio = btcEthPosValueRatio
		}
		maxVal := accountEquity * ratio
		sb.WriteString(fmt.Sprintf("- Position Value Limit (%s): max %.0f USDT (= equity %.0f × %.1fx)\n", primarySymbol, maxVal, accountEquity, ratio))
	} else {
		sb.WriteString(fmt.Sprintf("- Position Value Limit (Altcoin): max %.0f USDT (= equity %.0f × %.1fx)\n", accountEquity*altcoinPosValueRatio, accountEquity, altcoinPosValueRatio))
		sb.WriteString(fmt.Sprintf("- Position Value Limit (BTC/ETH): max %.0f USDT (= equity %.0f × %.1fx)\n", accountEquity*btcEthPosValueRatio, accountEquity, btcEthPosValueRatio))
	}

	sb.WriteString(fmt.Sprintf("- Max Margin Usage: ≤%.0f%%\n", riskControl.MaxMarginUsage*100))
	sb.WriteString(fmt.Sprintf("- Min Position Size: ≥%.0f USDT\n\n", riskControl.MinPositionSize))
	sb.WriteString("## AI GUIDED (recommended):\n")

	if singleSymbol {
		lev := riskControl.AltcoinMaxLeverage
		if riskControl.BTCETHMaxLeverage > lev {
			lev = riskControl.BTCETHMaxLeverage
		}
		sb.WriteString(fmt.Sprintf("- Trading Leverage (%s): max %dx\n", primarySymbol, lev))
	} else {
		sb.WriteString(fmt.Sprintf("- Trading Leverage: Altcoin max %dx | BTC/ETH max %dx\n", riskControl.AltcoinMaxLeverage, riskControl.BTCETHMaxLeverage))
	}
	sb.WriteString(fmt.Sprintf("- Risk-Reward Ratio: ≥1:%.1f (take_profit / stop_loss)\n", riskControl.MinRiskRewardRatio))
	sb.WriteString(fmt.Sprintf("- Min Confidence: ≥%d to open position\n\n", riskControl.MinConfidence))

	// Position sizing guidance
	exampleRatio := btcEthPosValueRatio
	if singleSymbol {
		exampleRatio = altcoinPosValueRatio
		if btcEthPosValueRatio > exampleRatio {
			exampleRatio = btcEthPosValueRatio
		}
	}
	sb.WriteString("## Position Sizing Guidance\n")
	sb.WriteString("Calculate `position_size_usd` from your confidence and the Position Value Limits above:\n")
	sb.WriteString("- High confidence (≥85): use 80-100%% of the position value limit\n")
	sb.WriteString("- Medium confidence (70-84): use 50-80%% of the position value limit\n")
	sb.WriteString("- Low confidence (60-69): use 30-50%% of the position value limit\n")
	sb.WriteString(fmt.Sprintf("- Example: equity %.0f × %.1fx = max %.0f USDT\n", accountEquity, exampleRatio, accountEquity*exampleRatio))
	sb.WriteString("- **DO NOT** just use available_balance as position_size_usd. Use the Position Value Limit!\n\n")
}

func writeOutputFormat(sb *strings.Builder, accountEquity, btcEthPosValueRatio float64, riskControl store.RiskControlConfig, singleSymbol bool, primarySymbol string) {
	// Output format schema MUST stay English/structural; parser depends on it.
	sb.WriteString("# Output Format (Strictly Follow)\n\n")
	sb.WriteString("**Must use XML tags <reasoning> and <decision> to separate chain of thought and decision JSON, avoiding parsing errors**\n\n")
	sb.WriteString("## Format Requirements\n\n")
	sb.WriteString("<reasoning>\n")
	sb.WriteString("Your chain of thought analysis...\n- Briefly analyze your thinking process\n")
	sb.WriteString("</reasoning>\n\n")
	sb.WriteString("<decision>\n")
	sb.WriteString("Step 2: JSON decision array\n\n")
	sb.WriteString("```json\n[\n")

	if singleSymbol {
		lev := riskControl.AltcoinMaxLeverage
		if riskControl.BTCETHMaxLeverage > lev {
			lev = riskControl.BTCETHMaxLeverage
		}
		ratio := btcEthPosValueRatio
		size := accountEquity * ratio
		sb.WriteString(fmt.Sprintf("  {\"symbol\": \"%s\", \"action\": \"open_long\", \"leverage\": %d, \"position_size_usd\": %.0f, \"stop_loss\": 0, \"take_profit\": 0, \"confidence\": 85, \"risk_usd\": 0},\n", primarySymbol, lev, size))
		sb.WriteString(fmt.Sprintf("  {\"symbol\": \"%s\", \"action\": \"wait\"}\n", primarySymbol))
	} else {
		examplePositionSize := accountEquity * btcEthPosValueRatio
		sb.WriteString(fmt.Sprintf("  {\"symbol\": \"BTCUSDT\", \"action\": \"open_short\", \"leverage\": %d, \"position_size_usd\": %.0f, \"stop_loss\": 97000, \"take_profit\": 91000, \"confidence\": 85, \"risk_usd\": 300},\n",
			riskControl.BTCETHMaxLeverage, examplePositionSize))
		sb.WriteString("  {\"symbol\": \"ETHUSDT\", \"action\": \"close_long\"}\n")
	}
	sb.WriteString("]\n```\n")
	sb.WriteString("</decision>\n\n")

	sb.WriteString("## Field Description\n\n")
	sb.WriteString("- `action`: open_long | open_short | close_long | close_short | hold | wait\n")
	sb.WriteString(fmt.Sprintf("- `confidence`: 0-100 (opening recommended ≥ %d)\n", riskControl.MinConfidence))
	sb.WriteString("- Required when opening: leverage, position_size_usd, stop_loss, take_profit, confidence, risk_usd\n")
	sb.WriteString("- **IMPORTANT**: all numeric values must be calculated numbers, NOT formulas/expressions (e.g. use `27.76`, not `3000 * 0.01`)\n")
	if singleSymbol {
		sb.WriteString(fmt.Sprintf("- **This strategy trades only %s.** The JSON `symbol` MUST match `%s` exactly — do not add USDT/USDC suffix variants.\n", primarySymbol, primarySymbol))
	}
	sb.WriteString("\n")
}

func (e *StrategyEngine) writeAvailableIndicators(sb *strings.Builder) {
	indicators := e.config.Indicators
	kline := indicators.Klines

	sb.WriteString(fmt.Sprintf("- %s price series", kline.PrimaryTimeframe))
	if kline.EnableMultiTimeframe {
		sb.WriteString(fmt.Sprintf(" + %s K-line series\n", kline.LongerTimeframe))
	} else {
		sb.WriteString("\n")
	}

	if indicators.EnableEMA {
		sb.WriteString("- EMA indicators")
		if len(indicators.EMAPeriods) > 0 {
			sb.WriteString(fmt.Sprintf(" (periods: %v)", indicators.EMAPeriods))
		}
		sb.WriteString("\n")
	}
	if indicators.EnableMACD {
		sb.WriteString("- MACD indicators\n")
	}
	if indicators.EnableRSI {
		sb.WriteString("- RSI indicators")
		if len(indicators.RSIPeriods) > 0 {
			sb.WriteString(fmt.Sprintf(" (periods: %v)", indicators.RSIPeriods))
		}
		sb.WriteString("\n")
	}
	if indicators.EnableATR {
		sb.WriteString("- ATR indicators")
		if len(indicators.ATRPeriods) > 0 {
			sb.WriteString(fmt.Sprintf(" (periods: %v)", indicators.ATRPeriods))
		}
		sb.WriteString("\n")
	}
	if indicators.EnableBOLL {
		sb.WriteString("- Bollinger Bands (BOLL) - Upper/Middle/Lower bands")
		if len(indicators.BOLLPeriods) > 0 {
			sb.WriteString(fmt.Sprintf(" (periods: %v)", indicators.BOLLPeriods))
		}
		sb.WriteString("\n")
	}
	if indicators.EnableVolume {
		sb.WriteString("- Volume data\n")
	}
	if indicators.EnableOI {
		sb.WriteString("- Open Interest (OI) data\n")
	}
	if indicators.EnableFundingRate {
		sb.WriteString("- Funding rate\n")
	}
}

// ============================================================================
// Prompt Building - User Prompt
// ============================================================================

// BuildUserPrompt builds User Prompt based on strategy configuration
func (e *StrategyEngine) BuildUserPrompt(ctx *Context) string {
	var sb strings.Builder

	// System status
	sb.WriteString(fmt.Sprintf("Time: %s | Period: #%d | Runtime: %d minutes\n\n",
		ctx.CurrentTime, ctx.CallCount, ctx.RuntimeMinutes))

	// BTC market
	if btcData, hasBTC := ctx.MarketDataMap["BTCUSDT"]; hasBTC {
		sb.WriteString(fmt.Sprintf("BTC: %.2f (1h: %+.2f%%, 4h: %+.2f%%) | MACD: %.4f | RSI: %.2f\n\n",
			btcData.CurrentPrice, btcData.PriceChange1h, btcData.PriceChange4h,
			btcData.CurrentMACD, btcData.CurrentRSI7))
	}

	// Account information
	sb.WriteString(fmt.Sprintf("Account: Equity %.2f | Balance %.2f (%.1f%%) | PnL %+.2f%% | Margin %.1f%% | Positions %d\n\n",
		ctx.Account.TotalEquity,
		ctx.Account.AvailableBalance,
		(ctx.Account.AvailableBalance/ctx.Account.TotalEquity)*100,
		ctx.Account.TotalPnLPct,
		ctx.Account.MarginUsedPct,
		ctx.Account.PositionCount))

	// Recently completed orders (placed before positions to ensure visibility)
	if len(ctx.RecentOrders) > 0 {
		sb.WriteString("## Recent Completed Trades\n")
		for i, order := range ctx.RecentOrders {
			resultStr := "Profit"
			if order.RealizedPnL < 0 {
				resultStr = "Loss"
			}
			sb.WriteString(fmt.Sprintf("%d. %s %s | Entry %.4f Exit %.4f | %s: %+.2f USDT (%+.2f%%) | %s→%s (%s)\n",
				i+1, order.Symbol, order.Side,
				order.EntryPrice, order.ExitPrice,
				resultStr, order.RealizedPnL, order.PnLPct,
				order.EntryTime, order.ExitTime, order.HoldDuration))
		}
		sb.WriteString("\n")
	}

	// Historical trading statistics (helps AI understand past performance)
	if ctx.TradingStats != nil && ctx.TradingStats.TotalTrades > 0 {
		var winLossRatio float64
		if ctx.TradingStats.AvgLoss > 0 {
			winLossRatio = ctx.TradingStats.AvgWin / ctx.TradingStats.AvgLoss
		}

		sb.WriteString("## Historical Trading Statistics\n")
		sb.WriteString(fmt.Sprintf("Total Trades: %d | Profit Factor: %.2f | Sharpe: %.2f | Win/Loss Ratio: %.2f\n",
			ctx.TradingStats.TotalTrades,
			ctx.TradingStats.ProfitFactor,
			ctx.TradingStats.SharpeRatio,
			winLossRatio))
		sb.WriteString(fmt.Sprintf("Total PnL: %+.2f USDT | Avg Win: +%.2f | Avg Loss: -%.2f | Max Drawdown: %.1f%%\n",
			ctx.TradingStats.TotalPnL,
			ctx.TradingStats.AvgWin,
			ctx.TradingStats.AvgLoss,
			ctx.TradingStats.MaxDrawdownPct))

		// Performance hints based on profit factor, sharpe, and drawdown
		if ctx.TradingStats.ProfitFactor >= 1.5 && ctx.TradingStats.SharpeRatio >= 1 {
			sb.WriteString("Performance: GOOD - maintain current strategy\n")
		} else if ctx.TradingStats.ProfitFactor < 1 {
			sb.WriteString("Performance: NEEDS IMPROVEMENT - improve win/loss ratio, optimize TP/SL\n")
		} else if ctx.TradingStats.MaxDrawdownPct > 30 {
			sb.WriteString("Performance: HIGH RISK - reduce position size, control drawdown\n")
		} else {
			sb.WriteString("Performance: NORMAL - room for optimization\n")
		}
		sb.WriteString("\n")
	}

	// Position information
	if len(ctx.Positions) > 0 {
		sb.WriteString("## Current Positions\n")
		for i, pos := range ctx.Positions {
			sb.WriteString(e.formatPositionInfo(i+1, pos, ctx))
		}
	} else {
		sb.WriteString("Current Positions: None\n\n")
	}

	// Candidate coins (exclude coins already in positions to avoid duplicate data)
	positionSymbols := make(map[string]bool)
	for _, pos := range ctx.Positions {
		// Normalize symbol to handle both "ETH" and "ETHUSDT" formats
		normalizedSymbol := market.Normalize(pos.Symbol)
		positionSymbols[normalizedSymbol] = true
	}

	sb.WriteString(fmt.Sprintf("## Candidate Coins (%d coins)\n\n", len(ctx.MarketDataMap)))
	displayedCount := 0
	for _, coin := range ctx.CandidateCoins {
		// Skip if this coin is already a position (data already shown in positions section)
		normalizedCoinSymbol := market.Normalize(coin.Symbol)
		if positionSymbols[normalizedCoinSymbol] {
			continue
		}

		marketData, hasData := ctx.MarketDataMap[coin.Symbol]
		if !hasData {
			continue
		}
		displayedCount++

		sourceTags := e.formatCoinSourceTag(coin.Sources)
		sb.WriteString(fmt.Sprintf("### %d. %s%s\n\n", displayedCount, coin.Symbol, sourceTags))
		sb.WriteString(e.formatMarketData(marketData))
		sb.WriteString("\n")
	}
	sb.WriteString("\n")

	sb.WriteString("---\n\n")
	sb.WriteString("Now please analyze briefly and output the decision JSON.\n")

	return sb.String()
}

func (e *StrategyEngine) formatPositionInfo(index int, pos PositionInfo, ctx *Context) string {
	var sb strings.Builder

	holdingDuration := ""
	if pos.UpdateTime > 0 {
		durationMs := time.Now().UnixMilli() - pos.UpdateTime
		durationMin := durationMs / (1000 * 60)
		if durationMin < 60 {
			holdingDuration = fmt.Sprintf(" | Holding Duration %d min", durationMin)
		} else {
			durationHour := durationMin / 60
			durationMinRemainder := durationMin % 60
			holdingDuration = fmt.Sprintf(" | Holding Duration %dh %dm", durationHour, durationMinRemainder)
		}
	}

	positionValue := pos.Quantity * pos.MarkPrice
	if positionValue < 0 {
		positionValue = -positionValue
	}

	sb.WriteString(fmt.Sprintf("%d. %s %s | Entry %.4f Current %.4f | Qty %.4f | Position Value %.2f USDT | PnL%+.2f%% | PnL Amount%+.2f USDT | Peak PnL%.2f%% | Leverage %dx | Margin %.0f | Liq Price %.4f%s\n\n",
		index, pos.Symbol, strings.ToUpper(pos.Side),
		pos.EntryPrice, pos.MarkPrice, pos.Quantity, positionValue, pos.UnrealizedPnLPct, pos.UnrealizedPnL, pos.PeakPnLPct,
		pos.Leverage, pos.MarginUsed, pos.LiquidationPrice, holdingDuration))

	if marketData, ok := ctx.MarketDataMap[pos.Symbol]; ok {
		sb.WriteString(e.formatMarketData(marketData))
		sb.WriteString("\n")
	}

	return sb.String()
}

func (e *StrategyEngine) formatCoinSourceTag(sources []string) string {
	if len(sources) == 1 && sources[0] == "static" {
		return " (Manual selection)"
	}
	return ""
}

// ============================================================================
// Market Data Formatting
// ============================================================================

func (e *StrategyEngine) formatMarketData(data *market.Data) string {
	var sb strings.Builder
	indicators := e.config.Indicators

	// Clearly label the coin symbol
	sb.WriteString(fmt.Sprintf("=== %s Market Data ===\n\n", data.Symbol))
	sb.WriteString(fmt.Sprintf("current_price = %.4f", data.CurrentPrice))

	if indicators.EnableEMA {
		sb.WriteString(fmt.Sprintf(", current_ema20 = %.3f", data.CurrentEMA20))
	}

	if indicators.EnableMACD {
		sb.WriteString(fmt.Sprintf(", current_macd = %.3f", data.CurrentMACD))
	}

	if indicators.EnableRSI {
		sb.WriteString(fmt.Sprintf(", current_rsi7 = %.3f", data.CurrentRSI7))
	}

	sb.WriteString("\n\n")

	if indicators.EnableOI || indicators.EnableFundingRate {
		sb.WriteString(fmt.Sprintf("Additional data for %s:\n\n", data.Symbol))

		if indicators.EnableOI {
			if data.OpenInterest == nil {
				sb.WriteString("Open Interest: unavailable (not a measured zero); OI liquidity has not been verified\n\n")
			} else {
				sb.WriteString(fmt.Sprintf("Open Interest: Latest: %.2f Average: %.2f\n\n",
					data.OpenInterest.Latest, data.OpenInterest.Average))
			}
		}

		if indicators.EnableFundingRate {
			sb.WriteString(fmt.Sprintf("Funding Rate: %.2e\n\n", data.FundingRate))
		}
	}

	if len(data.TimeframeData) > 0 {
		timeframeOrder := []string{"1m", "3m", "5m", "15m", "30m", "1h", "2h", "4h", "6h", "8h", "12h", "1d", "3d", "1w"}
		for _, tf := range timeframeOrder {
			if tfData, ok := data.TimeframeData[tf]; ok {
				sb.WriteString(fmt.Sprintf("=== %s Timeframe (oldest → latest) ===\n\n", strings.ToUpper(tf)))
				e.formatTimeframeSeriesData(&sb, tfData, indicators)
			}
		}
	} else {
		// Compatible with old data format
		if data.IntradaySeries != nil {
			klineConfig := indicators.Klines
			sb.WriteString(fmt.Sprintf("Intraday series (%s intervals, oldest → latest):\n\n", klineConfig.PrimaryTimeframe))

			if len(data.IntradaySeries.MidPrices) > 0 {
				sb.WriteString(fmt.Sprintf("Mid prices: %s\n\n", formatFloatSlice(data.IntradaySeries.MidPrices)))
			}

			if indicators.EnableEMA && len(data.IntradaySeries.EMA20Values) > 0 {
				sb.WriteString(fmt.Sprintf("EMA indicators (20-period): %s\n\n", formatFloatSlice(data.IntradaySeries.EMA20Values)))
			}

			if indicators.EnableMACD && len(data.IntradaySeries.MACDValues) > 0 {
				sb.WriteString(fmt.Sprintf("MACD indicators: %s\n\n", formatFloatSlice(data.IntradaySeries.MACDValues)))
			}

			if indicators.EnableRSI {
				if len(data.IntradaySeries.RSI7Values) > 0 {
					sb.WriteString(fmt.Sprintf("RSI indicators (7-Period): %s\n\n", formatFloatSlice(data.IntradaySeries.RSI7Values)))
				}
				if len(data.IntradaySeries.RSI14Values) > 0 {
					sb.WriteString(fmt.Sprintf("RSI indicators (14-Period): %s\n\n", formatFloatSlice(data.IntradaySeries.RSI14Values)))
				}
			}

			if indicators.EnableVolume && len(data.IntradaySeries.Volume) > 0 {
				sb.WriteString(fmt.Sprintf("Volume: %s\n\n", formatFloatSlice(data.IntradaySeries.Volume)))
			}

			if indicators.EnableATR {
				sb.WriteString(fmt.Sprintf("3m ATR (14-period): %.3f\n\n", data.IntradaySeries.ATR14))
			}
		}

		if data.LongerTermContext != nil && indicators.Klines.EnableMultiTimeframe {
			sb.WriteString(fmt.Sprintf("Longer-term context (%s timeframe):\n\n", indicators.Klines.LongerTimeframe))

			if indicators.EnableEMA {
				sb.WriteString(fmt.Sprintf("20-Period EMA: %.3f vs. 50-Period EMA: %.3f\n\n",
					data.LongerTermContext.EMA20, data.LongerTermContext.EMA50))
			}

			if indicators.EnableATR {
				sb.WriteString(fmt.Sprintf("3-Period ATR: %.3f vs. 14-Period ATR: %.3f\n\n",
					data.LongerTermContext.ATR3, data.LongerTermContext.ATR14))
			}

			if indicators.EnableVolume {
				sb.WriteString(fmt.Sprintf("Current Volume: %.3f vs. Average Volume: %.3f\n\n",
					data.LongerTermContext.CurrentVolume, data.LongerTermContext.AverageVolume))
			}

			if indicators.EnableMACD && len(data.LongerTermContext.MACDValues) > 0 {
				sb.WriteString(fmt.Sprintf("MACD indicators: %s\n\n", formatFloatSlice(data.LongerTermContext.MACDValues)))
			}

			if indicators.EnableRSI && len(data.LongerTermContext.RSI14Values) > 0 {
				sb.WriteString(fmt.Sprintf("RSI indicators (14-Period): %s\n\n", formatFloatSlice(data.LongerTermContext.RSI14Values)))
			}
		}
	}

	return sb.String()
}

func (e *StrategyEngine) formatTimeframeSeriesData(sb *strings.Builder, data *market.TimeframeSeriesData, indicators store.IndicatorConfig) {
	if len(data.Klines) > 0 {
		sb.WriteString("Time(UTC)      Open      High      Low       Close     Volume\n")
		for i, k := range data.Klines {
			t := time.Unix(k.Time/1000, 0).UTC()
			timeStr := t.Format("01-02 15:04")
			marker := ""
			if i == len(data.Klines)-1 {
				marker = "  <- current"
			}
			sb.WriteString(fmt.Sprintf("%-14s %-9.4f %-9.4f %-9.4f %-9.4f %-12.2f%s\n",
				timeStr, k.Open, k.High, k.Low, k.Close, k.Volume, marker))
		}
		sb.WriteString("\n")
	} else if len(data.MidPrices) > 0 {
		sb.WriteString(fmt.Sprintf("Mid prices: %s\n\n", formatFloatSlice(data.MidPrices)))
		if indicators.EnableVolume && len(data.Volume) > 0 {
			sb.WriteString(fmt.Sprintf("Volume: %s\n\n", formatFloatSlice(data.Volume)))
		}
	}

	if indicators.EnableEMA {
		if len(data.EMA20Values) > 0 {
			sb.WriteString(fmt.Sprintf("EMA20: %s\n", formatFloatSlice(data.EMA20Values)))
		}
		if len(data.EMA50Values) > 0 {
			sb.WriteString(fmt.Sprintf("EMA50: %s\n", formatFloatSlice(data.EMA50Values)))
		}
	}

	if indicators.EnableMACD && len(data.MACDValues) > 0 {
		sb.WriteString(fmt.Sprintf("MACD: %s\n", formatFloatSlice(data.MACDValues)))
	}

	if indicators.EnableRSI {
		if len(data.RSI7Values) > 0 {
			sb.WriteString(fmt.Sprintf("RSI7: %s\n", formatFloatSlice(data.RSI7Values)))
		}
		if len(data.RSI14Values) > 0 {
			sb.WriteString(fmt.Sprintf("RSI14: %s\n", formatFloatSlice(data.RSI14Values)))
		}
	}

	if indicators.EnableATR && data.ATR14 > 0 {
		sb.WriteString(fmt.Sprintf("ATR14: %.4f\n", data.ATR14))
	}

	if indicators.EnableBOLL && len(data.BOLLUpper) > 0 {
		sb.WriteString(fmt.Sprintf("BOLL Upper: %s\n", formatFloatSlice(data.BOLLUpper)))
		sb.WriteString(fmt.Sprintf("BOLL Middle: %s\n", formatFloatSlice(data.BOLLMiddle)))
		sb.WriteString(fmt.Sprintf("BOLL Lower: %s\n", formatFloatSlice(data.BOLLLower)))
	}

	sb.WriteString("\n")
}

func formatFloatSlice(values []float64) string {
	strValues := make([]string, len(values))
	for i, v := range values {
		strValues[i] = fmt.Sprintf("%.4f", v)
	}
	return "[" + strings.Join(strValues, ", ") + "]"
}
