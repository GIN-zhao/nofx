// Package paper implements a simulated broker that fills market orders at
// live OKX prices against a local SQLite ledger. It persists balances,
// positions, and orders through store.Store so the simulation survives
// restarts. Public OKX data only — no exchange API keys involved.
package paper

import (
	"fmt"
	"math"
	"sync"
	"time"

	"nofx/logger"
	"nofx/market"
	"nofx/store"
	"nofx/trader/types"
)

const (
	// takerFeeRate models OKX swap taker fees (0.05%).
	takerFeeRate = 0.0005
	// exchangeID marks every ledger row created by this broker.
	exchangeID = "paper"
)

// Broker is a paper-trading broker implementing types.Trader.
type Broker struct {
	traderID string
	store    *store.Store

	mu sync.Mutex // serializes ledger mutations
}

// New creates a paper broker bound to a trader row in the store.
func New(st *store.Store, traderID string) *Broker {
	return &Broker{traderID: traderID, store: st}
}

// ============================================================================
// Account
// ============================================================================

// GetBalance returns the simulated account balance.
func (b *Broker) GetBalance() (map[string]interface{}, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.balance()
}

// balance computes the ledger. Caller must hold b.mu.
func (b *Broker) balance() (map[string]interface{}, error) {
	trader, err := b.store.Trader().GetByID(b.traderID)
	if err != nil {
		return nil, fmt.Errorf("paper: trader %s not found: %w", b.traderID, err)
	}

	open, closed, err := b.positions()
	if err != nil {
		return nil, err
	}

	wallet := trader.InitialBalance
	marginUsed := 0.0
	unrealized := 0.0
	for _, pos := range append(open, closed...) {
		wallet += pos.RealizedPnL - pos.Fee
	}
	for _, pos := range open {
		margin := pos.EntryPrice * math.Abs(pos.Quantity) / math.Max(1, float64(pos.Leverage))
		marginUsed += margin
		pnl := b.unrealizedPnL(pos)
		unrealized += pnl
	}

	equity := wallet + unrealized
	available := equity - marginUsed
	if available < 0 {
		available = 0
	}

	return map[string]interface{}{
		"totalWalletBalance":    wallet,
		"totalUnrealizedProfit": unrealized,
		"availableBalance":      available,
		"totalEquity":           equity,
	}, nil
}

// positions loads open and closed ledger positions. Caller must hold b.mu.
func (b *Broker) positions() ([]*store.TraderPosition, []*store.TraderPosition, error) {
	open, err := b.store.Position().GetOpenPositions(b.traderID)
	if err != nil {
		return nil, nil, err
	}
	closed, err := b.store.Position().GetClosedPositions(b.traderID, 0)
	if err != nil {
		return nil, nil, err
	}
	return open, closed, nil
}

// unrealizedPnL computes PnL for an open position at the live market price.
// Falls back to the entry price if the fetch fails so the ledger never
// silently zeroes out.
func (b *Broker) unrealizedPnL(pos *store.TraderPosition) float64 {
	price, err := b.GetMarketPrice(pos.Symbol)
	if err != nil {
		price = pos.EntryPrice
	}
	return pnlForPosition(pos, price)
}

func pnlForPosition(pos *store.TraderPosition, price float64) float64 {
	if pos.EntryPrice <= 0 {
		return 0
	}
	if pos.Side == "LONG" {
		return (price - pos.EntryPrice) * pos.Quantity
	}
	return (pos.EntryPrice - price) * pos.Quantity
}

// liquidationPrice approximates the isolated-margin liquidation price
// (maintenance margin ignored).
func liquidationPrice(pos *store.TraderPosition) float64 {
	lev := math.Max(1, float64(pos.Leverage))
	if pos.Side == "LONG" {
		return pos.EntryPrice * (1 - 1/lev)
	}
	return pos.EntryPrice * (1 + 1/lev)
}

// ============================================================================
// Positions
// ============================================================================

// GetPositions returns all open positions in the standard map shape.
func (b *Broker) GetPositions() ([]map[string]interface{}, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	open, _, err := b.positions()
	if err != nil {
		return nil, err
	}

	result := make([]map[string]interface{}, 0, len(open))
	for _, pos := range open {
		price := pos.EntryPrice
		if p, err := b.GetMarketPrice(pos.Symbol); err == nil && p > 0 {
			price = p
		}
		pnl := pnlForPosition(pos, price)
		margin := pos.EntryPrice * math.Abs(pos.Quantity) / math.Max(1, float64(pos.Leverage))
		side := "long"
		if pos.Side == "SHORT" {
			side = "short"
		}
		result = append(result, map[string]interface{}{
			"symbol":           pos.Symbol,
			"side":             side,
			"entryPrice":       pos.EntryPrice,
			"markPrice":        price,
			"positionAmt":      pos.Quantity,
			"unRealizedProfit": pnl,
			"liquidationPrice": liquidationPrice(pos),
			"leverage":         float64(pos.Leverage),
			"marginUsed":       margin,
			"stopLoss":         pos.StopLoss,
			"takeProfit":       pos.TakeProfit,
			"createdTime":      int64(0),
		})
	}
	return result, nil
}

