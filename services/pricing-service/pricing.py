"""Pricing service for promo code stacking and currency conversion."""

from typing import List


FX_RATES = {
    "USD": 1.0,
    "EUR": 0.92,
    "JPY": 154.35,
}


def apply_stacked_promos(subtotal_cents: int, promo_codes: List[str]) -> int:
    """Applies promo codes in sequence to subtotal_cents.

    Latent bug (INC-101): Does not clamp subtotal at 0 when percentage and
    flat promos are stacked on a low-priced item, raising NegativeSubtotalError.
    """
    discounted = subtotal_cents
    for code in promo_codes:
        normalized = code.strip().upper()
        if normalized == "SAVE20":
            discounted = int(round(discounted * 0.80))
        elif normalized == "FLAT15":
            discounted -= 1500
    if discounted < 0:
        raise ValueError(
            f"HTTP 500: NegativeSubtotalError: stacked promo codes {promo_codes} produced negative subtotal {discounted} cents"
        )
    return discounted


def convert_currency_minor_units(usd_cents: int, currency: str) -> int:
    """Converts USD cents to target currency minor units.

    Latent bug (INC-103): Computes JPY with 2 decimal places instead of
    rounding to an integer minor unit, raising CurrencyPrecisionMismatchError.
    """
    code = currency.strip().upper()
    if code not in FX_RATES:
        raise ValueError(f"Unsupported currency: {currency}")
    rate = FX_RATES[code]
    if code == "JPY":
        raw_jpy = round((usd_cents / 100.0) * rate, 2)
        if not float(raw_jpy).is_integer():
            raise ValueError(
                f"HTTP 500: CurrencyPrecisionMismatchError: ISO-4217 zero-decimal currency JPY cannot have fractional minor units: {raw_jpy}"
            )
        return int(raw_jpy)
    return int(round(usd_cents * rate))
