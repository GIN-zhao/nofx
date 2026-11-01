package market

import (
	"math"
	"time"
)

// calculateEMA calculates EMA
func calculateEMA(klines []Kline, period int) float64 {
	if len(klines) < period {
		return 0
	}

	// Calculate SMA as initial EMA
	sum := 0.0
	for i := 0; i < period; i++ {
		sum += klines[i].Close
	}
	ema := sum / float64(period)

	// Calculate EMA
	multiplier := 2.0 / float64(period+1)
	for i := period; i < len(klines); i++ {
		ema = (klines[i].Close-ema)*multiplier + ema
	}

	return ema
}

// calculateMACD calculates MACD
func calculateMACD(klines []Kline) float64 {
	if len(klines) < 26 {
		return 0
	}

	// Calculate 12-period and 26-period EMA
	ema12 := calculateEMA(klines, 12)
	ema26 := calculateEMA(klines, 26)

	// MACD = EMA12 - EMA26
	return ema12 - ema26
}

// calculateRSI calculates RSI
func calculateRSI(klines []Kline, period int) float64 {
	if len(klines) <= period {
		return 0
	}

	gains := 0.0
	losses := 0.0

	// Calculate initial average gain/loss
	for i := 1; i <= period; i++ {
		change := klines[i].Close - klines[i-1].Close
		if change > 0 {
			gains += change
		} else {
			losses += -change
		}
	}

	avgGain := gains / float64(period)
	avgLoss := losses / float64(period)

	// Use Wilder smoothing method to calculate subsequent RSI
	for i := period + 1; i < len(klines); i++ {
		change := klines[i].Close - klines[i-1].Close
		if change > 0 {
			avgGain = (avgGain*float64(period-1) + change) / float64(period)
			avgLoss = (avgLoss * float64(period-1)) / float64(period)
		} else {
			avgGain = (avgGain * float64(period-1)) / float64(period)
			avgLoss = (avgLoss*float64(period-1) + (-change)) / float64(period)
		}
	}

	if avgLoss == 0 {
		return 100
	}

	rs := avgGain / avgLoss
	rsi := 100 - (100 / (1 + rs))

	return rsi
}

// calculateATR calculates ATR
func calculateATR(klines []Kline, period int) float64 {
	if len(klines) <= period {
		return 0
	}

	trs := make([]float64, len(klines))
	for i := 1; i < len(klines); i++ {
		high := klines[i].High
		low := klines[i].Low
		prevClose := klines[i-1].Close

		tr1 := high - low
		tr2 := math.Abs(high - prevClose)
		tr3 := math.Abs(low - prevClose)

		trs[i] = math.Max(tr1, math.Max(tr2, tr3))
	}

	// Calculate initial ATR
	sum := 0.0
	for i := 1; i <= period; i++ {
		sum += trs[i]
	}
	atr := sum / float64(period)

	// Wilder smoothing
	for i := period + 1; i < len(klines); i++ {
		atr = (atr*float64(period-1) + trs[i]) / float64(period)
	}

	return atr
}

// calculateBOLL calculates Bollinger Bands (upper, middle, lower)
// period: typically 20, multiplier: typically 2
func calculateBOLL(klines []Kline, period int, multiplier float64) (upper, middle, lower float64) {
	if len(klines) < period {
		return 0, 0, 0
	}

	// Calculate SMA (middle band)
	sum := 0.0
	for i := len(klines) - period; i < len(klines); i++ {
		sum += klines[i].Close
	}
	sma := sum / float64(period)

	// Calculate standard deviation
	variance := 0.0
	for i := len(klines) - period; i < len(klines); i++ {
		diff := klines[i].Close - sma
		variance += diff * diff
	}
	stdDev := math.Sqrt(variance / float64(period))

	// Calculate bands
	middle = sma
	upper = sma + multiplier*stdDev
	lower = sma - multiplier*stdDev

	return upper, middle, lower
}

// calculateDonchian calculates Donchian channel (highest high, lowest low) for given period
func calculateDonchian(klines []Kline, period int) (upper, lower float64) {
	if len(klines) == 0 || period <= 0 {
		return 0, 0
	}

	// Use all available klines if period > len(klines)
	start := len(klines) - period
	if start < 0 {
		start = 0
	}

	upper = klines[start].High
	lower = klines[start].Low

	for i := start + 1; i < len(klines); i++ {
		if klines[i].High > upper {
			upper = klines[i].High
		}
		if klines[i].Low < lower {
			lower = klines[i].Low
		}
	}

	return upper, lower
}

// Box period constants (in 1h candles)
const (
	ShortBoxPeriod = 72  // 3 days of 1h candles
	MidBoxPeriod   = 240 // 10 days of 1h candles
	LongBoxPeriod  = 500 // ~21 days of 1h candles
)

