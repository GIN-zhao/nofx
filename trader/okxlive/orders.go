package okxlive

import (
	"fmt"
	"strings"
	"time"

	"nofx/logger"
	"nofx/market"
)

// orderSideForOpen: hedge mode — open long = buy/long, open short = sell/short.
func openSide(posSide string) string {
	if posSide == "long" {
		return "buy"
	}
	return "sell"
}

// closeSide: closing a long = sell/long, closing a short = buy/short.
func closeSide(posSide string) string {
	if posSide == "long" {
		return "sell"
	}
	return "buy"
}

func normalizePosSide(positionSide string) string {
	if strings.EqualFold(strings.TrimSpace(positionSide), "short") {
		return "short"
	}
	return "long"
}

// OpenLong opens a long position (market fill) with leverage applied.
func (b *Broker) OpenLong(symbol string, quantity float64, leverage int) (map[string]any, error) {
	return b.openPosition(symbol, "long", quantity, leverage)
}

// OpenShort opens a short position (market fill) with leverage applied.
func (b *Broker) OpenShort(symbol string, quantity float64, leverage int) (map[string]any, error) {
	return b.openPosition(symbol, "short", quantity, leverage)
}

func (b *Broker) openPosition(symbol, posSide string, quantity float64, leverage int) (map[string]any, error) {
	instID := market.ToOKXInstID(symbol)
	inst, err := b.getInstrument(symbol)
	if err != nil {
		return nil, err
	}
	contracts, err := contractsForQuantity(quantity, inst)
	if err != nil {
		return nil, err
	}
	sz := formatContracts(contracts, inst)

	_ = b.cancelAlgos(symbol, posSide) // stale protection from a previous position
	if err := b.setLeverage(instID, leverage); err != nil {
		logger.Warnf("  ⚠️ [OKX Live] set leverage %dx on %s: %v", leverage, instID, err)
	}

	var results []struct {
		OrdId string `json:"ordId"`
		SCode string `json:"sCode"`
		SMsg  string `json:"sMsg"`
	}
	body := map[string]any{
		"instId":  instID,
		"tdMode":  b.marginMode(),
		"side":    openSide(posSide),
		"posSide": posSide,
		"ordType": "market",
		"sz":      sz,
	}
	if err := b.post(pathOrder, body, &results); err != nil {
		return nil, fmt.Errorf("open %s %s: %w", posSide, instID, err)
	}
	if len(results) == 0 || results[0].SCode != "0" {
		msg := "unknown error"
		if len(results) > 0 {
			msg = results[0].SMsg
		}
		return nil, fmt.Errorf("open %s %s rejected: %s", posSide, instID, msg)
	}

	// Market orders fill immediately; pull avg fill price/fee for the record.
	detail := b.waitOrderDetail(instID, results[0].OrdId)
	entry := detail.avgPx
	if entry <= 0 {
		entry, _ = b.GetMarketPrice(symbol)
	}

	logger.Infof("📝 [OKX Live] opened %s %s %s contracts @ %.8g (lev %dx, fee %.4g)",
		posSide, instID, sz, entry, leverage, detail.fee)
	return map[string]any{
		"orderId": results[0].OrdId,
		"symbol":  symbol,
		"side":    posSide,
		"price":   entry,
		"quantity": quantity,
		"fee":     detail.fee,
	}, nil
}

// CloseLong closes (all or part of) a long position.
func (b *Broker) CloseLong(symbol string, quantity float64) (map[string]any, error) {
	return b.closePosition(symbol, "long", quantity)
}

// CloseShort closes (all or part of) a short position.
func (b *Broker) CloseShort(symbol string, quantity float64) (map[string]any, error) {
	return b.closePosition(symbol, "short", quantity)
}

