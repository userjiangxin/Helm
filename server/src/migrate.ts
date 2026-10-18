// Applies db/migrations/*.sql in filename order, each in its own transaction, recording
// applied files in schema_migrations. Safe to run repeatedly; refuses to run if an
// already-applied file was edited (checksum mismatch).
import { createHash } from "node:crypto";
import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import type { Sql } from "./store.js";

export const MIGRATIONS_DIR = join(import.meta.dirname, "../../db/migrations");

export async function migrate(sql: Sql, dir = MIGRATIONS_DIR, log: (m: string) => void = () => {}): Promise<string[]> {
  await sql.query(`CREATE TABLE IF NOT EXISTS schema_migrations (
    name text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`);
  const done = new Map(
    (await sql.query<{ name: string; checksum: string }>(`SELECT name, checksum FROM schema_migrations`)).map((r) => [r.name, r.checksum]),
  );
  const applied: string[] = [];
  for (const name of readdirSync(dir).filter((f) => f.endsWith(".sql")).sort()) {
    const body = readFileSync(join(dir, name), "utf8");
    const sum = createHash("sha256").update(body).digest("hex");
    const prev = done.get(name);
    if (prev) {
      if (prev !== sum) throw new Error(`migration ${name} was edited after being applied; add a new migration instead`);
      continue;
    }
    await sql.tx(async (tx) => {
      await tx.exec(body);
      await tx.query(`INSERT INTO schema_migrations (name, checksum) VALUES ($1, $2)`, [name, sum]);
    });
    log(`applied ${name}`);
    applied.push(name);
  }
  return applied;
}
