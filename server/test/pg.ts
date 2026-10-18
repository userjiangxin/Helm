import { PGlite } from "@electric-sql/pglite";
import { pgcrypto } from "@electric-sql/pglite/contrib/pgcrypto";
import { vector } from "@electric-sql/pglite-pgvector";
import { migrate } from "../src/migrate.js";
import type { Sql } from "../src/store.js";

type Q = Pick<PGlite, "query" | "exec">;

export function wrapPglite(db: PGlite): Sql {
  const wrap = (q: Q): Sql => ({
    query: async (text, params) => (await q.query(text, params)).rows as never,
    exec: async (script) => { await q.exec(script); },
    tx: (fn) => db.transaction((tx) => fn(wrap(tx))),
  });
  return wrap(db);
}

/** Fresh in-memory Postgres with all migrations applied through the real migrator. */
export async function testDb(): Promise<{ sql: Sql; db: PGlite }> {
  const db = await PGlite.create({ extensions: { vector, pgcrypto } });
  const sql = wrapPglite(db);
  await migrate(sql);
  return { sql, db };
}
