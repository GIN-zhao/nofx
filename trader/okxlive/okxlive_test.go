package okxlive

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

// ----------------------------------------------------------------------------
// signing
// ----------------------------------------------------------------------------

func TestSignMatchesManualHMAC(t *testing.T) {
	b := &Broker{secretKey: "secret"}
	got := b.sign("2026-01-02T03:04:05.000Z", "GET", "/api/v5/account/balance?ccy=USDT", "")

	h := hmac.New(sha256.New, []byte("secret"))
	h.Write([]byte("2026-01-02T03:04:05.000ZGET/api/v5/account/balance?ccy=USDT"))
	want := base64.StdEncoding.EncodeToString(h.Sum(nil))
	if got != want {
		t.Fatalf("sign = %q, want %q", got, want)
	}
}

func TestSignChangesWithInput(t *testing.T) {
	b := &Broker{secretKey: "secret"}
	a := b.sign("t1", "GET", "/a", "")
	bb := b.sign("t2", "GET", "/a", "")
	c := b.sign("t1", "POST", "/a", "")
	if a == bb || a == c {
		t.Fatal("signature should change when timestamp/method changes")
	}
}

// ----------------------------------------------------------------------------
// contract sizing
// ----------------------------------------------------------------------------

func TestContractsForQuantity(t *testing.T) {
	inst := &instrument{CtVal: 0.01, LotSz: 1, MinSz: 1}

	// 0.123 BTC / 0.01 ctVal = 12.3 contracts -> floor to 12
	got, err := contractsForQuantity(0.123, inst)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 12 {
		t.Fatalf("contracts = %v, want 12", got)
	}

	// Below minimum (0.005 BTC -> 0.5 contracts -> floored 0 < minSz 1)
	if _, err := contractsForQuantity(0.005, inst); err == nil {
		t.Fatal("expected error for quantity below minSz")
	}
}

func TestContractsForQuantityFractionalLot(t *testing.T) {
	inst := &instrument{CtVal: 0.1, LotSz: 0.1, MinSz: 0.1}
	got, err := contractsForQuantity(1.0, inst) // 10 contracts exactly
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 10 {
		t.Fatalf("contracts = %v, want 10", got)
	}
}

func TestFormatContracts(t *testing.T) {
	cases := []struct {
		lotSz, contracts float64
		want             string
	}{
		{1, 12, "12"},
		{0.1, 3.5, "3.5"},
		{0.01, 1.23, "1.23"},
	}
	for _, c := range cases {
		inst := &instrument{LotSz: c.lotSz}
		if got := formatContracts(c.contracts, inst); got != c.want {
			t.Errorf("formatContracts(%v, lotSz=%v) = %q, want %q", c.contracts, c.lotSz, got, c.want)
		}
	}
}

// ----------------------------------------------------------------------------
// parsing helpers
// ----------------------------------------------------------------------------

func TestParseFloatAndInt(t *testing.T) {
	if got := parseFloat("12345.678"); got != 12345.678 {
		t.Errorf("parseFloat = %v", got)
	}
	if got := parseIntSafe("10"); got != 10 {
		t.Errorf("parseIntSafe(\"10\") = %v", got)
	}
	if got := parseIntSafe(""); got != 0 {
		t.Errorf("parseIntSafe(\"\") = %v", got)
	}
}

func TestOkxSeconds(t *testing.T) {
	// 1700000000000 ms -> 1700000000 s
	if got := okxSeconds("1700000000000"); got != 1700000000 {
		t.Errorf("okxSeconds = %v", got)
	}
	if got := okxSeconds(""); got != 0 {
		t.Errorf("okxSeconds(\"\") = %v", got)
	}
}

func TestNormalizePosSide(t *testing.T) {
	if normalizePosSide("SHORT") != "short" || normalizePosSide(" short ") != "short" {
		t.Error("short normalization failed")
	}
	if normalizePosSide("long") != "long" || normalizePosSide("") != "long" || normalizePosSide("bogus") != "long" {
		t.Error("long default normalization failed")
	}
}

func TestFormatPrice(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{78000, "78000"},
		{78123.45, "78123.45"},
		{0.5, "0.5"},
	}
	for _, c := range cases {
		if got := formatPrice(c.in); got != c.want {
			t.Errorf("formatPrice(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMarginMode(t *testing.T) {
	if (&Broker{crossMargin: true}).marginMode() != "cross" {
		t.Error("expected cross")
	}
	if (&Broker{crossMargin: false}).marginMode() != "isolated" {
		t.Error("expected isolated")
	}
}
