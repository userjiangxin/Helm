import { serve, type ServerType } from "@hono/node-server";
import { createNodeWebSocket } from "@hono/node-ws";
import { Hono } from "hono";
import { buildApp, Hub } from "./app.js";
import { sweep, type Sql } from "./store.js";

export interface StartOptions {
  sql: Sql;
  /** Subscribe to the `helm` NOTIFY channel. */
  listen: (onPayload: (payload: string) => void) => Promise<unknown>;
  port: number;
  host?: string;
  sweepMs?: number;
}

export async function startServer(o: StartOptions): Promise<{ server: ServerType; hub: Hub; port: number; close: () => void }> {
  const hub = new Hub();
  const root = new Hono();
  const { injectWebSocket, upgradeWebSocket } = createNodeWebSocket({ app: root });
  root.route("/", buildApp(o.sql, hub, upgradeWebSocket));
  await o.listen((p) => hub.onNotify(p));
  const timer = setInterval(() => {
    sweep(o.sql).then((ids) => ids.length && console.warn("orphaned runs", ids)).catch(console.error);
  }, o.sweepMs ?? 15_000);
  let server!: ServerType;
  const port = await new Promise<number>((resolve) => {
    server = serve({ fetch: root.fetch, port: o.port, hostname: o.host ?? "127.0.0.1" }, (i) => resolve(i.port));
    injectWebSocket(server);
  });
  return { server, hub, port, close: () => { clearInterval(timer); server.close(); } };
}
