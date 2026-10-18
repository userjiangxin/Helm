import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { PGlite } from "@electric-sql/pglite";
import { expect, it } from "vitest";
import { migrate } from "../src/migrate.js";
import { wrapPglite } from "./pg.js";

it("applies in order, is idempotent, and rejects edited migrations", async () => {
  const dir = mkdtempSync(join(tmpdir(), "mig-"));
  writeFileSync(join(dir, "0002_b.sql"), "ALTER TABLE a ADD COLUMN y int;");
  writeFileSync(join(dir, "0001_a.sql"), "CREATE TABLE a (x int); INSERT INTO a VALUES (1);");
  const sql = wrapPglite(await PGlite.create());

  expect(await migrate(sql, dir)).toEqual(["0001_a.sql", "0002_b.sql"]);
  expect(await migrate(sql, dir)).toEqual([]);
  expect(await sql.query(`SELECT x, y FROM a`)).toEqual([{ x: 1, y: null }]);

  writeFileSync(join(dir, "0001_a.sql"), "CREATE TABLE a (x bigint);");
  await expect(migrate(sql, dir)).rejects.toThrow(/edited after being applied/);
});

it("a failing migration leaves nothing half-applied", async () => {
  const dir = mkdtempSync(join(tmpdir(), "mig-"));
  writeFileSync(join(dir, "0001_bad.sql"), "CREATE TABLE ok (x int); SELECT no_such_function();");
  const sql = wrapPglite(await PGlite.create());
  await expect(migrate(sql, dir)).rejects.toThrow();
  expect(await sql.query(`SELECT to_regclass('ok') AS t`)).toEqual([{ t: null }]);
});
