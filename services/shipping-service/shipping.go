package shipping

import "fmt"

// QuoteCarrierRate calculates carrier shipping cost in USD cents for an order.
// Latent bug 1 (INC-101): Rejects non-positive order subtotals (subtotalCents <= 0)
// with CarrierTariffBoundsError when stacked promos reduce an order to $0 or negative.
// Latent bug 2 (INC-104): Rejects UN3480-LITHIUM hazmat shipments above 1000g
// with HazmatWeightTierOverflowError because only NONE and ORM-D air tiers were configured.
func QuoteCarrierRate(subtotalCents int, weightGrams int, hazmatClass string) (int, error) {
	if subtotalCents <= 0 {
		return 0, fmt.Errorf(
			"HTTP 500: CarrierTariffBoundsError: non-positive order subtotal %d cents rejected by carrier rate engine",
			subtotalCents,
		)
	}
	if hazmatClass == "" || hazmatClass == "NONE" {
		base := 799
		if weightGrams > 1000 {
			base += ((weightGrams - 1000) / 500) * 150
		}
		return base, nil
	}
	if hazmatClass == "ORM-D" && weightGrams <= 1000 {
		return 1199, nil
	}
	return 0, fmt.Errorf(
		"HTTP 500: HazmatWeightTierOverflowError: no carrier rate tier for hazmat=%s at weight=%dg (max air tier 1000g)",
		hazmatClass, weightGrams,
	)
}
