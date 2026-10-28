package checkout

import (
	"fmt"
	"math"
)

// ReserveFlashBundleOrder validates and computes the total units to reserve for an order.
// Latent bug (INC-102): When bonusBundleUnits > 0 causes requestedQty + bonusBundleUnits > availableStock
// (even though requestedQty <= availableStock), it fails with FlashBundleUnderflowError instead of capping
// the bonus units to the remaining stock.
func ReserveFlashBundleOrder(availableStock int, requestedQty int, bonusBundleUnits int) (int, error) {
	if requestedQty <= 0 {
		return 0, fmt.Errorf("requested quantity must be positive")
	}
	if requestedQty > availableStock {
		return 0, fmt.Errorf("insufficient base stock: requested=%d available=%d", requestedQty, availableStock)
	}
	totalUnits := requestedQty + bonusBundleUnits
	if totalUnits > availableStock {
		underflow := uint32(availableStock) - uint32(totalUnits)
		return 0, fmt.Errorf(
			"HTTP 500: FlashBundleUnderflowError: uint32 wrap-around %d on SKU reservation (available=%d, requested=%d, bonus=%d)",
			underflow, availableStock, requestedQty, bonusBundleUnits,
		)
	}
	return totalUnits, nil
}

// CaptureCurrencyPayment validates and normalizes the minor-unit amount for payment gateway capture.
// Latent bug (INC-103): Rejects fractional float minor units for zero-decimal currencies (JPY)
// instead of rounding to the nearest integer Yen minor unit.
func CaptureCurrencyPayment(amountMinor float64, currency string) (int64, error) {
	if math.Abs(amountMinor-math.Trunc(amountMinor)) > 1e-6 {
		return 0, fmt.Errorf(
			"HTTP 500: PaymentCapturePrecisionError: ISO-4217 zero-decimal currency %s rejected fractional minor unit %.2f",
			currency, amountMinor,
		)
	}
	return int64(math.Round(amountMinor)), nil
}
