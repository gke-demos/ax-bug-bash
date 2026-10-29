package storefront

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// BaselineGitTag is the canonical initial Git tag on storefront.git used for < 3s resets.
const BaselineGitTag = "baseline-v1"

const (
	IncidentStackedPromoNegativeSubtotal = "INC-101"
	IncidentFlashSaleBundleUnderflow     = "INC-102"
	IncidentZeroDecimalJPYRounding       = "INC-103"
	IncidentLithiumHazmatWeightTier      = "INC-104"
)

const (
	ServiceStorefrontWeb = "storefront-web"
	ServiceCheckout      = "checkout-service"
	ServiceShipping      = "shipping-service"
	ServicePricing       = "pricing-service"
	ServiceInventory     = "inventory-service"
)

// AllServices returns all 5 microservices in the polyglot Retail Storefront.
func AllServices() []string {
	return []string{
		ServiceStorefrontWeb,
		ServiceCheckout,
		ServiceShipping,
		ServicePricing,
		ServiceInventory,
	}
}

// ServiceLanguage returns the canonical display language/framework label for a storefront microservice.
func ServiceLanguage(service string) string {
	switch service {
	case ServiceStorefrontWeb:
		return "Node.js / TypeScript"
	case ServiceCheckout, ServiceShipping:
		return "Go"
	case ServicePricing:
		return "Python / FastAPI"
	case ServiceInventory:
		return "Rust / Axum"
	default:
		return "Polyglot"
	}
}

// ServiceCacheMount returns the pre-warmed build cache mount description for a storefront microservice.
func ServiceCacheMount(service string) string {
	switch service {
	case ServiceStorefrontWeb:
		return "/cache/node_modules (pre-warmed tsx/esbuild bundle)"
	case ServiceCheckout, ServiceShipping:
		return "/cache/gocache & /cache/gomodcache (incremental go build)"
	case ServicePricing:
		return "/cache/venv (pre-warmed wheel & bytecode cache)"
	case ServiceInventory:
		return "/cache/cargo-target (pre-warmed crate artifacts)"
	default:
		return "/cache/prewarmed"
	}
}

// Product represents an item in the Retail Storefront catalog.
type Product struct {
	SKU                   string `json:"sku"`
	Name                  string `json:"name"`
	Category              string `json:"category"`
	Description           string `json:"description"`
	PriceUSDCents         int    `json:"price_usd_cents"`
	WeightGrams           int    `json:"weight_grams"`
	HazmatClass           string `json:"hazmat_class"`
	AvailableStock        int    `json:"available_stock"`
	FlashBundleBonusUnits int    `json:"flash_bundle_bonus_units"`
}

// CartItem represents a requested SKU and quantity in a checkout request.
type CartItem struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
}

// CheckoutRequest is the payload sent to POST /api/cart/checkout.
type CheckoutRequest struct {
	Items       []CartItem `json:"items"`
	Currency    string     `json:"currency"`
	PromoCodes  []string   `json:"promo_codes"`
	FlashBundle bool       `json:"flash_bundle"`
}

// ServiceIncidentDetail describes the exact failure, stack trace, regression test,
// and verified code fix for a single implicated microservice in an Incident Scenario.
type ServiceIncidentDetail struct {
	Service                   string `json:"service"`
	Language                  string `json:"language"`
	SourceFile                string `json:"source_file"`
	TestFile                  string `json:"test_file"`
	ErrorType                 string `json:"error_type"`
	ErrorMessage              string `json:"error_message"`
	StackTrace                string `json:"stack_trace"`
	RegressionTestName        string `json:"regression_test_name"`
	RegressionTestFileContent string `json:"regression_test_file_content"`
	PatchedSourceFileContent  string `json:"patched_source_file_content"`
}

// IncidentScenario defines one of the 4 subtle multi-service production failure modes.
type IncidentScenario struct {
	ID                 string                           `json:"id"`
	Title              string                           `json:"title"`
	Summary            string                           `json:"summary"`
	WhyCIMissedIt      string                           `json:"why_ci_missed_it"`
	ImplicatedServices []string                         `json:"implicated_services"`
	UnaffectedServices []string                         `json:"unaffected_services"`
	TriggerCart        CheckoutRequest                  `json:"trigger_cart"`
	ErrorCode          string                           `json:"error_code"`
	ErrorMessage       string                           `json:"error_message"`
	ServiceTraces      map[string]ServiceIncidentDetail `json:"service_traces"`
}

