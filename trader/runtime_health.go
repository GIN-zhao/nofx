package trader

// Runtime health state written by the run-loop goroutine and read by
// GetStatus. Everything goes through runtimeHealthMu so status polling never
// races the trading loop.

func (at *AutoTrader) safeModeState() (bool, string) {
	at.runtimeHealthMu.RLock()
	defer at.runtimeHealthMu.RUnlock()
	return at.safeMode, at.safeModeReason
}
