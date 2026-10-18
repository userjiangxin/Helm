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
