export interface CatalogProduct {
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

/**
 * Converts USD cents into the target currency's minor units.
 * Latent bug (INC-103): For zero-decimal currencies like JPY, divides by 100
 * and keeps 2 decimal places (e.g. 4476.15) instead of rounding to an integer
 * minor unit, causing downstream ISO-4217 integer minor-unit validation to fail.
 */
export function convertToCurrencyMinorUnits(usdCents: number, currency: string): number {
  const code = currency.toUpperCase();
  const rate = FX_RATES[code];
  if (!rate) {
    throw new Error(`Unsupported currency: ${currency}`);
  }
  if (code === "JPY") {
    const rawMinor = Number(((usdCents / 100) * rate).toFixed(2));
    if (!Number.isInteger(rawMinor)) {
      throw new Error(
        `HTTP 500: StorefrontCurrencyPrecisionError: zero-decimal currency JPY payload contains fractional minor units: ${rawMinor}`
      );
    }
    return rawMinor;
  }
  return Math.round(usdCents * rate);
}