// closePosition: quantity <= 0 closes the whole position via the
// close-position endpoint; a positive quantity sends a reduce-only market
// order for a partial close.
func (b *Broker) closePosition(symbol, posSide string, quantity float64) (map[string]any, error) {
	posSide = normalizePosSide(posSide)
	instID := market.ToOKXInstID(symbol)

	inst, err := b.getInstrument(symbol)
	if err != nil {
		return nil, err
	}

	var ordID string
	if quantity <= 0 {
		var results []struct {
			OrdId string `json:"ordId"`
			SCode string `json:"sCode"`
			SMsg  string `json:"sMsg"`
		}
		body := map[string]any{
			"instId":  instID,
			"mgnMode": b.marginMode(),
			"posSide": posSide,
			"autoCxl": true, // cancel any pending algo protection with the position
		}
		if err := b.post(pathClosePosition, body, &results); err != nil {
			return nil, fmt.Errorf("close %s %s: %w", posSide, instID, err)
		}
		if len(results) == 0 || results[0].SCode != "0" {
			msg := "unknown error"
			if len(results) > 0 {
				msg = results[0].SMsg
			}
			return nil, fmt.Errorf("close %s %s rejected: %s", posSide, instID, msg)
		}
		ordID = results[0].OrdId
	} else {
		contracts, err := contractsForQuantity(quantity, inst)
		if err != nil {
			return nil, err
		}
		var results []struct {
			OrdId string `json:"ordId"`
			SCode string `json:"sCode"`
			SMsg  string `json:"sMsg"`
		}
		body := map[string]any{
			"instId":  instID,
			"tdMode":  b.marginMode(),
			"side":    closeSide(posSide),
			"posSide": posSide,
			"ordType": "market",
			"sz":      formatContracts(contracts, inst),
		}
		if err := b.post(pathOrder, body, &results); err != nil {
			return nil, fmt.Errorf("partial close %s %s: %w", posSide, instID, err)
		}
		if len(results) == 0 || results[0].SCode != "0" {
			msg := "unknown error"
			if len(results) > 0 {
				msg = results[0].SMsg
			}
			return nil, fmt.Errorf("partial close %s %s rejected: %s", posSide, instID, msg)
		}
		ordID = results[0].OrdId
	}

	detail := b.waitOrderDetail(instID, ordID)
	exit := detail.avgPx
	pnl := b.realizedPnLAfterClose(instID, posSide)

	logger.Infof("📝 [OKX Live] closed %s %s @ %.8g (PnL %.4g, fee %.4g)", posSide, instID, exit, pnl, detail.fee)

	// Remember: this close was broker-initiated, so CheckStops won't double-report it.
	b.markInitiatedClose(instID, posSide)

	return map[string]any{
		"orderId":     ordID,
		"symbol":      symbol,
		"side":        posSide,
		"price":       exit,
		"realizedPnl": pnl,
		"fee":         detail.fee,
	}, nil
}

// ============================================================================
// SL/TP (algo orders)
// ============================================================================

type pendingAlgo struct {
	AlgoID      string `json:"algoId"`
	InstID      string `json:"instId"`
	PosSide     string `json:"posSide"`
	OrdType     string `json:"ordType"` // conditional / oco
	SlTriggerPx string `json:"slTriggerPx"`
	TpTriggerPx string `json:"tpTriggerPx"`
	State       string `json:"state"`
}

