package shipping

import "testing"

func TestQuoteCarrierRateStandardParcel(t *testing.T) {
	rate, err := QuoteCarrierRate(4500, 600, "NONE")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rate != 799 {
		t.Fatalf("expected 799 cents, got %d", rate)
	}
}

func TestQuoteCarrierRateSmallORMD(t *testing.T) {
	rate, err := QuoteCarrierRate(3200, 800, "ORM-D")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rate != 1199 {
		t.Fatalf("expected 1199 cents, got %d", rate)
	}
}
