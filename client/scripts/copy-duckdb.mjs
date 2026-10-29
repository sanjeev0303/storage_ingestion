// Copies DuckDB's WebAssembly engine and its workers next to the console's
// other static files, so the SQL panel loads them from this origin and
// never from a CDN. Run before dev and build.
import { copyFileSync, mkdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const from = join(root, "node_modules", "@duckdb", "duckdb-wasm", "dist");
const to = join(root, "public", "duckdb");
mkdirSync(to, { recursive: true });
for (const file of ["duckdb-mvp.wasm", "duckdb-eh.wasm", "duckdb-browser-mvp.worker.js", "duckdb-browser-eh.worker.js"]) {
  copyFileSync(join(from, file), join(to, file));
}
