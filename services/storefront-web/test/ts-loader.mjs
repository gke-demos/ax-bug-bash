import { register } from "node:module";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";

export async function load(url, context, nextLoad) {
  if (url.endsWith(".ts")) {
    const raw = await readFile(fileURLToPath(url), "utf8");
    const stripped = raw
      .replace(/export\s+interface\s+\w+\s*\{[\s\S]*?\}/g, "")
      .replace(/:\s*CatalogProduct\[\]/g, "")
      .replace(/:\s*Record<[^>]+>/g, "")
      .replace(/(\w+)\s*:\s*(number|string|boolean)(\[\])?/g, "$1")
      .replace(/\)\s*:\s*(number|string|boolean|void)\s*\{/g, ") {");
    return {
      format: "module",
      shortCircuit: true,
      source: stripped,
    };
  }
  return nextLoad(url, context);
}

register(import.meta.url);