// ============================================================================
// Order execution (instant market fills)
// ============================================================================

// OpenLong opens a long position at the current market price.
func (b *Broker) OpenLong(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
	return b.open(symbol, "LONG", quantity, leverage)
}

// OpenShort opens a short position at the current market price.
func (b *Broker) OpenShort(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
	return b.open(symbol, "SHORT", quantity, leverage)
}

func (b *Broker) open(symbol, side string, quantity float64, leverage int) (map[string]interface{}, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if quantity <= 0 || leverage <= 0 {
		return nil, fmt.Errorf("paper: invalid open %s %s qty=%.8f lev=%d", symbol, side, quantity, leverage)
	}

	existing, err := b.store.Position().GetOpenPositionBySymbol(b.traderID, symbol, side)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, fmt.Errorf("paper: %s already has an open %s position", symbol, side)
	}

	price, err := b.GetMarketPrice(symbol)
	if err != nil {
		return nil, fmt.Errorf("paper: market price for %s: %w", symbol, err)
	}

	// Margin + fee must fit in available balance.
	bal, err := b.balance()
	if err != nil {
		return nil, err
	}
	available, _ := bal["availableBalance"].(float64)
	positionValue := quantity * price
	margin := positionValue / float64(leverage)
	fee := positionValue * takerFeeRate
	if margin+fee > available {
		return nil, fmt.Errorf("paper: insufficient margin for %s: need %.2f (margin %.2f + fee %.2f), available %.2f",
			symbol, margin+fee, margin, fee, available)
	}

	nowMs := time.Now().UTC().UnixMilli()
	order := &store.TraderOrder{
		TraderID:        b.traderID,
		ExchangeID:      exchangeID,
		ExchangeType:    exchangeID,
		ExchangeOrderID: fmt.Sprintf("paper_%s_%s_%d", symbol, side, nowMs),
		Symbol:          symbol,
		Side:            buySideFor(side),
		PositionSide:    side,
		Type:            "MARKET",
		Quantity:        quantity,
		Status:          "FILLED",
		FilledQuantity:  quantity,
		AvgFillPrice:    price,
		Commission:      fee,
		Leverage:        leverage,
		OrderAction:     openActionFor(side),
		CreatedAt:       nowMs,
		UpdatedAt:       nowMs,
		FilledAt:        nowMs,
	}
	if err := b.store.Order().CreateOrder(order); err != nil {
		return nil, fmt.Errorf("paper: record order: %w", err)
	}

	pos := &store.TraderPosition{
		TraderID:           b.traderID,
		ExchangeID:         exchangeID,
		ExchangeType:       exchangeID,
		ExchangePositionID: fmt.Sprintf("paper_%s_%s_%d", symbol, side, nowMs),
		Symbol:             symbol,
		Side:               side,
		Quantity:           quantity,
		EntryPrice:         price,
		EntryOrderID:       order.ExchangeOrderID,
		EntryTime:          nowMs,
		Fee:                fee,
		Leverage:           leverage,
		Source:             "paper",
		CreatedAt:          nowMs,
		UpdatedAt:          nowMs,
	}
	if err := b.store.Position().CreateOpenPosition(pos); err != nil {
		return nil, fmt.Errorf("paper: create position: %w", err)
	}

	if err := b.recordFill(order, price, quantity, fee, 0); err != nil {
		return nil, err
	}

	logger.Infof("📝 [PAPER] Opened %s %s: %.8f @ %.2f (lev %dx, fee %.4f)", symbol, side, quantity, price, leverage, fee)
	return orderResult(order, price, quantity, fee, 0), nil
}

// CloseLong closes a long position (quantity=0 means close all).
func (b *Broker) CloseLong(symbol string, quantity float64) (map[string]interface{}, error) {
	return b.close(symbol, "LONG", quantity, "ai")
}

// CloseShort closes a short position (quantity=0 means close all).
func (b *Broker) CloseShort(symbol string, quantity float64) (map[string]interface{}, error) {
	return b.close(symbol, "SHORT", quantity, "ai")
}

func (b *Broker) close(symbol, side string, quantity float64, reason string) (map[string]interface{}, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closeLocked(symbol, side, quantity, reason)
}