// ProductCatalog returns the 4 storefront items covering accessories, flash bundles, workspace gear, and hazmat electronics.
func ProductCatalog() []Product {
	return []Product{
		{
			SKU:                   "sku-cable-usbc",
			Name:                  "Braided USB-C 240W Cable",
			Category:              "Accessories",
			Description:           "2-meter braided USB4 / PD 3.1 EPR charging cable ($16.00).",
			PriceUSDCents:         1600,
			WeightGrams:           120,
			HazmatClass:           "NONE",
			AvailableStock:        45,
			FlashBundleBonusUnits: 0,
		},
		{
			SKU:                   "sku-mechanical-kb-bundle",
			Name:                  "Tactile Mechanical Keyboard (Flash 2-for-1 Bundle)",
			Category:              "Peripherals",
			Description:           "Hot-swappable aluminum 75% keyboard with Flash-Sale 2-for-1 bonus units ($145.00).",
			PriceUSDCents:         14500,
			WeightGrams:           980,
			HazmatClass:           "NONE",
			AvailableStock:        2,
			FlashBundleBonusUnits: 2,
		},
		{
			SKU:                   "sku-desk-mat-xl",
			Name:                  "Merino Felt Desk Mat XL",
			Category:              "Workspace",
			Description:           "900x400mm natural merino wool felt desk pad with stitched edge ($29.00).",
			PriceUSDCents:         2900,
			WeightGrams:           450,
			HazmatClass:           "NONE",
			AvailableStock:        18,
			FlashBundleBonusUnits: 0,
		},
		{
			SKU:                   "sku-power-station-240wh",
			Name:                  "Portable Power Station 240Wh (LiFePO4)",
			Category:              "Power & Hazmat",
			Description:           "High-capacity LiFePO4 field battery pack (3,400g, UN3480-LITHIUM class, $199.00).",
			PriceUSDCents:         19900,
			WeightGrams:           3400,
			HazmatClass:           "UN3480-LITHIUM",
			AvailableStock:        9,
			FlashBundleBonusUnits: 0,
		},
	}
}

const patchedPricingSource = `"""Pricing service for promo code stacking and currency conversion."""

from typing import List


FX_RATES = {
    "USD": 1.0,
    "EUR": 0.92,
    "JPY": 154.35,
}


def apply_stacked_promos(subtotal_cents: int, promo_codes: List[str]) -> int:
    """Applies promo codes in sequence to subtotal_cents and clamps at 0."""
    discounted = subtotal_cents
    for code in promo_codes:
        normalized = code.strip().upper()
        if normalized == "SAVE20":
            discounted = int(round(discounted * 0.80))
        elif normalized == "FLAT15":
            discounted -= 1500
    return max(0, discounted)


def convert_currency_minor_units(usd_cents: int, currency: str) -> int:
    """Converts USD cents to target currency integer minor units (including zero-decimal JPY)."""
    code = currency.strip().upper()
    if code not in FX_RATES:
        raise ValueError(f"Unsupported currency: {currency}")
    rate = FX_RATES[code]
    if code == "JPY":
        return int(round((usd_cents / 100.0) * rate))
    return int(round(usd_cents * rate))
`

const patchedShippingSource = `package shipping

import "fmt"

// QuoteCarrierRate calculates carrier shipping cost in USD cents for an order.
// Handles zero-subtotal promotional orders and UN3480-LITHIUM regulated ground tiers.
func QuoteCarrierRate(subtotalCents int, weightGrams int, hazmatClass string) (int, error) {
	if subtotalCents < 0 {
		subtotalCents = 0
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
	if hazmatClass == "UN3480-LITHIUM" {
		groundBase := 2499
		if weightGrams > 1000 {
			groundBase += ((weightGrams - 1000) / 500) * 250
		}
		return groundBase, nil
	}
	return 0, fmt.Errorf(
		"HTTP 500: HazmatWeightTierOverflowError: no carrier rate tier for hazmat=%s at weight=%dg (max air tier 1000g)",
		hazmatClass, weightGrams,
	)
}
`

const patchedInventorySource = `/// High-concurrency SKU stock reservation and warehouse hazmat ledger.

/// Reserves SKU units for standard and flash-sale bundle orders, capping bonus units at remaining stock.
pub fn reserve_bundle_stock(
    available: u32,
    requested: u32,
    bonus_bundle_units: u32,
) -> Result<u32, String> {
    if requested == 0 || requested > available {
        return Err(format!(
            "insufficient base stock: available={} requested={}",
            available, requested
        ));
    }
    let remaining_after_base = available.saturating_sub(requested);
    let effective_bonus = bonus_bundle_units.min(remaining_after_base);
    let total_units = requested + effective_bonus;
    Ok(available.saturating_sub(total_units))
}

/// Classifies warehouse handling tier for a SKU based on weight and hazmat class.
pub fn classify_warehouse_hazmat(
    weight_grams: u32,
    hazmat_class: &str,
) -> Result<&'static str, String> {
    match hazmat_class {
        "" | "NONE" => Ok("STANDARD_PARCEL"),
        "ORM-D" if weight_grams <= 1000 => Ok("LIMITED_QUANTITY_AIR"),
        "UN3480-LITHIUM" => Ok("GROUND_LITHIUM_REGULATED"),
        _ => Err(format!(
            "HTTP 500: UnhandledHazmatClassError: warehouse ledger panicked on unmapped hazmat class {} ({}g)",
            hazmat_class, weight_grams
        )),
    }
}
`

