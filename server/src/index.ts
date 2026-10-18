// helm-server entrypoint. Applies pending migrations, then serves API + board.
//   DATABASE_URL=postgres://…        production
//   DATABASE_URL=pglite:./helm-data   zero-install single-machine mode
import { migrate } from "./migrate.js";
import { connectAny } from "./pg.js";
import { startServer } from "./server.js";

const host = process.env.HOST ?? "127.0.0.1";
if (!process.env.HELM_ALLOWED_USERS && !process.env.HELM_BOARD_PASSWORD && !["127.0.0.1", "localhost", "::1"].includes(host)) {
  console.error("refusing to start: the board would be open on " + host + ". Set HELM_ALLOWED_USERS or HELM_BOARD_PASSWORD.");
  process.exit(1);
}
const db = await connectAny();
await migrate(db.sql, undefined, console.log);
const { port } = await startServer({
  sql: db.sql,
  listen: db.listen,
  port: Number(process.env.PORT ?? 8080),
  // Bind to localhost and expose with `tailscale serve`, or set HOST to the tailnet IP.
  host,
});
console.log(`helm-server listening on :${port}`);