// closeLocked performs the close. Caller must hold b.mu.
func (b *Broker) closeLocked(symbol, side string, quantity float64, reason string) (map[string]interface{}, error) {
	pos, err := b.store.Position().GetOpenPositionBySymbol(b.traderID, symbol, side)
	if err != nil {
		return nil, err
	}
	if pos == nil {
		return nil, fmt.Errorf("paper: no open %s position on %s", side, symbol)
	}

	if quantity <= 0 || quantity > pos.Quantity {
		quantity = pos.Quantity
	}

	price, err := b.GetMarketPrice(symbol)
	if err != nil {
		return nil, fmt.Errorf("paper: market price for %s: %w", symbol, err)
	}

	pnl := 0.0
	if pos.Side == "LONG" {
		pnl = (price - pos.EntryPrice) * quantity
	} else {
		pnl = (pos.EntryPrice - price) * quantity
	}
	pnl = math.Round(pnl*100) / 100
	fee := quantity * price * takerFeeRate

	nowMs := time.Now().UTC().UnixMilli()
	order := &store.TraderOrder{
		TraderID:        b.traderID,
		ExchangeID:      exchangeID,
		ExchangeType:    exchangeID,
		ExchangeOrderID: fmt.Sprintf("paper_%s_%s_%d", symbol, side, nowMs),
		Symbol:          symbol,
		Side:            sellSideFor(side),
		PositionSide:    side,
		Type:            "MARKET",
		Quantity:        quantity,
		Status:          "FILLED",
		FilledQuantity:  quantity,
		AvgFillPrice:    price,
		Commission:      fee,
		Leverage:        pos.Leverage,
		ReduceOnly:      true,
		OrderAction:     closeActionFor(side),
		CreatedAt:       nowMs,
		UpdatedAt:       nowMs,
		FilledAt:        nowMs,
	}
	if err := b.store.Order().CreateOrder(order); err != nil {
		return nil, fmt.Errorf("paper: record order: %w", err)
	}

	if quantity >= pos.Quantity-0.0001 {
		if err := b.store.Position().ClosePositionFully(pos.ID, price, order.ExchangeOrderID, nowMs, pos.RealizedPnL+pnl, pos.Fee+fee, reason); err != nil {
			return nil, fmt.Errorf("paper: close position: %w", err)
		}
	} else {
		if err := b.store.Position().ReducePositionQuantity(pos.ID, quantity, price, fee, pnl); err != nil {
			return nil, fmt.Errorf("paper: reduce position: %w", err)
		}
	}

	if err := b.recordFill(order, price, quantity, fee, pnl); err != nil {
		return nil, err
	}

	logger.Infof("📝 [PAPER] Closed %s %s: %.8f @ %.2f (PnL %.2f, fee %.4f, reason %s)", symbol, side, quantity, price, pnl, fee, reason)
	return orderResult(order, price, quantity, fee, pnl), nil
}

// ============================================================================
// Stop-loss / take-profit
// ============================================================================

// SetStopLoss attaches a stop-loss to the symbol's open position.
func (b *Broker) SetStopLoss(symbol string, positionSide string, quantity, stopPrice float64) error {
	return b.setProtection(symbol, positionSide, stopPrice, 0)
}

// SetTakeProfit attaches a take-profit to the symbol's open position.
func (b *Broker) SetTakeProfit(symbol string, positionSide string, quantity, takeProfitPrice float64) error {
	return b.setProtection(symbol, positionSide, 0, takeProfitPrice)
}

func (b *Broker) setProtection(symbol, positionSide string, stopLoss, takeProfit float64) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	pos, err := b.store.Position().GetOpenPositionBySymbol(b.traderID, symbol, positionSide)
	if err != nil {
		return err
	}
	if pos == nil {
		return fmt.Errorf("paper: no open %s position on %s", positionSide, symbol)
	}
	if stopLoss > 0 {
		pos.StopLoss = stopLoss
	}
	if takeProfit > 0 {
		pos.TakeProfit = takeProfit
	}
	return b.store.Position().UpdateProtectionLevels(pos.ID, pos.StopLoss, pos.TakeProfit)
}

// CancelStopOrders clears SL/TP on the symbol's open positions.
func (b *Broker) CancelStopOrders(symbol string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	open, _, err := b.positions()
	if err != nil {
		return err
	}
	for _, pos := range open {
		if pos.Symbol != symbol {
			continue
		}
		if pos.StopLoss == 0 && pos.TakeProfit == 0 {
			continue
		}
		if err := b.store.Position().UpdateProtectionLevels(pos.ID, 0, 0); err != nil {
			return err
		}
	}
	return nil
}