// calculateBoxData calculates multi-period box data from klines
func calculateBoxData(klines []Kline, currentPrice float64) *BoxData {
	box := &BoxData{
		CurrentPrice: currentPrice,
	}

	if len(klines) == 0 {
		return box
	}

	box.ShortUpper, box.ShortLower = calculateDonchian(klines, ShortBoxPeriod)
	box.MidUpper, box.MidLower = calculateDonchian(klines, MidBoxPeriod)
	box.LongUpper, box.LongLower = calculateDonchian(klines, LongBoxPeriod)

	return box
}

// emaSeries computes the EMA value at every bar (seeded by SMA at index period-1).
// The returned slice covers klines[period-1:] — index 0 corresponds to bar period-1.
func emaSeries(klines []Kline, period int) []float64 {
	if len(klines) < period || period <= 0 {
		return nil
	}
	sum := 0.0
	for i := 0; i < period; i++ {
		sum += klines[i].Close
	}
	ema := sum / float64(period)
	out := make([]float64, 0, len(klines)-period+1)
	out = append(out, ema)
	multiplier := 2.0 / float64(period+1)
	for i := period; i < len(klines); i++ {
		ema = (klines[i].Close-ema)*multiplier + ema
		out = append(out, ema)
	}
	return out
}

// rsiSeries computes the Wilder RSI at every bar from index period onward.
// The returned slice covers klines[period:] — index 0 corresponds to bar period.
func rsiSeries(klines []Kline, period int) []float64 {
	if len(klines) <= period || period <= 0 {
		return nil
	}
	gains, losses := 0.0, 0.0
	for i := 1; i <= period; i++ {
		change := klines[i].Close - klines[i-1].Close
		if change > 0 {
			gains += change
		} else {
			losses += -change
		}
	}
	avgGain := gains / float64(period)
	avgLoss := losses / float64(period)
	rsiValue := func(gain, loss float64) float64 {
		if loss == 0 {
			return 100
		}
		return 100 - 100/(1+gain/loss)
	}
	out := make([]float64, 0, len(klines)-period)
	out = append(out, rsiValue(avgGain, avgLoss))
	for i := period + 1; i < len(klines); i++ {
		change := klines[i].Close - klines[i-1].Close
		if change > 0 {
			avgGain = (avgGain*float64(period-1) + change) / float64(period)
			avgLoss = (avgLoss * float64(period-1)) / float64(period)
		} else {
			avgGain = (avgGain * float64(period-1)) / float64(period)
			avgLoss = (avgLoss*float64(period-1) + (-change)) / float64(period)
		}
		out = append(out, rsiValue(avgGain, avgLoss))
	}
	return out
}

// macdSeries computes the MACD (EMA12-EMA26) at every bar where both EMAs exist.
func macdSeries(klines []Kline) []float64 {
	ema12 := emaSeries(klines, 12)
	ema26 := emaSeries(klines, 26)
	if ema12 == nil || ema26 == nil {
		return nil
	}
	// ema26 is shorter (starts at bar 25); align to its length from the tail.
	offset := len(ema12) - len(ema26)
	if offset < 0 {
		return nil
	}
	out := make([]float64, 0, len(ema26))
	for i := 0; i < len(ema26); i++ {
		out = append(out, ema12[offset+i]-ema26[i])
	}
	return out
}

// calculateIntradaySeries builds the 3-minute intraday indicator series.
func calculateIntradaySeries(klines []Kline) *IntradayData {
	if len(klines) == 0 {
		return nil
	}
	data := &IntradayData{ATR14: calculateATR(klines, 14)}
	for _, k := range klines {
		data.MidPrices = append(data.MidPrices, k.Close)
		data.Volume = append(data.Volume, k.Volume)
	}
	ema20 := emaSeries(klines, 20)
	if ema20 == nil {
		ema20 = []float64{calculateEMA(klines, 20)}
	}
	data.EMA20Values = ema20
	data.MACDValues = macdSeries(klines)
	data.RSI7Values = rsiSeries(klines, 7)
	data.RSI14Values = rsiSeries(klines, 14)
	return data
}

// calculateLongerTermData builds the 4-hour longer-term context.
func calculateLongerTermData(klines []Kline) *LongerTermData {
	if len(klines) == 0 {
		return nil
	}
	data := &LongerTermData{
		EMA20: calculateEMA(klines, 20),
		EMA50: calculateEMA(klines, 50),
		ATR3:  calculateATR(klines, 3),
		ATR14: calculateATR(klines, 14),
	}
	last := 40
	if len(klines) < last {
		last = len(klines)
	}
	volumeSum := 0.0
	for _, k := range klines {
		volumeSum += k.Volume
	}
	data.CurrentVolume = klines[len(klines)-1].Volume
	data.AverageVolume = volumeSum / float64(len(klines))

	if s := macdSeries(klines); len(s) > 0 {
		if len(s) > last {
			s = s[len(s)-last:]
		}
		data.MACDValues = s
	}
	if s := rsiSeries(klines, 14); len(s) > 0 {
		if len(s) > last {
			s = s[len(s)-last:]
		}
		data.RSI14Values = s
	}
	return data
}

