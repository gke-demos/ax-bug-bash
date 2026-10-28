import unittest
from pricing import apply_stacked_promos, convert_currency_minor_units


class TestPricingService(unittest.TestCase):
    def test_single_promo_codes_on_standard_cart(self):
        self.assertEqual(apply_stacked_promos(5000, ["SAVE20"]), 4000)
        self.assertEqual(apply_stacked_promos(4000, ["FLAT15"]), 2500)

    def test_currency_conversion_usd_and_eur(self):
        self.assertEqual(convert_currency_minor_units(2900, "USD"), 2900)
        self.assertEqual(convert_currency_minor_units(2900, "EUR"), 2668)


if __name__ == "__main__":
    unittest.main()
