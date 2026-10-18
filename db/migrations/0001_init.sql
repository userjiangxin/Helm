-- Helm v1 · M0 schema. Postgres 17 + pgvector.
-- Postgres is the single source of truth; daemons and the brain never keep authoritative state elsewhere.
-- memory_item / skill / lesson tables arrive in M3 (0003_memory.sql).

CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS vector;

-- ---------- machines ----------
CREATE TABLE machine (
  id             text PRIMARY KEY,                       -- 'mch_' + ulid
  name           text NOT NULL UNIQUE,
  labels         text[] NOT NULL DEFAULT '{}',
  capabilities   jsonb NOT NULL DEFAULT '{}',            -- wire.Capabilities
  token_hash     text NOT NULL,                          -- sha256 of machine token
  status         text NOT NULL DEFAULT 'offline' CHECK (status IN ('online','offline','draining')),
  last_seen_at   timestamptz,
  last_heartbeat jsonb,                                  -- wire.Heartbeat
  created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE registration_code (
  code_hash   text PRIMARY KEY,
  expires_at  timestamptz NOT NULL,
  used_at     timestamptz,
  machine_id  text REFERENCES machine(id)
);

-- ---------- goals & tasks (the board) ----------
CREATE TABLE goal (
  id          text PRIMARY KEY,                          -- 'gol_' + ulid
  title       text NOT NULL,
  intent      text NOT NULL DEFAULT '',
  status      text NOT NULL DEFAULT 'active' CHECK (status IN ('active','done','archived')),
  autonomy    text NOT NULL DEFAULT 'L0' CHECK (autonomy IN ('L0','L1','L2')),
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE task (
  id          text PRIMARY KEY,                          -- 'tsk_' + ulid
  goal_id     text NOT NULL REFERENCES goal(id) ON DELETE CASCADE,
  parent_id   text REFERENCES task(id) ON DELETE CASCADE,
  title       text NOT NULL,
  -- board columns
  status      text NOT NULL DEFAULT 'inbox'
              CHECK (status IN ('inbox','planning','awaiting_approval','running','verifying','done','blocked','cancelled')),
  position    double precision NOT NULL DEFAULT 0,       -- order within column (fractional indexing)
  assignee    text NOT NULL DEFAULT 'brain' CHECK (assignee IN ('brain','human')),
  target_machine_id text REFERENCES machine(id),         -- null = any machine matching labels
  target_labels text[] NOT NULL DEFAULT '{}',
  executor    text NOT NULL DEFAULT 'codex' CHECK (executor IN ('codex','claude','shell','job')),
  current_contract_version int,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX task_board_idx ON task (goal_id, status, position);

-- ---------- contracts (versioned; never updated in place) ----------
CREATE TABLE contract (
  task_id     text NOT NULL REFERENCES task(id) ON DELETE CASCADE,
  version     int  NOT NULL,
  body        jsonb NOT NULL,                            -- wire.Contract
  author      text NOT NULL CHECK (author IN ('brain','human')),
  reason      text NOT NULL DEFAULT '',
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (task_id, version)
);

-- ---------- runs ----------
CREATE TABLE run (
  id               text PRIMARY KEY,                     -- 'run_' + ulid
  task_id          text NOT NULL REFERENCES task(id) ON DELETE CASCADE,
  contract_version int  NOT NULL,
  machine_id       text REFERENCES machine(id),          -- set on claim
  executor         text NOT NULL,
  state            text NOT NULL DEFAULT 'PENDING'
                   CHECK (state IN ('PENDING','PREPARING','RUNNING','GATED','SUSPENDED','VERIFYING',
                                    'SUCCEEDED','FAILED','ABORTED','ORPHANED')),
  epoch            bigint NOT NULL DEFAULT 0,            -- bumped on every (re)claim; fencing token
  lease_until      timestamptz,
  session_ref      text,                                 -- agent-native session id for resume
  last_seq         bigint NOT NULL DEFAULT 0,            -- highest event seq persisted
  state_reason     text NOT NULL DEFAULT '',
  created_at       timestamptz NOT NULL DEFAULT now(),
  started_at       timestamptz,
  ended_at         timestamptz,
  FOREIGN KEY (task_id, contract_version) REFERENCES contract(task_id, version)
);
CREATE INDEX run_claimable_idx ON run (created_at) WHERE state = 'PENDING';
CREATE INDEX run_machine_idx ON run (machine_id) WHERE state NOT IN ('SUCCEEDED','FAILED','ABORTED');

-- L1 events. Idempotent insert on (run_id, seq) makes resends harmless.
-- TODO(M1): convert to monthly partitions once volume warrants it.
CREATE TABLE run_event (
  run_id  text   NOT NULL REFERENCES run(id) ON DELETE CASCADE,
  seq     bigint NOT NULL,
  ts      timestamptz NOT NULL,
  type    text   NOT NULL,
  data    jsonb  NOT NULL,
  PRIMARY KEY (run_id, seq)
);

-- L2 digests (M1). narrative is an LLM claim, fields are rule-computed.
CREATE TABLE digest (
  run_id     text NOT NULL REFERENCES run(id) ON DELETE CASCADE,
  window_end bigint NOT NULL,                            -- last seq covered
  fields     jsonb NOT NULL,
  narrative  text,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (run_id, window_end)
);

CREATE TABLE evidence (
  run_id  text NOT NULL REFERENCES run(id) ON DELETE CASCADE,
  uri     text NOT NULL,                                 -- ev://<machine>/<run>/<name>, bytes stay on the machine
  kind    text NOT NULL,
  bytes   bigint NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (run_id, uri)
);

-- ---------- control: directives & approvals ----------
CREATE TABLE directive (
  id               text PRIMARY KEY,                     -- 'dir_' + ulid (sortable → used as pull cursor)
  run_id           text NOT NULL REFERENCES run(id) ON DELETE CASCADE,
  machine_id       text NOT NULL REFERENCES machine(id),
  kind             text NOT NULL CHECK (kind IN ('CONTINUE','STEER','AMEND','PAUSE','ROLLBACK','ABORT')),
  contract_version int  NOT NULL,
  args             jsonb NOT NULL DEFAULT '{}',
  issued_by        text NOT NULL CHECK (issued_by IN ('brain','human','jev')),
  status           text NOT NULL DEFAULT 'queued'
                   CHECK (status IN ('queued','applied','rejected_stale','rejected_invalid','failed')),
  ack_detail       text,
  created_at       timestamptz NOT NULL DEFAULT now(),
  acked_at         timestamptz
);
CREATE INDEX directive_pull_idx ON directive (machine_id, id) WHERE status = 'queued';

CREATE TABLE approval (
  id          text PRIMARY KEY,                          -- 'apr_' + ulid
  run_id      text REFERENCES run(id) ON DELETE CASCADE,
  task_id     text REFERENCES task(id) ON DELETE CASCADE,
  kind        text NOT NULL CHECK (kind IN ('gate','contract','question')),
  question    text NOT NULL,
  context     jsonb NOT NULL DEFAULT '{}',               -- tool call, digest excerpt, brain recommendation
  options     jsonb NOT NULL DEFAULT '[]',
  decision    jsonb,
  decided_by  text CHECK (decided_by IN ('human','brain')),
  created_at  timestamptz NOT NULL DEFAULT now(),
  decided_at  timestamptz
);
CREATE INDEX approval_open_idx ON approval (created_at) WHERE decided_at IS NULL;

-- ---------- brain audit (M2) ----------
CREATE TABLE brain_wake (
  id          text PRIMARY KEY,
  goal_id     text REFERENCES goal(id) ON DELETE CASCADE,
  graph       text NOT NULL CHECK (graph IN ('plan','supervise','verify','reflect')),
  reason      text NOT NULL,
  inputs      jsonb NOT NULL,
  decisions   jsonb,
  model       text,
  tokens_in   int,
  tokens_out  int,
  created_at  timestamptz NOT NULL DEFAULT now()
);

-- Board & daemons get changes via LISTEN/NOTIFY; the server fans out to SSE / WebSocket.
CREATE OR REPLACE FUNCTION helm_notify() RETURNS trigger AS $$
BEGIN
  PERFORM pg_notify('helm', json_build_object(
    'table', TG_TABLE_NAME, 'op', TG_OP,
    'id', COALESCE(to_jsonb(NEW)->>'id', to_jsonb(NEW)->>'run_id'),
    'machine_id', to_jsonb(NEW)->>'machine_id')::text);
  RETURN NEW;
END $$ LANGUAGE plpgsql;

CREATE TRIGGER run_notify       AFTER INSERT OR UPDATE ON run       FOR EACH ROW EXECUTE FUNCTION helm_notify();
CREATE TRIGGER task_notify      AFTER INSERT OR UPDATE ON task      FOR EACH ROW EXECUTE FUNCTION helm_notify();
CREATE TRIGGER directive_notify AFTER INSERT ON directive           FOR EACH ROW EXECUTE FUNCTION helm_notify();
CREATE TRIGGER approval_notify  AFTER INSERT OR UPDATE ON approval  FOR EACH ROW EXECUTE FUNCTION helm_notify();