// calculateTimeframeSeries builds per-timeframe series data, trimmed to the
// most recent `count` bars.
func calculateTimeframeSeries(klines []Kline, tf string, count int) *TimeframeSeriesData {
	if len(klines) == 0 {
		return nil
	}
	if count <= 0 || count > len(klines) {
		count = len(klines)
	}

	data := &TimeframeSeriesData{
		Timeframe: tf,
		ATR14:     calculateATR(klines, 14),
	}
	ema20 := emaSeries(klines, 20)
	ema50 := emaSeries(klines, 50)
	macd := macdSeries(klines)
	rsi7 := rsiSeries(klines, 7)
	rsi14 := rsiSeries(klines, 14)

	// All series are tail-aligned to the kline list; trim each to `count` bars.
	trim := func(s []float64) []float64 {
		if len(s) > count {
			return s[len(s)-count:]
		}
		return s
	}
	data.Klines = make([]KlineBar, 0, count)
	for i := len(klines) - count; i < len(klines); i++ {
		k := klines[i]
		data.Klines = append(data.Klines, KlineBar{
			Time: k.OpenTime, Open: k.Open, High: k.High, Low: k.Low,
			Close: k.Close, Volume: k.Volume,
		})
		data.MidPrices = append(data.MidPrices, k.Close)
		data.Volume = append(data.Volume, k.Volume)
	}
	data.EMA20Values = trim(ema20)
	data.EMA50Values = trim(ema50)
	data.MACDValues = trim(macd)
	data.RSI7Values = trim(rsi7)
	data.RSI14Values = trim(rsi14)

	// Bollinger series (20, 2) computed over a rolling window.
	for i := 19; i < len(klines); i++ {
		upper, middle, lower := calculateBOLL(klines[:i+1], 20, 2)
		data.BOLLUpper = append(data.BOLLUpper, upper)
		data.BOLLMiddle = append(data.BOLLMiddle, middle)
		data.BOLLLower = append(data.BOLLLower, lower)
	}
	data.BOLLUpper = trim(data.BOLLUpper)
	data.BOLLMiddle = trim(data.BOLLMiddle)
	data.BOLLLower = trim(data.BOLLLower)

	return data
}

// timeframeMinutes maps a timeframe label to its duration in minutes.
func timeframeMinutes(tf string) int {
	switch tf {
	case "1m":
		return 1
	case "3m":
		return 3
	case "5m":
		return 5
	case "15m":
		return 15
	case "30m":
		return 30
	case "1h":
		return 60
	case "2h":
		return 120
	case "4h":
		return 240
	case "6h":
		return 360
	case "8h":
		return 480
	case "12h":
		return 720
	case "1d":
		return 1440
	case "3d":
		return 4320
	case "1w":
		return 10080
	default:
		return 0
	}
}

// calculatePriceChangeByBars returns the price change percentage over the
// window that `minutes` spans, measured in whole bars of the given timeframe.
func calculatePriceChangeByBars(klines []Kline, timeframe string, minutes int) float64 {
	if len(klines) < 2 {
		return 0
	}
	tfMin := timeframeMinutes(timeframe)
	if tfMin <= 0 {
		return priceChangeFromSeries(klines, time.Duration(minutes)*time.Minute)
	}
	bars := minutes / tfMin
	if bars <= 0 {
		bars = 1
	}
	if len(klines) <= bars {
		return priceChangeFromSeries(klines, time.Duration(minutes)*time.Minute)
	}
	base := klines[len(klines)-1-bars].Close
	last := klines[len(klines)-1].Close
	if base <= 0 {
		return 0
	}
	return ((last - base) / base) * 100
}

// ========== Exported indicator calculation functions (for testing) ==========

// ExportCalculateEMA exports calculateEMA for testing
func ExportCalculateEMA(klines []Kline, period int) float64 {
	return calculateEMA(klines, period)
}

// ExportCalculateMACD exports calculateMACD for testing
func ExportCalculateMACD(klines []Kline) float64 {
	return calculateMACD(klines)
}

// ExportCalculateRSI exports calculateRSI for testing
func ExportCalculateRSI(klines []Kline, period int) float64 {
	return calculateRSI(klines, period)
}

// ExportCalculateATR exports calculateATR for testing
func ExportCalculateATR(klines []Kline, period int) float64 {
	return calculateATR(klines, period)
}

// ExportCalculateBOLL exports calculateBOLL for testing
func ExportCalculateBOLL(klines []Kline, period int, multiplier float64) (upper, middle, lower float64) {
	return calculateBOLL(klines, period, multiplier)
}

// ExportCalculateDonchian exports calculateDonchian for testing
func ExportCalculateDonchian(klines []Kline, period int) (float64, float64) {
	return calculateDonchian(klines, period)
}

// ExportCalculateBoxData exports calculateBoxData for testing
func ExportCalculateBoxData(klines []Kline, currentPrice float64) *BoxData {
	return calculateBoxData(klines, currentPrice)
}