const patchedCheckoutSource = `package checkout

import (
	"fmt"
	"math"
)

// ReserveFlashBundleOrder validates and computes the total units to reserve for an order,
// capping flash-sale bonus units to available stock after base quantity reservation.
func ReserveFlashBundleOrder(availableStock int, requestedQty int, bonusBundleUnits int) (int, error) {
	if requestedQty <= 0 {
		return 0, fmt.Errorf("requested quantity must be positive")
	}
	if requestedQty > availableStock {
		return 0, fmt.Errorf("insufficient base stock: requested=%d available=%d", requestedQty, availableStock)
	}
	remaining := availableStock - requestedQty
	effectiveBonus := bonusBundleUnits
	if effectiveBonus > remaining {
		effectiveBonus = remaining
	}
	if effectiveBonus < 0 {
		effectiveBonus = 0
	}
	return requestedQty + effectiveBonus, nil
}

// CaptureCurrencyPayment normalizes and rounds minor-unit amounts (including zero-decimal JPY)
// before gateway payment capture.
func CaptureCurrencyPayment(amountMinor float64, currency string) (int64, error) {
	if currency == "JPY" {
		return int64(math.Round(amountMinor)), nil
	}
	if math.Abs(amountMinor-math.Trunc(amountMinor)) > 1e-6 {
		return 0, fmt.Errorf(
			"HTTP 500: PaymentCapturePrecisionError: ISO-4217 zero-decimal currency %s rejected fractional minor unit %.2f",
			currency, amountMinor,
		)
	}
	return int64(math.Round(amountMinor)), nil
}
`

const patchedStorefrontWebSource = `export interface CatalogProduct {
  sku: string;
  name: string;
  category: string;
  priceUsdCents: number;
  weightGrams: number;
  hazmatClass: string;
  availableStock: number;
  flashBundleBonusUnits: number;
}

export const CATALOG_PRODUCTS: CatalogProduct[] = [
  {
    sku: "sku-cable-usbc",
    name: "Braided USB-C 240W Cable",
    category: "Accessories",
    priceUsdCents: 1600,
    weightGrams: 120,
    hazmatClass: "NONE",
    availableStock: 45,
    flashBundleBonusUnits: 0,
  },
  {
    sku: "sku-mechanical-kb-bundle",
    name: "Tactile Mechanical Keyboard (Flash 2-for-1 Bundle)",
    category: "Peripherals",
    priceUsdCents: 14500,
    weightGrams: 980,
    hazmatClass: "NONE",
    availableStock: 2,
    flashBundleBonusUnits: 2,
  },
  {
    sku: "sku-desk-mat-xl",
    name: "Merino Felt Desk Mat XL",
    category: "Workspace",
    priceUsdCents: 2900,
    weightGrams: 450,
    hazmatClass: "NONE",
    availableStock: 18,
    flashBundleBonusUnits: 0,
  },
  {
    sku: "sku-power-station-240wh",
    name: "Portable Power Station 240Wh (LiFePO4)",
    category: "Power & Hazmat",
    priceUsdCents: 19900,
    weightGrams: 3400,
    hazmatClass: "UN3480-LITHIUM",
    availableStock: 9,
    flashBundleBonusUnits: 0,
  },
];

const FX_RATES: Record<string, number> = {
  USD: 1.0,
  EUR: 0.92,
  JPY: 154.35,
};

export function convertToCurrencyMinorUnits(usdCents: number, currency: string): number {
  const code = currency.toUpperCase();
  const rate = FX_RATES[code];
  if (!rate) {
    throw new Error(` + "`Unsupported currency: ${currency}`" + `);
  }
  if (code === "JPY") {
    return Math.round((usdCents / 100) * rate);
  }
  return Math.round(usdCents * rate);
}
`

