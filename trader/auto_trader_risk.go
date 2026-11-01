package trader

import (
	"fmt"
	"nofx/logger"
	"strings"
)

// ============================================================================
// Risk-control helpers (CODE ENFORCED, applied on top of kernel validation)
// ============================================================================

// enforcePositionValueRatio caps position value at equity × ratio for the
// asset tier (BTC/ETH higher tier, altcoins lower tier). Returns the capped
// size and whether a cap was applied.
func (at *AutoTrader) enforcePositionValueRatio(positionSizeUSD, equity float64, symbol string) (float64, bool) {
	riskControl := at.currentEngine().GetRiskControlConfig()

	maxPositionValueRatio := riskControl.AltcoinMaxPositionValueRatio
	if maxPositionValueRatio <= 0 {
		maxPositionValueRatio = 1.0 // Default: 1x equity for altcoins
	}
	if isBTCETH(symbol) {
		maxPositionValueRatio = riskControl.BTCETHMaxPositionValueRatio
		if maxPositionValueRatio <= 0 {
			maxPositionValueRatio = 5.0 // Default: 5x equity for BTC/ETH
		}
	}

	maxPositionValue := equity * maxPositionValueRatio
	if positionSizeUSD > maxPositionValue {
		logger.Infof("  ⚠️ [RISK CONTROL] Position %.2f USDT exceeds limit (equity %.2f × %.1fx = %.2f USDT max for %s), capping",
			positionSizeUSD, equity, maxPositionValueRatio, maxPositionValue, symbol)
		return maxPositionValue, true
	}
	return positionSizeUSD, false
}

// enforceMinPositionSize rejects positions below the configured minimum.
func (at *AutoTrader) enforceMinPositionSize(positionSizeUSD float64) error {
	riskControl := at.currentEngine().GetRiskControlConfig()
	minSize := riskControl.MinPositionSize
	if minSize <= 0 {
		minSize = 12 // Default: 12 USDT
	}
	if positionSizeUSD < minSize {
		return fmt.Errorf("[RISK CONTROL] Position %.2f USDT below minimum (%.2f USDT)", positionSizeUSD, minSize)
	}
	return nil
}

// enforceMaxPositions rejects opens beyond the configured position count.
func (at *AutoTrader) enforceMaxPositions(currentPositionCount int) error {
	riskControl := at.currentEngine().GetRiskControlConfig()
	maxPositions := riskControl.MaxPositions
	if maxPositions <= 0 {
		maxPositions = 3 // Default: 3 positions
	}
	if currentPositionCount >= maxPositions {
		return fmt.Errorf("[RISK CONTROL] Already at max positions (%d/%d)", currentPositionCount, maxPositions)
	}
	return nil
}

// isBTCETH checks if a symbol is a BTC or ETH pair.
func isBTCETH(symbol string) bool {
	symbol = strings.ToUpper(symbol)
	return strings.HasPrefix(symbol, "BTC") || strings.HasPrefix(symbol, "ETH")
}
