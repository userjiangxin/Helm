// postgres.js adapter for the Sql interface.
import postgres from "postgres";
import type { Sql } from "./store.js";

export function connect(url = process.env.DATABASE_URL ?? "postgres://helm:helm@localhost:5432/helm") {
  const pg = postgres(url, { max: 10, onnotice: () => {} });
  const wrap = (q: postgres.Sql | postgres.TransactionSql): Sql => ({
    query: (text, params = []) => q.unsafe(text, params as never[]) as never,
    exec: async (script) => { await q.unsafe(script).simple(); },
    tx: (fn) => (q as postgres.Sql).begin((tx) => fn(wrap(tx))) as never,
  });
  return { pg, sql: wrap(pg) };
}

/**
 * Zero-install mode: DATABASE_URL=pglite:./data runs an embedded Postgres in-process.
 * Good for trying Helm on one machine; use real Postgres for anything you depend on.
 */
export async function connectAny(url = process.env.DATABASE_URL ?? "postgres://helm:helm@localhost:5432/helm"): Promise<{
  sql: Sql; listen: (cb: (p: string) => void) => Promise<unknown>; close: () => Promise<void>;
}> {
  if (url.startsWith("pglite:")) {
    const [{ PGlite }, { vector }, { pgcrypto }] = await Promise.all([
      import("@electric-sql/pglite"), import("@electric-sql/pglite-pgvector"), import("@electric-sql/pglite/contrib/pgcrypto"),
    ]);
    const db = await PGlite.create(url.slice("pglite:".length) || undefined, { extensions: { vector, pgcrypto } });
    type Q = Pick<typeof db, "query" | "exec">;
    const wrap = (q: Q): Sql => ({
      query: async (text, params) => (await q.query(text, params)).rows as never,
      exec: async (script) => { await q.exec(script); },
      tx: (fn) => db.transaction((tx) => fn(wrap(tx))),
    });
    return { sql: wrap(db), listen: (cb) => db.listen("helm", cb), close: () => db.close() };
  }
  const { pg, sql } = connect(url);
  return { sql, listen: (cb) => pg.listen("helm", cb), close: () => pg.end() };
}
