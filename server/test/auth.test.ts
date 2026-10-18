import { Hono } from "hono";
import { describe, expect, it } from "vitest";
import { boardAuth } from "../src/auth.js";

const app = (cfg: Parameters<typeof boardAuth>[0]) => new Hono().use("*", boardAuth(cfg)).get("/api/x", (c) => c.text("ok"));
const basic = (p: string) => "Basic " + Buffer.from(`helm:${p}`).toString("base64");

describe("board auth", () => {
  it("open when nothing is configured", async () => {
    expect((await app({ allowedUsers: [] }).request("/api/x")).status).toBe(200);
  });
  it("admits only listed Tailscale logins", async () => {
    const a = app({ allowedUsers: ["me@example.com"] });
    expect((await a.request("/api/x", { headers: { "Tailscale-User-Login": "Me@Example.com" } })).status).toBe(200);
    expect((await a.request("/api/x", { headers: { "Tailscale-User-Login": "other@example.com" } })).status).toBe(403);
    expect((await a.request("/api/x")).status).toBe(403);
  });
  it("falls back to a password", async () => {
    const a = app({ allowedUsers: ["me@example.com"], password: "s3cret:with:colons" });
    expect((await a.request("/api/x", { headers: { Authorization: basic("s3cret:with:colons") } })).status).toBe(200);
    const bad = await a.request("/api/x", { headers: { Authorization: basic("nope") } });
    expect(bad.status).toBe(401);
    expect(bad.headers.get("WWW-Authenticate")).toMatch(/Basic/);
    expect((await a.request("/api/x", { headers: { "Tailscale-User-Login": "me@example.com" } })).status).toBe(200);
  });
});
