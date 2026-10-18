import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { PGlite } from "@electric-sql/pglite";
import { vector } from "@electric-sql/pglite-pgvector";
import { pgcrypto } from "@electric-sql/pglite/contrib/pgcrypto";
import type { Sql } from "../src/store.js";

const MIGRATIONS = join(import.meta.dirname, "../../db/migrations");

/** Fresh in-memory Postgres with all migrations applied. */
export async function testDb(): Promise<{ sql: Sql; db: PGlite }> {
  const db = await PGlite.create({ extensions: { vector, pgcrypto } });
  for (const f of readdirSync(MIGRATIONS).filter((f) => f.endsWith(".sql")).sort()) {
    await db.exec(readFileSync(join(MIGRATIONS, f), "utf8"));
  }
  const wrap = (q: { query: PGlite["query"] }): Sql => ({
    query: async (text, params) => (await q.query(text, params)).rows as never,
    tx: (fn) => db.transaction((tx) => fn(wrap(tx))),
  });
  return { sql: wrap(db), db };
}