// ScenarioCatalog returns the 4 Incident Scenarios in canonical order.
func ScenarioCatalog() []IncidentScenario {
	return []IncidentScenario{
		{
			ID:                 IncidentStackedPromoNegativeSubtotal,
			Title:              "Stacked Promo Negative Subtotal Crash",
			Summary:            "Stacking percentage (SAVE20) and flat-rate (FLAT15) promo codes on a $16.00 accessory yields a -220 cent subtotal, crashing pricing-service and shipping-service carrier tariff bounds.",
			WhyCIMissedIt:      "Pre-existing unit tests only tested SAVE20 and FLAT15 individually on carts >= $40.00 and only tested positive subtotals in shipping-service.",
			ImplicatedServices: []string{ServicePricing, ServiceShipping},
			UnaffectedServices: []string{ServiceStorefrontWeb, ServiceCheckout, ServiceInventory},
			TriggerCart: CheckoutRequest{
				Items:      []CartItem{{SKU: "sku-cable-usbc", Quantity: 1}},
				Currency:   "USD",
				PromoCodes: []string{"SAVE20", "FLAT15"},
			},
			ErrorCode:    "NEGATIVE_SUBTOTAL_CARRIER_BOUNDS",
			ErrorMessage: "HTTP 500: NegativeSubtotalError in pricing-service (-220 cents) & CarrierTariffBoundsError in shipping-service (0 cents)",
			ServiceTraces: map[string]ServiceIncidentDetail{
				ServicePricing: {
					Service:      ServicePricing,
					Language:     "Python / FastAPI",
					SourceFile:   "pricing.py",
					TestFile:     "test_pricing.py",
					ErrorType:    "NegativeSubtotalError",
					ErrorMessage: "HTTP 500: NegativeSubtotalError: stacked promo codes ['SAVE20', 'FLAT15'] produced negative subtotal -220 cents",
					StackTrace: `Traceback (most recent call last):
  File "/app/services/pricing-service/pricing.py", line 25, in apply_stacked_promos
    raise ValueError("HTTP 500: NegativeSubtotalError: stacked promo codes ['SAVE20', 'FLAT15'] produced negative subtotal -220 cents")
ValueError: HTTP 500: NegativeSubtotalError: stacked promo codes ['SAVE20', 'FLAT15'] produced negative subtotal -220 cents`,
					RegressionTestName: "test_stacked_promos_clamp_at_zero_subtotal",
					RegressionTestFileContent: `import unittest
from pricing import apply_stacked_promos, convert_currency_minor_units


class TestPricingService(unittest.TestCase):
    def test_single_promo_codes_on_standard_cart(self):
        self.assertEqual(apply_stacked_promos(5000, ["SAVE20"]), 4000)
        self.assertEqual(apply_stacked_promos(4000, ["FLAT15"]), 2500)

    def test_currency_conversion_usd_and_eur(self):
        self.assertEqual(convert_currency_minor_units(2900, "USD"), 2900)
        self.assertEqual(convert_currency_minor_units(2900, "EUR"), 2668)

    def test_stacked_promos_clamp_at_zero_subtotal(self):
        self.assertEqual(apply_stacked_promos(1600, ["SAVE20", "FLAT15"]), 0)


if __name__ == "__main__":
    unittest.main()
`,
					PatchedSourceFileContent: patchedPricingSource,
				},
				ServiceShipping: {
					Service:      ServiceShipping,
					Language:     "Go",
					SourceFile:   "shipping.go",
					TestFile:     "shipping_test.go",
					ErrorType:    "CarrierTariffBoundsError",
					ErrorMessage: "HTTP 500: CarrierTariffBoundsError: non-positive order subtotal 0 cents rejected by carrier rate engine",
					StackTrace: `goroutine 18 [running]:
github.com/gke-demos/ax-bug-bash/services/shipping-service.QuoteCarrierRate(0x0, 0x78, {0x4a8c10, 0x4})
	/workspace/services/shipping-service/shipping.go:12 +0x145
main.handleShippingQuote(...)
	/workspace/services/shipping-service/main.go:41 +0x92`,
					RegressionTestName: "TestQuoteCarrierRateZeroSubtotalStackedPromo",
					RegressionTestFileContent: `package shipping

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

func TestQuoteCarrierRateZeroSubtotalStackedPromo(t *testing.T) {
	rate, err := QuoteCarrierRate(0, 120, "NONE")
	if err != nil {
		t.Fatalf("expected zero-subtotal promotional order to succeed, got: %v", err)
	}
	if rate != 799 {
		t.Fatalf("expected base rate 799 cents for promotional order, got %d", rate)
	}
}
`,
					PatchedSourceFileContent: patchedShippingSource,
				},
			},
		},
		{
			ID:                 IncidentFlashSaleBundleUnderflow,
			Title:              "Flash-Sale Bundle u32 Stock Underflow",
			Summary:            "Ordering 2 units of the Flash-Sale 2-for-1 Mechanical Keyboard Bundle when 2 units remain in stock adds 2 bonus units without capping against remaining inventory, causing a u32 subtraction underflow panic in inventory-service and uint32 wrap-around in checkout-service.",
			WhyCIMissedIt:      "Baseline unit tests only exercised single-item reservations with bonus_bundle_units=0 when stock was plentiful (available=10, requested=2).",
			ImplicatedServices: []string{ServiceInventory, ServiceCheckout},
			UnaffectedServices: []string{ServiceStorefrontWeb, ServiceShipping, ServicePricing},
			TriggerCart: CheckoutRequest{
				Items:       []CartItem{{SKU: "sku-mechanical-kb-bundle", Quantity: 2}},
				Currency:    "USD",
				FlashBundle: true,
			},
			ErrorCode:    "FLASH_BUNDLE_U32_UNDERFLOW",
			ErrorMessage: "HTTP 500: RustU32UnderflowPanic in inventory-service (available=2, requested=2, bonus=2) & FlashBundleUnderflowError in checkout-service",
			ServiceTraces: map[string]ServiceIncidentDetail{
				ServiceInventory: {
					Service:      ServiceInventory,
					Language:     "Rust / Axum",
					SourceFile:   "src/lib.rs",
					TestFile:     "src/lib.rs",
					ErrorType:    "RustU32UnderflowPanic",
					ErrorMessage: "HTTP 500: RustU32UnderflowPanic: attempt to subtract with overflow in reserve_bundle_stock (available=2, requested=2, bonus=2)",
					StackTrace: `thread 'tokio-runtime-worker' panicked at src/lib.rs:20:9:
HTTP 500: RustU32UnderflowPanic: attempt to subtract with overflow in reserve_bundle_stock (available=2, requested=2, bonus=2)
stack backtrace:
   0: rust_begin_unwind
   1: core::panicking::panic_fmt
   2: inventory_service::reserve_bundle_stock
             at ./src/lib.rs:20:9`,
					RegressionTestName:        "test_flash_sale_bundle_caps_bonus_without_u32_underflow",
					RegressionTestFileContent: buildInventoryTestFile("test_flash_sale_bundle_caps_bonus_without_u32_underflow", false),
					PatchedSourceFileContent:  buildInventoryTestFile("test_flash_sale_bundle_caps_bonus_without_u32_underflow", true),
				},
				ServiceCheckout: {
					Service:      ServiceCheckout,
					Language:     "Go",
					SourceFile:   "checkout.go",
					TestFile:     "checkout_test.go",
					ErrorType:    "FlashBundleUnderflowError",
					ErrorMessage: "HTTP 500: FlashBundleUnderflowError: uint32 wrap-around 4294967294 on SKU reservation (available=2, requested=2, bonus=2)",
					StackTrace: `goroutine 34 [running]:
github.com/gke-demos/ax-bug-bash/services/checkout-service.ReserveFlashBundleOrder(0x2, 0x2, 0x2)
	/workspace/services/checkout-service/checkout.go:20 +0x168
main.handleCheckoutOrder(...)
	/workspace/services/checkout-service/main.go:58 +0x114`,
					RegressionTestName: "TestReserveFlashBundleOrderCapsBonusAtAvailableStock",
					RegressionTestFileContent: `package checkout

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

func TestReserveFlashBundleOrderCapsBonusAtAvailableStock(t *testing.T) {
	units, err := ReserveFlashBundleOrder(2, 2, 2)
	if err != nil {
		t.Fatalf("expected flash bundle reservation to cap bonus units instead of underflowing, got: %v", err)
	}
	if units != 2 {
		t.Fatalf("expected 2 capped units reserved, got %d", units)
	}
}
`,
					PatchedSourceFileContent: patchedCheckoutSource,
				},
			},
		},
		{
			ID:                 IncidentZeroDecimalJPYRounding,
			Title:              "Zero-Decimal JPY Currency Rounding Mismatch",
			Summary:            "Switching storefront currency to JPY converts $29.00 at FX rate 154.35 into fractional minor unit 4476.15 instead of rounding to an integer minor unit (4476), triggering ISO-4217 precision failures across storefront-web, pricing-service, and checkout-service.",
			WhyCIMissedIt:      "Pre-existing currency tests across Node.js, Python, and Go only tested 2-decimal currencies (USD and EUR).",
			ImplicatedServices: []string{ServiceStorefrontWeb, ServicePricing, ServiceCheckout},
			UnaffectedServices: []string{ServiceShipping, ServiceInventory},
			TriggerCart: CheckoutRequest{
				Items:    []CartItem{{SKU: "sku-desk-mat-xl", Quantity: 1}},
				Currency: "JPY",
			},
			ErrorCode:    "JPY_ZERO_DECIMAL_PRECISION_MISMATCH",
			ErrorMessage: "HTTP 500: ISO-4217 zero-decimal currency JPY fractional minor unit 4476.15 rejected across storefront-web, pricing-service, and checkout-service",
			ServiceTraces: map[string]ServiceIncidentDetail{
				ServiceStorefrontWeb: {
					Service:      ServiceStorefrontWeb,
					Language:     "Node.js / TypeScript",
					SourceFile:   "src/server.ts",
					TestFile:     "test/storefront.test.mjs",
					ErrorType:    "StorefrontCurrencyPrecisionError",
					ErrorMessage: "HTTP 500: StorefrontCurrencyPrecisionError: zero-decimal currency JPY payload contains fractional minor units: 4476.15",
					StackTrace: `Error: HTTP 500: StorefrontCurrencyPrecisionError: zero-decimal currency JPY payload contains fractional minor units: 4476.15
    at convertToCurrencyMinorUnits (/workspace/services/storefront-web/src/server.ts:69:13)
    at buildCheckoutOrder (/workspace/services/storefront-web/src/server.ts:84:22)`,
					RegressionTestName: "convertToCurrencyMinorUnits rounds zero-decimal JPY to integer minor units",
					RegressionTestFileContent: `import assert from "node:assert/strict";
import { CATALOG_PRODUCTS, convertToCurrencyMinorUnits } from "../src/server.ts";

const tests = [];
function test(name, fn) {
  tests.push({ name, fn });
}

test("catalog exposes all 4 core storefront products", () => {
  assert.equal(CATALOG_PRODUCTS.length, 4);
  assert.equal(CATALOG_PRODUCTS[0].sku, "sku-cable-usbc");
});

test("convertToCurrencyMinorUnits converts USD and EUR to integer minor units", () => {
  assert.equal(convertToCurrencyMinorUnits(2900, "USD"), 2900);
  assert.equal(convertToCurrencyMinorUnits(2900, "EUR"), 2668);
});

test("convertToCurrencyMinorUnits rounds zero-decimal JPY to integer minor units", () => {
  assert.equal(convertToCurrencyMinorUnits(2900, "JPY"), 4476);
});

let failed = 0;
for (const t of tests) {
  try {
    t.fn();
    console.log(` + "`PASS - ${t.name}`" + `);
  } catch (err) {
    failed++;
    console.error(` + "`FAIL - ${t.name}: ${err.message}`" + `);
  }
}
if (failed > 0) {
  process.exit(1);
}
console.log(` + "`PASS (${tests.length} tests)`" + `);
`,
					PatchedSourceFileContent: patchedStorefrontWebSource,
				},
				ServicePricing: {
					Service:      ServicePricing,
					Language:     "Python / FastAPI",
					SourceFile:   "pricing.py",
					TestFile:     "test_pricing.py",
					ErrorType:    "CurrencyPrecisionMismatchError",
					ErrorMessage: "HTTP 500: CurrencyPrecisionMismatchError: ISO-4217 zero-decimal currency JPY cannot have fractional minor units: 4476.15",
					StackTrace: `Traceback (most recent call last):
  File "/app/services/pricing-service/pricing.py", line 42, in convert_currency_minor_units
    raise ValueError("HTTP 500: CurrencyPrecisionMismatchError: ISO-4217 zero-decimal currency JPY cannot have fractional minor units: 4476.15")
ValueError: HTTP 500: CurrencyPrecisionMismatchError: ISO-4217 zero-decimal currency JPY cannot have fractional minor units: 4476.15`,
					RegressionTestName: "test_zero_decimal_jpy_currency_rounds_to_integer",
					RegressionTestFileContent: `import unittest
from pricing import apply_stacked_promos, convert_currency_minor_units


class TestPricingService(unittest.TestCase):
    def test_single_promo_codes_on_standard_cart(self):
        self.assertEqual(apply_stacked_promos(5000, ["SAVE20"]), 4000)
        self.assertEqual(apply_stacked_promos(4000, ["FLAT15"]), 2500)

    def test_currency_conversion_usd_and_eur(self):
        self.assertEqual(convert_currency_minor_units(2900, "USD"), 2900)
        self.assertEqual(convert_currency_minor_units(2900, "EUR"), 2668)

    def test_zero_decimal_jpy_currency_rounds_to_integer(self):
        self.assertEqual(convert_currency_minor_units(2900, "JPY"), 4476)


if __name__ == "__main__":
    unittest.main()
`,
					PatchedSourceFileContent: patchedPricingSource,
				},
				ServiceCheckout: {
					Service:      ServiceCheckout,
					Language:     "Go",
					SourceFile:   "checkout.go",
					TestFile:     "checkout_test.go",
					ErrorType:    "PaymentCapturePrecisionError",
					ErrorMessage: "HTTP 500: PaymentCapturePrecisionError: ISO-4217 zero-decimal currency JPY rejected fractional minor unit 4476.15",
					StackTrace: `goroutine 22 [running]:
github.com/gke-demos/ax-bug-bash/services/checkout-service.CaptureCurrencyPayment(0x40b17c2666666666, {0x4a9120, 0x3})
	/workspace/services/checkout-service/checkout.go:32 +0x112
main.handleCheckoutOrder(...)
	/workspace/services/checkout-service/main.go:64 +0x158`,
					RegressionTestName: "TestCaptureCurrencyPaymentRoundsZeroDecimalJPY",
					RegressionTestFileContent: `package checkout

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

func TestCaptureCurrencyPaymentRoundsZeroDecimalJPY(t *testing.T) {
	captured, err := CaptureCurrencyPayment(4476.15, "JPY")
	if err != nil {
		t.Fatalf("expected JPY fractional minor unit to round cleanly, got: %v", err)
	}
	if captured != 4476 {
		t.Fatalf("expected 4476 JPY minor units, got %d", captured)
	}
}
`,
					PatchedSourceFileContent: patchedCheckoutSource,
				},
			},
		},
		{
			ID:                 IncidentLithiumHazmatWeightTier,
			Title:              "Lithium Hazmat Weight Tier Overflow",
			Summary:            "Checking out with the 3,400g Portable Power Station 240Wh (UN3480-LITHIUM) triggers an unmapped hazmat classification error in inventory-service and overflows the 1,000g ORM-D air tier in shipping-service.",
			WhyCIMissedIt:      "Existing tests only covered non-hazmat parcels (NONE) and light ORM-D parcels under 1,000g, missing regulated ground lithium batteries above 1,000g.",
			ImplicatedServices: []string{ServiceInventory, ServiceShipping},
			UnaffectedServices: []string{ServiceStorefrontWeb, ServiceCheckout, ServicePricing},
			TriggerCart: CheckoutRequest{
				Items:    []CartItem{{SKU: "sku-power-station-240wh", Quantity: 1}},
				Currency: "USD",
			},
			ErrorCode:    "LITHIUM_HAZMAT_WEIGHT_OVERFLOW",
			ErrorMessage: "HTTP 500: UnhandledHazmatClassError in inventory-service (UN3480-LITHIUM, 3400g) & HazmatWeightTierOverflowError in shipping-service",
			ServiceTraces: map[string]ServiceIncidentDetail{
				ServiceInventory: {
					Service:      ServiceInventory,
					Language:     "Rust / Axum",
					SourceFile:   "src/lib.rs",
					TestFile:     "src/lib.rs",
					ErrorType:    "UnhandledHazmatClassError",
					ErrorMessage: "HTTP 500: UnhandledHazmatClassError: warehouse ledger panicked on unmapped hazmat class UN3480-LITHIUM (3400g)",
					StackTrace: `thread 'tokio-runtime-worker' panicked at src/lib.rs:36:14:
HTTP 500: UnhandledHazmatClassError: warehouse ledger panicked on unmapped hazmat class UN3480-LITHIUM (3400g)
stack backtrace:
   0: rust_begin_unwind
   1: inventory_service::classify_warehouse_hazmat
             at ./src/lib.rs:36:14`,
					RegressionTestName:        "test_classify_un3480_lithium_heavy_ground_tier",
					RegressionTestFileContent: buildInventoryTestFile("test_classify_un3480_lithium_heavy_ground_tier", false),
					PatchedSourceFileContent:  buildInventoryTestFile("test_classify_un3480_lithium_heavy_ground_tier", true),
				},
				ServiceShipping: {
					Service:      ServiceShipping,
					Language:     "Go",
					SourceFile:   "shipping.go",
					TestFile:     "shipping_test.go",
					ErrorType:    "HazmatWeightTierOverflowError",
					ErrorMessage: "HTTP 500: HazmatWeightTierOverflowError: no carrier rate tier for hazmat=UN3480-LITHIUM at weight=3400g (max air tier 1000g)",
					StackTrace: `goroutine 29 [running]:
github.com/gke-demos/ax-bug-bash/services/shipping-service.QuoteCarrierRate(0x4db4, 0xd48, {0x4b1200, 0xe})
	/workspace/services/shipping-service/shipping.go:26 +0x1c4
main.handleShippingQuote(...)
	/workspace/services/shipping-service/main.go:41 +0x92`,
					RegressionTestName: "TestQuoteCarrierRateLithiumHazmatGroundTier",
					RegressionTestFileContent: `package shipping

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

func TestQuoteCarrierRateLithiumHazmatGroundTier(t *testing.T) {
	rate, err := QuoteCarrierRate(19900, 3400, "UN3480-LITHIUM")
	if err != nil {
		t.Fatalf("expected regulated ground rate for UN3480-LITHIUM 3400g, got error: %v", err)
	}
	if rate != 3499 {
		t.Fatalf("expected 3499 cents for 3400g lithium ground shipment, got %d", rate)
	}
}
`,
					PatchedSourceFileContent: patchedShippingSource,
				},
			},
		},
	}
}

