import { migrate } from "./migrate.js";
import { connect } from "./pg.js";

const { pg, sql } = connect();
const applied = await migrate(sql, undefined, console.log);
console.log(applied.length ? `${applied.length} migration(s) applied` : "database is up to date");
await pg.end();
