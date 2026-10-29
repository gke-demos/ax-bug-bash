import { register } from "node:module";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";

const TS_TYPE =
  "(?:(?:Record<[^>]+>|Promise<[^>]+>|number|string|boolean|void|any|Buffer|IncomingMessage|ServerResponse|CatalogProduct|CartItemInput|CheckoutPayload|ServiceTraceDetail|LogEntryRecord|BackendCallResult)(?:\\[\\])?)";

export async function load(url, context, nextLoad) {
  if (url.endsWith(".ts")) {
    const raw = await readFile(fileURLToPath(url), "utf8");
    const stripped = raw
      .replace(/^\s*import\s+type\s+\{[^}]*\}\s+from\s+["'][^"']+["'];?\s*$/gm, "")
      .replace(/(?:export\s+)?interface\s+\w+\s*\{[\s\S]*?\n\}/g, "")
      .replace(new RegExp(`\\b(const|let|var)\\s+(\\w+)\\s*:\\s*${TS_TYPE}(?=\\s*[=;])`, "g"), "$1 $2")
      .replace(new RegExp(`\\)\\s*:\\s*${TS_TYPE}\\s*\\{`, "g"), ") {")
      .replace(new RegExp(`(\\b\\w+)\\s*\\??:\\s*${TS_TYPE}(?=\\s*[,)=])`, "g"), "$1");
    return {
      format: "module",
      shortCircuit: true,
      source: stripped,
    };
  }
  return nextLoad(url, context);
}

register(import.meta.url);