func buildInventoryTestFile(extraTestName string, patched bool) string {
	unpatchedImpl := `/// High-concurrency SKU stock reservation and warehouse hazmat ledger.

pub fn reserve_bundle_stock(
    available: u32,
    requested: u32,
    bonus_bundle_units: u32,
) -> Result<u32, String> {
    if requested == 0 || requested > available {
        return Err(format!(
            "insufficient base stock: available={} requested={}",
            available, requested
        ));
    }
    let total_units = requested + bonus_bundle_units;
    if total_units > available {
        panic!(
            "HTTP 500: RustU32UnderflowPanic: attempt to subtract with overflow in reserve_bundle_stock (available={}, requested={}, bonus={})",
            available, requested, bonus_bundle_units
        );
    }
    Ok(available - total_units)
}

pub fn classify_warehouse_hazmat(
    weight_grams: u32,
    hazmat_class: &str,
) -> Result<&'static str, String> {
    match hazmat_class {
        "" | "NONE" => Ok("STANDARD_PARCEL"),
        "ORM-D" if weight_grams <= 1000 => Ok("LIMITED_QUANTITY_AIR"),
        _ => Err(format!(
            "HTTP 500: UnhandledHazmatClassError: warehouse ledger panicked on unmapped hazmat class {} ({}g)",
            hazmat_class, weight_grams
        )),
    }
}
`
	impl := unpatchedImpl
	if patched {
		impl = patchedInventorySource
	}

	extraTest := ""
	switch extraTestName {
	case "test_flash_sale_bundle_caps_bonus_without_u32_underflow":
		extraTest = `
    #[test]
    fn test_flash_sale_bundle_caps_bonus_without_u32_underflow() {
        assert_eq!(reserve_bundle_stock(2, 2, 2), Ok(0));
    }
`
	case "test_classify_un3480_lithium_heavy_ground_tier":
		extraTest = `
    #[test]
    fn test_classify_un3480_lithium_heavy_ground_tier() {
        assert_eq!(classify_warehouse_hazmat(3400, "UN3480-LITHIUM"), Ok("GROUND_LITHIUM_REGULATED"));
    }
`
	}

	return impl + `
#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_reserve_standard_sku() {
        assert_eq!(reserve_bundle_stock(10, 2, 0), Ok(8));
    }

    #[test]
    fn test_classify_standard_merchandise() {
        assert_eq!(classify_warehouse_hazmat(450, "NONE"), Ok("STANDARD_PARCEL"));
    }
` + extraTest + `}
`
}