// pendingAlgosFor lists live algo orders (SL/TP) for one instrument.
func (b *Broker) pendingAlgosFor(instID string) ([]pendingAlgo, error) {
	var out []pendingAlgo
	for _, ordType := range []string{"conditional", "oco"} {
		var rows []pendingAlgo
		path := fmt.Sprintf("%s?ordType=%s&instType=SWAP&instId=%s", pathAlgoPending, ordType, instID)
		if err := b.get(path, &rows); err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

func (b *Broker) cancelAlgos(symbol, posSide string) error {
	instID := market.ToOKXInstID(symbol)
	algos, err := b.pendingAlgosFor(instID)
	if err != nil {
		return err
	}
	var ids []map[string]string
	for _, a := range algos {
		if posSide != "" && !strings.EqualFold(a.PosSide, posSide) {
			continue
		}
		ids = append(ids, map[string]string{"algoId": a.AlgoID, "instId": instID})
	}
	if len(ids) == 0 {
		return nil
	}
	_, err = b.do("POST", pathCancelAlgos, ids)
	return err
}

// SetStopLoss attaches (or replaces) the stop-loss on the symbol's open position.
func (b *Broker) SetStopLoss(symbol string, positionSide string, quantity, stopPrice float64) error {
	return b.setProtection(symbol, normalizePosSide(positionSide), stopPrice, 0)
}

// SetTakeProfit attaches (or replaces) the take-profit on the open position.
func (b *Broker) SetTakeProfit(symbol string, positionSide string, quantity, takeProfitPrice float64) error {
	return b.setProtection(symbol, normalizePosSide(positionSide), 0, takeProfitPrice)
}

// setProtection merges the requested SL/TP with whatever is currently armed
// and replaces the position's algo order (conditional for one leg, oco for two).
func (b *Broker) setProtection(symbol, posSide string, stopLoss, takeProfit float64) error {
	instID := market.ToOKXInstID(symbol)
	inst, err := b.getInstrument(symbol)
	if err != nil {
		return err
	}

	// Carry over the untouched leg from the currently armed algo order.
	if algos, err := b.pendingAlgosFor(instID); err == nil {
		for _, a := range algos {
			if strings.EqualFold(a.PosSide, posSide) {
				if stopLoss <= 0 {
					stopLoss = parseFloat(a.SlTriggerPx)
				}
				if takeProfit <= 0 {
					takeProfit = parseFloat(a.TpTriggerPx)
				}
			}
		}
	}
	if stopLoss <= 0 && takeProfit <= 0 {
		return fmt.Errorf("setProtection: 止损/止盈至少需要一个正值")
	}

	if err := b.cancelAlgos(symbol, posSide); err != nil {
		logger.Warnf("  ⚠️ [OKX Live] cancel old protection on %s: %v", instID, err)
	}

	contracts := b.openContracts(instID, posSide, inst)
	body := map[string]any{
		"instId":  instID,
		"tdMode":  b.marginMode(),
		"posSide": posSide,
		"side":    closeSide(posSide),
		"sz":      formatContracts(contracts, inst),
		"slOrdPx": "-1", // market fill on trigger
		"tpOrdPx": "-1",
	}
	ordType := "conditional"
	if stopLoss > 0 && takeProfit > 0 {
		ordType = "oco"
		body["slTriggerPx"] = formatPrice(stopLoss)
		body["tpTriggerPx"] = formatPrice(takeProfit)
	} else if stopLoss > 0 {
		body["slTriggerPx"] = formatPrice(stopLoss)
	} else {
		body["tpTriggerPx"] = formatPrice(takeProfit)
	}
	body["ordType"] = ordType

	var results []struct {
		SCode string `json:"sCode"`
		SMsg  string `json:"sMsg"`
		AlgoId string `json:"algoId"`
	}
	if err := b.post(pathAlgoOrder, body, &results); err != nil {
		return fmt.Errorf("setProtection %s %s: %w", posSide, instID, err)
	}
	if len(results) == 0 || results[0].SCode != "0" {
		msg := "unknown error"
		if len(results) > 0 {
			msg = results[0].SMsg
		}
		return fmt.Errorf("setProtection %s %s rejected: %s", posSide, instID, msg)
	}
	logger.Infof("  🛡 [OKX Live] %s %s protection: SL %.8g TP %.8g (%s, algo %s)",
		posSide, instID, stopLoss, takeProfit, ordType, results[0].AlgoId)
	return nil
}

// CancelStopOrders clears SL/TP attached to the symbol's open positions.
func (b *Broker) CancelStopOrders(symbol string) error {
	return b.cancelAlgos(symbol, "")
}

// openContracts finds the contract count of the current open position so the
// replacement algo order covers it fully.
func (b *Broker) openContracts(instID, posSide string, inst *instrument) float64 {
	positions, err := b.getSwapPositions()
	if err != nil {
		return inst.MinSz
	}
	for _, p := range positions {
		if p.InstID == instID && strings.EqualFold(p.PosSide, posSide) {
			return parseFloat(p.Pos)
		}
	}
	return inst.MinSz
}

// setLeverage sets contract leverage for the instrument (ignore-already-set).
func (b *Broker) setLeverage(instID string, leverage int) error {
	body := map[string]any{
		"instId":  instID,
		"lever":   fmt.Sprintf("%d", leverage),
		"mgnMode": b.marginMode(),
	}
	_, err := b.do("POST", pathSetLeverage, body)
	if err != nil && (strings.Contains(err.Error(), "leverage not modified") ||
		strings.Contains(err.Error(), "same as system")) {
		return nil
	}
	return err
}

// ============================================================================
// Order detail (avg fill price / fee) with a short retry for fill latency
// ============================================================================

type orderFillInfo struct {
	avgPx float64
	fee   float64
}

func (b *Broker) waitOrderDetail(instID, ordID string) orderFillInfo {
	var info orderFillInfo
	if ordID == "" {
		return info
	}
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(400 * time.Millisecond)
		}
		var rows []struct {
			State string `json:"state"`
			AvgPx string `json:"avgPx"`
			Fee   string `json:"fee"`
		}
		path := fmt.Sprintf("%s?instId=%s&ordId=%s", pathOrderDetail, instID, ordID)
		if err := b.get(path, &rows); err != nil || len(rows) == 0 {
			continue
		}
		if px := parseFloat(rows[0].AvgPx); px > 0 {
			info.avgPx = px
			info.fee = parseFloat(rows[0].Fee)
			return info
		}
	}
	return info
}

// formatPrice trims a price to a compact string OKX accepts (max 1 decimal
// beyond what's given; triggers accept full precision strings).
func formatPrice(px float64) string {
	s := fmt.Sprintf("%.10f", px)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}