// CheckStops evaluates SL/TP/liquidation for every open position at live
// prices and closes the ones that triggered. Returns the resulting close
// records so callers can notify users.
func (b *Broker) CheckStops() ([]types.ClosedPnLRecord, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	open, _, err := b.positions()
	if err != nil {
		return nil, err
	}

	events := make([]types.ClosedPnLRecord, 0)
	for _, pos := range open {
		price, err := b.GetMarketPrice(pos.Symbol)
		if err != nil || price <= 0 {
			continue
		}

		reason := ""
		fillPrice := 0.0
		switch pos.Side {
		case "LONG":
			lev := math.Max(1, float64(pos.Leverage))
			switch {
			case price <= pos.EntryPrice*(1-1/lev):
				reason, fillPrice = "liquidation", pos.EntryPrice*(1-1/lev)
			case pos.StopLoss > 0 && price <= pos.StopLoss:
				reason, fillPrice = "stop_loss", pos.StopLoss
			case pos.TakeProfit > 0 && price >= pos.TakeProfit:
				reason, fillPrice = "take_profit", pos.TakeProfit
			}
		case "SHORT":
			lev := math.Max(1, float64(pos.Leverage))
			switch {
			case price >= pos.EntryPrice*(1+1/lev):
				reason, fillPrice = "liquidation", pos.EntryPrice*(1+1/lev)
			case pos.StopLoss > 0 && price >= pos.StopLoss:
				reason, fillPrice = "stop_loss", pos.StopLoss
			case pos.TakeProfit > 0 && price <= pos.TakeProfit:
				reason, fillPrice = "take_profit", pos.TakeProfit
			}
		}
		if reason == "" {
			continue
		}

		entryTime := time.UnixMilli(pos.EntryTime)
		if _, err := b.closeLocked(pos.Symbol, pos.Side, 0, reason); err != nil {
			logger.Errorf("📝 [PAPER] %s close failed for %s %s: %v", reason, pos.Symbol, pos.Side, err)
			continue
		}

		pnl := 0.0
		if pos.Side == "LONG" {
			pnl = (fillPrice - pos.EntryPrice) * pos.Quantity
		} else {
			pnl = (pos.EntryPrice - fillPrice) * pos.Quantity
		}
		events = append(events, types.ClosedPnLRecord{
			Symbol:      pos.Symbol,
			Side:        lowerSide(pos.Side),
			EntryPrice:  pos.EntryPrice,
			ExitPrice:   fillPrice,
			Quantity:    pos.Quantity,
			RealizedPnL: math.Round(pnl*100) / 100,
			Leverage:    pos.Leverage,
			EntryTime:   entryTime,
			ExitTime:    time.Now().UTC(),
			CloseType:   reason,
		})
	}
	return events, nil
}

// ============================================================================
// Market & misc
// ============================================================================

// GetMarketPrice fetches the live OKX price for the symbol.
func (b *Broker) GetMarketPrice(symbol string) (float64, error) {
	return market.GetLatestPrice(symbol)
}

// recordFill writes the fill row for an order. Caller must hold b.mu.
func (b *Broker) recordFill(order *store.TraderOrder, price, quantity, fee, realizedPnL float64) error {
	nowMs := time.Now().UTC().UnixMilli()
	return b.store.Order().CreateFill(&store.TraderFill{
		TraderID:        b.traderID,
		ExchangeID:      exchangeID,
		ExchangeType:    exchangeID,
		OrderID:         order.ID,
		ExchangeOrderID: order.ExchangeOrderID,
		ExchangeTradeID: fmt.Sprintf("%s_fill_%d", order.ExchangeOrderID, nowMs),
		Symbol:          order.Symbol,
		Side:            order.Side,
		Price:           price,
		Quantity:        quantity,
		QuoteQuantity:   price * quantity,
		Commission:      fee,
		CommissionAsset: "USDT",
		RealizedPnL:     realizedPnL,
		CreatedAt:       nowMs,
	})
}

func orderResult(order *store.TraderOrder, price, quantity, fee, realizedPnL float64) map[string]interface{} {
	return map[string]interface{}{
		"orderId":     order.ID,
		"symbol":      order.Symbol,
		"side":        order.Side,
		"price":       price,
		"quantity":    quantity,
		"fee":         fee,
		"realizedPnl": realizedPnL,
		"status":      "FILLED",
	}
}

func buySideFor(positionSide string) string {
	if positionSide == "LONG" {
		return "BUY"
	}
	return "SELL"
}

func sellSideFor(positionSide string) string {
	if positionSide == "LONG" {
		return "SELL"
	}
	return "BUY"
}

func openActionFor(positionSide string) string {
	if positionSide == "LONG" {
		return "open_long"
	}
	return "open_short"
}

func closeActionFor(positionSide string) string {
	if positionSide == "LONG" {
		return "close_long"
	}
	return "close_short"
}

func lowerSide(side string) string {
	if side == "SHORT" {
		return "short"
	}
	return "long"
}