// GetScenario looks up an IncidentScenario by ID (e.g. "INC-101").
func GetScenario(id string) (IncidentScenario, bool) {
	for _, sc := range ScenarioCatalog() {
		if sc.ID == id {
			return sc, true
		}
	}
	return IncidentScenario{}, false
}

// SeedServicesTree copies the 5-service monorepo (and root go.mod) into destRoot.
func SeedServicesTree(destRoot string) error {
	srcServices := strings.TrimSpace(os.Getenv("STOREFRONT_SERVICES_DIR"))
	if srcServices != "" {
		if st, err := os.Stat(srcServices); err != nil || !st.IsDir() {
			srcServices = ""
		}
	}
	if srcServices == "" {
		_, callerFile, _, ok := runtime.Caller(0)
		if !ok {
			return fmt.Errorf("failed to resolve caller path")
		}
		srcRepoRoot := filepath.Clean(filepath.Join(filepath.Dir(callerFile), "..", ".."))
		srcServices = filepath.Join(srcRepoRoot, "services")
	}
	dstServices := filepath.Join(destRoot, "services")

	if err := copyDirRecursive(srcServices, dstServices); err != nil {
		return fmt.Errorf("copying services tree: %w", err)
	}

	goModContent := "module github.com/gke-demos/ax-bug-bash\n\ngo 1.23\n"
	if err := os.WriteFile(filepath.Join(destRoot, "go.mod"), []byte(goModContent), 0o644); err != nil {
		return fmt.Errorf("writing workspace go.mod: %w", err)
	}
	return nil
}

func copyDirRecursive(srcDir, dstDir string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			switch name {
			case "node_modules", "target", "__pycache__", ".pytest_cache", "bin":
				continue
			}
			if err := copyDirRecursive(filepath.Join(srcDir, name), filepath.Join(dstDir, name)); err != nil {
				return err
			}
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if err := copyFile(filepath.Join(srcDir, name), filepath.Join(dstDir, name), info.Mode()); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(srcPath, dstPath string, mode os.FileMode) error {
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
}
