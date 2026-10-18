import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import * as P from "../src/protocol.js";

const fx = (name: string) => JSON.parse(readFileSync(join(import.meta.dirname, "../../protocol/fixtures", name), "utf8"));

describe("protocol fixtures (shared with Go)", () => {
  it.each([
    ["run_assignment.json", P.RunAssignment],
    ["directive.json", P.Directive],
    ["directive_ack.json", P.DirectiveAck],
    ["capabilities.json", P.Capabilities],
  ] as const)("%s parses strictly and round-trips", (name, schema) => {
    const raw = fx(name);
    expect(schema.parse(raw)).toEqual(raw);
  });

  it.each([
    ["ws_wake.json", P.Wake],
    ["ws_heartbeat.json", P.Heartbeat],
    ["ws_run_events.json", P.RunEvents],
    ["ws_events_ack.json", P.EventsAck],
  ] as const)("%s envelope + payload", (name, payload) => {
    const env = P.Envelope.parse(fx(name));
    expect(payload.parse(env.payload)).toEqual(env.payload);
  });
});
