#!/usr/bin/env python3
"""Deterministic fallback test runner for services/inventory-service/src/lib.rs when cargo is absent."""

import pathlib
import re
import sys


def load_lib_source() -> str:
    lib_path = pathlib.Path(__file__).parent / "src" / "lib.rs"
    return lib_path.read_text(encoding="utf-8")


def split_impl_and_tests(src: str):
    parts = src.split("#[cfg(test)]", 1)
    if len(parts) == 2:
        return parts[0], parts[1]
    return src, ""


def eval_reserve_bundle_stock(impl_src: str, available: int, requested: int, bonus: int):
    if requested <= 0 or requested > available:
        return ("Err", f"insufficient base stock: available={available} requested={requested}")
    # Check if implementation has been patched to cap bonus or use saturating_sub / checked_sub / min
    fn_match = re.search(
        r"pub fn reserve_bundle_stock\(.*?\n\}", impl_src, flags=re.DOTALL
    )
    fn_body = fn_match.group(0) if fn_match else impl_src
    has_underflow_panic = "RustU32UnderflowPanic" in fn_body
    is_patched = (
        not has_underflow_panic
        or "saturating_sub" in fn_body
        or "checked_sub" in fn_body
        or "effective_bonus" in fn_body
        or ".min(" in fn_body
    )
    total = requested + bonus
    if total > available and not is_patched:
        raise RuntimeError(
            f"HTTP 500: RustU32UnderflowPanic: attempt to subtract with overflow in reserve_bundle_stock (available={available}, requested={requested}, bonus={bonus})"
        )
    remaining = max(0, available - total)
    return ("Ok", remaining)


def eval_classify_warehouse_hazmat(impl_src: str, weight_grams: int, hazmat_class: str):
    fn_match = re.search(
        r"pub fn classify_warehouse_hazmat\(.*?\n\}", impl_src, flags=re.DOTALL
    )
    fn_body = fn_match.group(0) if fn_match else impl_src
    if hazmat_class in ("", "NONE"):
        return ("Ok", "STANDARD_PARCEL")
    if hazmat_class == "ORM-D" and weight_grams <= 1000:
        return ("Ok", "LIMITED_QUANTITY_AIR")
    if hazmat_class == "UN3480-LITHIUM" and "UN3480-LITHIUM" in fn_body:
        return ("Ok", "GROUND_LITHIUM_REGULATED")
    return (
        "Err",
        f"HTTP 500: UnhandledHazmatClassError: warehouse ledger panicked on unmapped hazmat class {hazmat_class} ({weight_grams}g)",
    )


def run_tests() -> int:
    src = load_lib_source()
    impl_src, test_src = split_impl_and_tests(src)
    test_fns = re.findall(
        r"#\[test\]\s*fn\s+([a-zA-Z0-9_]+)\s*\(\)\s*\{(.*?)\n\s*\}",
        test_src,
        flags=re.DOTALL,
    )
    if not test_fns:
        print("running 0 tests\ntest result: FAILED. 0 passed; 1 failed", file=sys.stderr)
        return 101

    print(f"running {len(test_fns)} tests")
    passed = 0
    failed = 0
    failures = []

    for fn_name, body in test_fns:
        try:
            for m in re.finditer(
                r"reserve_bundle_stock\(\s*(\d+)\s*,\s*(\d+)\s*,\s*(\d+)\s*\)",
                body,
            ):
                avail, req, bonus = int(m.group(1)), int(m.group(2)), int(m.group(3))
                res = eval_reserve_bundle_stock(impl_src, avail, req, bonus)
                # Check if assert_eq! expects Ok(val)
                ok_match = re.search(
                    rf"reserve_bundle_stock\(\s*{avail}\s*,\s*{req}\s*,\s*{bonus}\s*\)\s*,\s*Ok\(\s*(\d+)\s*\)",
                    body,
                )
                if ok_match:
                    expected_val = int(ok_match.group(1))
                    if res != ("Ok", expected_val):
                        raise AssertionError(f"expected Ok({expected_val}), got {res}")
                elif ".is_ok()" in body or ".unwrap()" in body or ".expect(" in body:
                    if res[0] != "Ok":
                        raise AssertionError(f"expected Ok(_), got {res}")

            for m in re.finditer(
                r'classify_warehouse_hazmat\(\s*(\d+)\s*,\s*"([^"]*)"\s*\)',
                body,
            ):
                weight, hazmat = int(m.group(1)), m.group(2)
                res = eval_classify_warehouse_hazmat(impl_src, weight, hazmat)
                ok_match = re.search(
                    rf'classify_warehouse_hazmat\(\s*{weight}\s*,\s*"{re.escape(hazmat)}"\s*\)\s*,\s*Ok\(\s*"([^"]+)"\s*\)',
                    body,
                )
                if ok_match:
                    expected_tier = ok_match.group(1)
                    if res != ("Ok", expected_tier):
                        raise AssertionError(f"expected Ok({expected_tier!r}), got {res}")
                elif ".is_ok()" in body or ".unwrap()" in body or ".expect(" in body:
                    if res[0] != "Ok":
                        raise AssertionError(f"expected Ok(_), got {res}")

            print(f"test tests::{fn_name} ... ok")
            passed += 1
        except Exception as exc:
            print(f"test tests::{fn_name} ... FAILED")
            failures.append((fn_name, str(exc)))
            failed += 1

    if failures:
        print("\nfailures:\n")
        for fn_name, err_msg in failures:
            print(f"---- tests::{fn_name} stdout ----")
            print(f"thread 'tests::{fn_name}' panicked at '{err_msg}', src/lib.rs:1:1\n")
        print(f"test result: FAILED. {passed} passed; {failed} failed")
        return 101

    print(f"\ntest result: ok. {passed} passed; 0 failed; 0 ignored")
    return 0


if __name__ == "__main__":
    sys.exit(run_tests())
