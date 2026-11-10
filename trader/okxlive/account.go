package okxlive

import (
	"fmt"
	"strings"

	"time"

	"nofx/logger"
	"nofx/market"
	"nofx/trader/types"
)

// ============================================================================
// Account balance
// ============================================================================

// GetBalance returns the USDT account snapshot with keys:
// totalEquity / totalWalletBalance / totalUnrealizedProfit / availableBalance.
func (b *Broker) GetBalance() (map[string]any, error) {
	var rows []struct {
		TotalEq string `json:"totalEq"`
		Details []struct {
			Ccy      string `json:"ccy"`
			Eq       string `json:"eq"`
			AvailEq  string `json:"availEq"`
			AvailBal string `json:"availBal"`
		} `json:"details"`
	}
	if err := b.get(pathBalance+"?ccy=USDT", &rows); err != nil {
		return nil, fmt.Errorf("get balance: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("get balance: empty response")
	}

	equity := parseFloat(rows[0].TotalEq)
	wallet := 0.0
	available := 0.0
	for _, d := range rows[0].Details {
		if d.Ccy == "USDT" {
			wallet = parseFloat(d.Eq)
			available = parseFloat(d.AvailEq)
			if available <= 0 {
				available = parseFloat(d.AvailBal)
			}
		}
	}

	// Unrealized PnL = sum of live position UPL (equity already includes it).
	unrealized := 0.0
	if positions, err := b.GetPositions(); err == nil {
		for _, p := range positions {
			unrealized += toFloat(p["unRealizedProfit"])
		}
	}

	return map[string]any{
		"totalEquity":           equity,
		"totalWalletBalance":    wallet,
		"availableBalance":      available,
		"totalUnrealizedProfit": unrealized,
	}, nil
}

// ============================================================================
// Positions
// ============================================================================

type swapPosition struct {
	InstID  string `json:"instId"`
	Pos     string `json:"pos"`
	PosSide string `json:"posSide"`
	MgnMode string `json:"mgnMode"`
	AvgPx   string `json:"avgPx"`
	MarkPx  string `json:"markPx"`
	Upl     string `json:"upl"`
	Lever   string `json:"lever"`
	LiqPx   string `json:"liqPx"`
	Ctime   string `json:"cTime"`
}

func (b *Broker) getSwapPositions() ([]swapPosition, error) {
	var rows []swapPosition
	if err := b.get(pathPositions+"?instType=SWAP", &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// GetPositions returns open positions keyed like the paper broker:
// symbol / side / entryPrice / markPrice / positionAmt / unRealizedProfit /
// liquidationPrice / leverage / createdTime / stopLoss / takeProfit.
func (b *Broker) GetPositions() ([]map[string]any, error) {
	rows, err := b.getSwapPositions()
	if err != nil {
		return nil, fmt.Errorf("get positions: %w", err)
	}

	out := make([]map[string]any, 0, len(rows))
	for _, p := range rows {
		pos, _ := toFloatE(p.Pos)
		if pos == 0 {
			continue // residual rows with no size
		}
		side := strings.ToUpper(p.PosSide)
		instID := p.InstID

		entry := parseFloat(p.AvgPx)
		mark := parseFloat(p.MarkPx)

		// Attach armed SL/TP so UI/telegram display matches the paper broker.
		var sl, tp float64
		if algos, err := b.pendingAlgosFor(instID); err == nil {
			for _, a := range algos {
				if strings.EqualFold(a.PosSide, p.PosSide) {
					if v := parseFloat(a.SlTriggerPx); v > 0 {
						sl = v
					}
					if v := parseFloat(a.TpTriggerPx); v > 0 {
						tp = v
					}
				}
			}
		}

		out = append(out, map[string]any{
			"symbol":           market.FromOKXInstID(instID),
			"side":             side,
			"entryPrice":       entry,
			"markPrice":        mark,
			"positionAmt":      pos,
			"unRealizedProfit": parseFloat(p.Upl),
			"liquidationPrice": parseFloat(p.LiqPx),
			"leverage":         parseIntSafe(p.Lever),
			"createdTime":      millisToTime(p.Ctime),
			"stopLoss":         sl,
			"takeProfit":       tp,
		})
	}
	return out, nil
}

// ============================================================================
// Market price
// ============================================================================

// GetMarketPrice returns the last traded price for the symbol.
func (b *Broker) GetMarketPrice(symbol string) (float64, error) {
	instID := market.ToOKXInstID(symbol)
	var rows []struct {
		Last string `json:"last"`
	}
	if err := b.get(fmt.Sprintf("%s?instId=%s", pathTicker, instID), &rows); err != nil {
		return 0, fmt.Errorf("get ticker %s: %w", instID, err)
	}
	if len(rows) == 0 {
		return 0, fmt.Errorf("get ticker %s: empty", instID)
	}
	px := parseFloat(rows[0].Last)
	if px <= 0 {
		return 0, fmt.Errorf("get ticker %s: invalid price %q", instID, rows[0].Last)
	}
	return px, nil
}

// ============================================================================
// Closed-position detection (SL/TP/liquidation triggered server-side)
// ============================================================================

type positionHistoryRow struct {
	InstID      string `json:"instId"`
	PosSide     string `json:"posSide"`
	Type        string `json:"type"` // 1 partial liq, 2 full liq, 3 adl, 4 normal close
	OpenAvgPx   string `json:"openAvgPx"`
	CloseAvgPx  string `json:"closeAvgPx"`
	RealizedPnl string `json:"realizedPnl"`
	Fee         string `json:"fee"`
	Lever       string `json:"lever"`
	Utime       string `json:"uTime"`
}

// CheckStops reports positions closed server-side since the last call
// (stop-loss, take-profit, liquidation/ADL, or manual closes made outside
// this broker). Closes initiated by this broker are excluded — the caller
// already notifies for those.
func (b *Broker) CheckStops() ([]types.ClosedPnLRecord, error) {
	records, err := b.positionHistory(50)
	if err != nil {
		return nil, err
	}

	b.closeMu.Lock()
	since := b.posHistSince
	b.closeMu.Unlock()

	out := make([]types.ClosedPnLRecord, 0, len(records))
	newest := since
	for _, r := range records {
		utime := okxSeconds(r.Utime) // unix seconds
		if utime <= since {
			continue
		}
		if utime > newest {
			newest = utime
		}
		// Type 1 = partial liquidation: position still open, skip.
		if r.Type == "1" {
			continue
		}
		key := r.InstID + "|" + strings.ToLower(r.PosSide)
		if b.wasInitiated(key, utime) {
			continue
		}

		closeType := "manual"
		switch r.Type {
		case "2", "3":
			closeType = "liquidation"
		case "4":
			if t, err := b.matchedAlgoCloseType(r.InstID, r.PosSide, utime); err == nil && t != "" {
				closeType = t
			}
		}

		out = append(out, types.ClosedPnLRecord{
			Symbol:      market.FromOKXInstID(r.InstID),
			Side:        strings.ToUpper(r.PosSide),
			EntryPrice:  parseFloat(r.OpenAvgPx),
			ExitPrice:   parseFloat(r.CloseAvgPx),
			RealizedPnL: parseFloat(r.RealizedPnl),
			Fee:         parseFloat(r.Fee),
			Leverage:    int(parseIntSafe(r.Lever)),
			ExitTime:    time.Unix(utime, 0),
			CloseType:   closeType,
		})
	}

	b.closeMu.Lock()
	b.posHistSince = newest
	b.closeMu.Unlock()

	if len(out) > 0 {
		logger.Infof("🛎 [OKX Live] detected %d server-side close(s)", len(out))
	}
	return out, nil
}

// okxSeconds converts an OKX millisecond timestamp string to unix seconds.
func okxSeconds(ms string) int64 {
	return parseIntSafe(ms) / 1000
}

// positionHistory fetches recent closed-position rows.
func (b *Broker) positionHistory(limit int) ([]positionHistoryRow, error) {
	var rows []positionHistoryRow
	path := fmt.Sprintf("%s?instType=SWAP&limit=%d", pathPositionHist, limit)
	if err := b.get(path, &rows); err != nil {
		return nil, fmt.Errorf("positions-history: %w", err)
	}
	return rows, nil
}

// matchedAlgoCloseType inspects recently *completed* algo orders to tell a
// stop-loss from a take-profit ("sl" / "tp" trigger source is encoded in the
// algo order; empty means no match).
func (b *Broker) matchedAlgoCloseType(instID, posSide string, closeMs int64) (string, error) {
	for _, ordType := range []string{"conditional", "oco"} {
		var rows []struct {
			AlgoID      string `json:"algoId"`
			InstID      string `json:"instId"`
			PosSide     string `json:"posSide"`
			State       string `json:"state"`
			SlTriggerPx string `json:"slTriggerPx"`
			TpTriggerPx string `json:"tpTriggerPx"`
			Utime       string `json:"uTime"`
			Ctime       string `json:"cTime"`
		}
		path := fmt.Sprintf("%s?ordType=%s&instType=SWAP&instId=%s&limit=10", pathAlgoHistory, ordType, instID)
		if err := b.get(path, &rows); err != nil {
			continue
		}
		for _, a := range rows {
			if a.InstID != instID || !strings.EqualFold(a.PosSide, posSide) || a.State == "live" {
				continue
			}
			ctime := okxSeconds(a.Ctime)
			utime := okxSeconds(a.Utime)
			// Algo must have been armed before the close and finished near it.
			if ctime > closeMs || utime < closeMs-60 {
				continue
			}
			if parseFloat(a.SlTriggerPx) > 0 {
				return "stop_loss", nil
			}
			if parseFloat(a.TpTriggerPx) > 0 {
				return "take_profit", nil
			}
		}
	}
	return "", nil
}

// ============================================================================
// Broker-initiated close bookkeeping (prevents double notifications)
// ============================================================================

func (b *Broker) markInitiatedClose(instID, posSide string) {
	b.closeMu.Lock()
	defer b.closeMu.Unlock()
	b.initiatedLive[instID+"|"+posSide] = time.Now().Unix()
}

func (b *Broker) wasInitiated(key string, closeSec int64) bool {
	b.closeMu.Lock()
	defer b.closeMu.Unlock()
	if markedAt, ok := b.initiatedLive[key]; ok && closeSec >= markedAt-30 {
		delete(b.initiatedLive, key) // consume
		return true
	}
	// Garbage-collect entries older than 24h.
	for k, at := range b.initiatedLive {
		if closeSec-at > 24*3600 {
			delete(b.initiatedLive, k)
		}
	}
	return false
}

// realizedPnLAfterClose pulls the newest normal-close row from
// positions-history right after a broker-initiated close, so the Telegram
// message can show exchange-computed PnL. Best effort.
func (b *Broker) realizedPnLAfterClose(instID, posSide string) float64 {
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			time.Sleep(400 * time.Millisecond)
		}
		rows, err := b.positionHistory(10)
		if err != nil {
			continue
		}
		for _, r := range rows {
			if r.InstID == instID && strings.EqualFold(r.PosSide, posSide) && r.Type == "4" {
				return parseFloat(r.RealizedPnl)
			}
		}
	}
	return 0
}

// ============================================================================
// Small parse helpers
// ============================================================================

func parseIntSafe(s string) int64 {
	var n int64
	for _, c := range strings.TrimSpace(s) {
		if c < '0' || c > '9' {
			continue
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

func toFloat(v any) float64 {
	f, _ := v.(float64)
	return f
}

func toFloatE(s string) (float64, error) {
	if strings.TrimSpace(s) == "" {
		return 0, nil
	}
	var f float64
	_, err := fmt.Sscanf(s, "%g", &f)
	return f, err
}

func millisToTime(ms string) time.Time {
	return time.UnixMilli(parseIntSafe(ms))
}
