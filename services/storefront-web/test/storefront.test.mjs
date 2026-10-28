import assert from "node:assert/strict";
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

let failed = 0;
for (const t of tests) {
  try {
    t.fn();
    console.log(`PASS - ${t.name}`);
  } catch (err) {
    failed++;
    console.error(`FAIL - ${t.name}: ${err.message}`);
  }
}
if (failed > 0) {
  process.exit(1);
}
console.log(`PASS (${tests.length} tests)`);
