-- COMPLETE: human (later: Verify graph) accepts a VERIFYING run → SUCCEEDED.
ALTER TABLE directive DROP CONSTRAINT directive_kind_check;
ALTER TABLE directive ADD CONSTRAINT directive_kind_check
  CHECK (kind IN ('CONTINUE','STEER','AMEND','PAUSE','ROLLBACK','ABORT','COMPLETE'));
