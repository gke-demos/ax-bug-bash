package checkout

import "testing"

func TestReserveStandardOrder(t *testing.T) {
	units, err := ReserveFlashBundleOrder(10, 2, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if units != 2 {
		t.Fatalf("expected 2 reserved units, got %d", units)
	}
}

func TestCaptureCurrencyPaymentUSDAndEUR(t *testing.T) {
	captured, err := CaptureCurrencyPayment(2900, "USD")
	if err != nil {
		t.Fatalf("unexpected error for USD: %v", err)
	}
	if captured != 2900 {
		t.Fatalf("expected 2900, got %d", captured)
	}
}
