// helm-server entrypoint. Applies pending migrations, then serves.
import { migrate } from "./migrate.js";
import { connect } from "./pg.js";
import { startServer } from "./server.js";

const { pg, sql } = connect();
await migrate(sql, undefined, console.log);
const { port } = await startServer({
  sql,
  listen: (cb) => pg.listen("helm", cb),
  port: Number(process.env.PORT ?? 8080),
  // Bind to localhost and expose with `tailscale serve`, or set HOST to the tailnet IP.
  host: process.env.HOST ?? "127.0.0.1",
});
console.log(`helm-server listening on :${port}`);
