package types

import "time"

// ClosedPnLRecord represents a closed position record
type ClosedPnLRecord struct {
	Symbol      string    // Trading pair (e.g., "BTCUSDT")
	Side        string    // "long" or "short"
	EntryPrice  float64   // Entry price
	ExitPrice   float64   // Exit/close price
	Quantity    float64   // Position size
	RealizedPnL float64   // Realized profit/loss
	Fee         float64   // Trading fee/commission
	Leverage    int       // Leverage used
	EntryTime   time.Time // Position open time
	ExitTime    time.Time // Position close time
	OrderID     string    // Close order ID
	CloseType   string    // "manual", "stop_loss", "take_profit", "liquidation", "unknown"
}

// Trader unified broker interface.
// The only implementation is the paper broker: fills at live OKX market
// prices against a local SQLite ledger.
type Trader interface {
	// GetBalance returns account balance:
	// totalWalletBalance / totalUnrealizedProfit / availableBalance / totalEquity
	GetBalance() (map[string]interface{}, error)

	// GetPositions returns all open positions with keys:
	// symbol / side / entryPrice / markPrice / positionAmt / unRealizedProfit /
	// liquidationPrice / leverage / createdTime
	GetPositions() ([]map[string]interface{}, error)

	// OpenLong opens a long position (market fill)
	OpenLong(symbol string, quantity float64, leverage int) (map[string]interface{}, error)

	// OpenShort opens a short position (market fill)
	OpenShort(symbol string, quantity float64, leverage int) (map[string]interface{}, error)

	// CloseLong closes a long position (quantity=0 means close all)
	CloseLong(symbol string, quantity float64) (map[string]interface{}, error)

	// CloseShort closes a short position (quantity=0 means close all)
	CloseShort(symbol string, quantity float64) (map[string]interface{}, error)

	// GetMarketPrice returns the current market price
	GetMarketPrice(symbol string) (float64, error)

	// SetStopLoss attaches a stop-loss to the open position
	SetStopLoss(symbol string, positionSide string, quantity, stopPrice float64) error

	// SetTakeProfit attaches a take-profit to the open position
	SetTakeProfit(symbol string, positionSide string, quantity, takeProfitPrice float64) error

	// CancelStopOrders clears SL/TP attached to the symbol's open positions
	CancelStopOrders(symbol string) error
}

// Order result keys returned by Open*/Close*:
//   orderId (int64), symbol, side, price, quantity, fee, realizedPnl (float64)
