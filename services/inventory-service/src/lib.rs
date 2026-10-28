/// High-concurrency SKU stock reservation and warehouse hazmat ledger.

/// Reserves SKU units for standard and flash-sale bundle orders.
/// Latent bug (INC-102): Subtracts `requested + bonus_bundle_units` directly from `available`
/// without capping `bonus_bundle_units` at remaining stock (`available - requested`),
/// causing a `u32` arithmetic underflow panic when `available == 2`, `requested == 2`, `bonus == 2`.
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

/// Classifies warehouse handling tier for a SKU based on weight and hazmat class.
/// Latent bug (INC-104): Only maps `NONE` and small `ORM-D` parcels, returning an error
/// on `UN3480-LITHIUM` heavy battery shipments (> 1000g).
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
}
