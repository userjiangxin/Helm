// helm-server entrypoint: Postgres via postgres.js, LISTEN/NOTIFY fan-out.
import postgres from "postgres";
import { startServer } from "./server.js";
import type { Sql } from "./store.js";

const pg = postgres(process.env.DATABASE_URL ?? "postgres://helm:helm@localhost:5432/helm", { max: 10 });
const wrap = (q: postgres.Sql | postgres.TransactionSql): Sql => ({
  query: (text, params = []) => q.unsafe(text, params as never[]) as never,
  tx: (fn) => (q as postgres.Sql).begin((tx) => fn(wrap(tx))) as never,
});

const { port } = await startServer({
  sql: wrap(pg),
  listen: (cb) => pg.listen("helm", cb),
  port: Number(process.env.PORT ?? 8080),
  // Bind to localhost and expose with `tailscale serve`, or set HOST to the tailnet IP.
  host: process.env.HOST ?? "127.0.0.1",
});
console.log(`helm-server listening on :${port}`);
